package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/fileimport"
)

func sandboxFilesCommand() *cli.Command {
	var id, owner, prefix, checkpoint string
	var batchMiB int64
	var resume bool
	return &cli.Command{Name: "files", Summary: "Import file bodies into a service", Usage: "veris sandbox files import NAME DIRECTORY --owner ID --prefix PATH [--resume]", Sub: []*cli.Command{{
		Name: "import", Summary: "Upload a directory in bounded, checkpointed batches", Usage: "veris sandbox files import NAME DIRECTORY --owner ID --prefix PATH [--batch-mib 128] [--checkpoint FILE] [--resume] [--id ID] [--json]",
		Help: "Uploads file bodies through /veris/files in merge mode. Existing paths keep their IDs and\nmay gain revisions. Each batch is atomic; the whole directory is not. --resume skips only\nacknowledged batches for the same files and target. An uncertain POST stops for reconciliation.\nFor gs:// sources, first download locally with gcloud storage rsync. Capture the sandbox\nafter importing, then verify a fresh boot before deleting the source.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "sandbox id (default: this folder's)")
			fs.StringVar(&owner, "owner", "", "destination identity from the service manual")
			fs.StringVar(&prefix, "prefix", "", "destination folder/subtree")
			fs.StringVar(&checkpoint, "checkpoint", "", "checkpoint JSON (default: .veris/file-import-SANDBOX-SERVICE.json)")
			fs.Int64Var(&batchMiB, "batch-mib", 128, "maximum ZIP batch payload in MiB (1..1024; larger single files stream raw)")
			fs.BoolVar(&resume, "resume", false, "resume the exact checkpoint, skipping acknowledged batches")
		},
		Run: func(ctx *cli.Context, args []string) error {
			if len(args) != 2 {
				return fmt.Errorf("files import needs a service NAME and source DIRECTORY")
			}
			if batchMiB < 1 || batchMiB > 1024 {
				return fmt.Errorf("--batch-mib must be 1..1024")
			}
			s, sb, tw, err := dataTwin(ctx, id, args[0], "import files")
			if err != nil {
				return err
			}
			// Older deployments advertised HTTP even through an HTTPS control plane.
			advertised, _ := url.Parse(tw.ControlURL)
			plane, _ := url.Parse(s.plane().Base)
			if advertised != nil && plane != nil && advertised.Host == plane.Host && plane.Scheme == "https" {
				advertised.Scheme = "https"
				tw.ControlURL = advertised.String()
			}
			if checkpoint == "" {
				checkpoint = filepath.Join(".veris", "file-import-"+sb.ID+"-"+args[0]+".json")
			}
			bg, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			tw.HTTP.Timeout = 10 * time.Minute
			receipt, err := fileimport.Run(bg, tw.HTTP, fileimport.Options{Source: args[1], ControlURL: tw.ControlURL, Owner: owner, Prefix: prefix, BatchBytes: batchMiB << 20, Checkpoint: checkpoint, Resume: resume}, func(r *fileimport.Receipt) {
				s.ui.Detail("Imported %d/%d files (%d bytes); checkpoint %s", r.Completed, len(r.Files), r.Bytes, checkpoint)
			})
			if err != nil {
				return s.fail("import", "files", err)
			}
			if ctx.Globals.JSON {
				return printJSON(ctx.Stdout, receipt)
			}
			s.ui.Success("Imported %d files (%d bytes)", receipt.Completed, receipt.Bytes)
			s.ui.Detail("Checkpoint: %s. Promote or snapshot, then verify a fresh boot before deleting the source.", checkpoint)
			return nil
		},
	}}}
}
