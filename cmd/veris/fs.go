package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/direct"
	"github.com/veris-ai/veris-cli/internal/procgroup"
	"github.com/veris-ai/veris-cli/internal/twin"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// The sandbox's filesystem is one member among the twins: it holds the
// folders `up --fs` gave the sandbox and serves them two ways. Its URL is a
// WebDAV root (one directory per folder) that any WebDAV client, rclone or
// Finder can open with credentials minted for the purpose; its control URL
// serves /veris/fs/* -- the token mint, the layout, a diff of each folder
// against the baseline it was pulled as, and a tar export. The verbs here
// are those routes with a terminal in front: connect prints the
// credentials as exports, diff and pull read, push writes, mount and
// attach put the folders on this machine.

// Defaults of the fs verbs.
const (
	// fsConnectTTL is how long a connect's credentials live. The engine's
	// own default is an hour too; sending it keeps the two from drifting.
	fsConnectTTL = time.Hour
	// fsMountTTL is a mount's. A mount outlives a shell session and has
	// no refresh path in this version, so it takes most of what the engine
	// allows; the expiry is said aloud.
	fsMountTTL = 12 * time.Hour
	// fsMaxTTL is the engine's ceiling, refused here first with the number.
	fsMaxTTL = 24 * time.Hour
)

// fsTransferTimeout bounds one PUT, and one export from its request to
// the last byte unpacked. A variable so a test can shorten it.
var fsTransferTimeout = 10 * time.Minute

// fsDiffTimeout bounds one diff, request through the last byte read. The
// twin client's raw path carries no client timeout (an export would be cut
// mid-tar), so the deadline rides the context. A variable so a test can
// shorten it.
var fsDiffTimeout = 2 * time.Minute

// The engine reports a folder ready once its pull has finished; until then
// the layout says ready:false and the directory is whatever has landed so
// far. A verb that read or wrote it then would export a partial tree, race
// the engine's own clone, or diff against a baseline not yet recorded, so
// the verbs wait, re-reading the layout every fsReadyPoll for at most
// fsReadyTimeout. Variables so a test can shorten them.
var (
	fsReadyTimeout = 5 * time.Minute
	fsReadyPoll    = 2 * time.Second
)

// Variables connect exports, and attach exports for a locally run agent.
const (
	envFSURL      = "VERIS_FS_URL"
	envFSUser     = "VERIS_FS_USER"
	envFSPassword = "VERIS_FS_PASSWORD"
	envWorkspace  = "WORKSPACE_DIR"
	envFolders    = "WORKSPACE_FOLDERS"
)

// lookPath finds rclone; a test replaces it to stand in for a machine
// without one.
var lookPath = exec.LookPath

// rcloneBinary is what mount and attach --mount shell out to.
const rcloneBinary = "rclone"

func sandboxFSCommand() *cli.Command {
	var connectID, diffID, pullID, pushID, mountID, attachID string
	var connectTTL, mountTTL, attachTTL string
	var diffFormat, pullDest, pushSrc string
	var pullForce, attachForce, mountDaemon bool
	var attachMounts []string
	return &cli.Command{
		Name:    "fs",
		Summary: "The sandbox's filesystem: connect, diff, pull, push, mount, attach",
		Usage:   "veris sandbox fs <command> [--id ID] [flags]",
		Help: "A sandbox started with `up --fs` holds folders: one directory per source, served as a WebDAV\n" +
			"root (the `filesystem` service's URL) that these verbs reach with credentials minted for the\n" +
			"purpose, and inspected through its control URL with your Veris key. connect prints the URL and\n" +
			"a fresh credential as exports; diff shows what changed in a folder since it was pulled; pull\n" +
			"and push copy a folder down and up; mount puts one, or all, on a mountpoint through rclone;\n" +
			"attach lays every folder out as the agent's pod sees it and prints WORKSPACE_DIR and\n" +
			"WORKSPACE_FOLDERS for an agent run here. Each verb acts on this folder's sandbox unless --id\n" +
			"names another. A sandbox without folders, or a control plane without these routes, says so.",
		Sub: []*cli.Command{
			{
				Name:    "connect",
				Summary: "Mint WebDAV credentials and print them as exports",
				Usage:   "veris sandbox fs connect [--ttl 1h] [--id ID] [--json]",
				Help: "Prints export lines for " + envFSURL + ", " + envFSUser + " and " + envFSPassword + " to STDOUT and nothing\n" +
					"else there, so eval \"$(veris sandbox fs connect)\" sets them; the credential lives --ttl (default\n" +
					"1h, at most 24h). The URL is a WebDAV root with one directory per folder: open it in Finder\n" +
					"(Go ▸ Connect to Server), Explorer, or rclone. --json prints {url, user, password, expires_at, folders}.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&connectID, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&connectTTL, "ttl", "", "how long the credential lives, e.g. 30m or 2h (default 1h, max 24h)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					ttl, err := parseFSTTL(connectTTL, fsConnectTTL)
					if err != nil {
						return err
					}
					return fsConnect(ctx, connectID, ttl)
				},
			},
			{
				Name:    "diff",
				Summary: "What changed in a folder since the sandbox pulled it",
				Usage:   "veris sandbox fs diff [FOLDER] [--format status|patch] [--id ID] [--json]",
				Help: "status (the default) lists each changed file with A, M, D or R and its line counts; patch\n" +
					"prints a unified diff against the baseline the folder was pulled as (binary files as a note),\n" +
					"capped by the engine at 8 MiB and marked when cut. --json prints status as the engine sent it.\n" +
					"FOLDER is one of `veris sandbox fs connect`'s folders; left out, a sandbox with one folder\n" +
					"uses it, a terminal is asked which, and anything else is told the names the sandbox has.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&diffID, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&diffFormat, "format", twin.FSDiffStatus, "status or patch")
				},
				Run: func(ctx *cli.Context, args []string) error {
					name, err := atMostOneFolderName(ctx, args)
					if err != nil {
						return err
					}
					if diffFormat != twin.FSDiffStatus && diffFormat != twin.FSDiffPatch {
						return &cli.UsageError{Msg: fmt.Sprintf("--format must be status or patch (got %q)", diffFormat)}
					}
					if diffFormat == twin.FSDiffPatch && ctx.Globals != nil && ctx.Globals.JSON {
						return &cli.UsageError{Msg: "--json prints the status document; drop --format patch or --json"}
					}
					return fsDiff(ctx, diffID, name, diffFormat)
				},
			},
			{
				Name:    "pull",
				Summary: "Copy a folder from the sandbox into a local directory",
				Usage:   "veris sandbox fs pull [FOLDER] [--dest DIR] [--force] [--id ID]",
				Help: "Exports the folder as the engine holds it now, its .git included when it has one, into DIR\n" +
					"(default ./FOLDER). A DIR that already has files in it is refused unless --force, which writes\n" +
					"over them and leaves what it does not touch. Entries that would land outside DIR are refused.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&pullID, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&pullDest, "dest", "", "local `directory` to pull into (default ./FOLDER)")
					fs.BoolVar(&pullForce, "force", false, "pull into a directory that is not empty")
				},
				Run: func(ctx *cli.Context, args []string) error {
					name, err := atMostOneFolderName(ctx, args)
					if err != nil {
						return err
					}
					return fsPull(ctx, pullID, name, pullDest, pullForce)
				},
			},
			{
				Name:    "push",
				Summary: "Copy a local directory into a folder of the sandbox",
				Usage:   "veris sandbox fs push [FOLDER] [--src DIR] [--id ID]",
				Help: "Uploads every file under DIR (default ./FOLDER) into the folder over WebDAV, writing over\n" +
					"what is there and deleting nothing. A top-level .git, .veris/twin.local.yaml and .veris/*.lock\n" +
					"stay behind: the engine keeps the folder's own repository and baseline. Symlinks are sent in\n" +
					"rclone's .rclonelink form, which the engine turns back into links.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&pushID, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&pushSrc, "src", "", "local `directory` to push (default ./FOLDER)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					name, err := atMostOneFolderName(ctx, args)
					if err != nil {
						return err
					}
					return fsPush(ctx, pushID, name, pushSrc)
				},
			},
			{
				Name:    "mount",
				Summary: "Mount the filesystem, or one folder, through rclone",
				Usage:   "veris sandbox fs mount [FOLDER] MOUNTPOINT [--ttl 12h] [--daemon] [--id ID]",
				Help: "Needs rclone on PATH (brew install rclone, or rclone.org/install). On macOS it is `rclone\n" +
					"nfsmount`, which needs no kernel extension; on Linux `rclone mount` (FUSE). Windows has no\n" +
					"mount here; pull and push do. Without FOLDER every folder appears under MOUNTPOINT; with one,\n" +
					"that folder is MOUNTPOINT. The mount holds until Ctrl-C, or --daemon leaves it mounted and\n" +
					"returns; either way its credential lives --ttl (default 12h, max 24h), after which it must be\n" +
					"mounted again. Unmount with umount (macOS) or fusermount -u (Linux).",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&mountID, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&mountTTL, "ttl", "", "how long the mount's credential lives (default 12h, max 24h)")
					fs.BoolVar(&mountDaemon, "daemon", false, "leave the mount up and return")
				},
				Run: func(ctx *cli.Context, args []string) error {
					var folder, mountpoint string
					switch len(args) {
					case 1:
						mountpoint = args[0]
					case 2:
						folder, mountpoint = args[0], args[1]
					default:
						return &cli.UsageError{Msg: "fs mount takes [FOLDER] MOUNTPOINT"}
					}
					ttl, err := parseFSTTL(mountTTL, fsMountTTL)
					if err != nil {
						return err
					}
					return fsMount(ctx, mountID, folder, mountpoint, ttl, mountDaemon)
				},
			},
			{
				Name:    "attach",
				Summary: "Lay the folders out locally as the agent's pod sees them",
				Usage:   "veris sandbox fs attach [DIR] [--mount FOLDER]... [--ttl 12h] [--force] [--id ID]",
				Help: "Pulls every folder into DIR (default ~/.veris/fs/<sandbox>) the way the pod lays /workspace out:\n" +
					"the root folder is DIR itself, every other folder DIR/<name>. A folder named by --mount is\n" +
					"mounted there through rclone instead of copied (see fs mount; not on Windows). Then it prints\n" +
					"export lines for " + envWorkspace + " and " + envFolders + " to STDOUT, so an agent run here with\n" +
					"eval \"$(veris sandbox fs attach)\" sees what it would see in the pod. Copies are one-way: push\n" +
					"sends changes back.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&attachID, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&attachTTL, "ttl", "", "how long a --mount's credential lives (default 12h, max 24h)")
					fs.BoolVar(&attachForce, "force", false, "pull into directories that are not empty")
					fs.Func("mount", "mount this `folder` rather than copy it; repeatable", func(v string) error {
						attachMounts = append(attachMounts, v)
						return nil
					})
				},
				Run: func(ctx *cli.Context, args []string) error {
					dir := ""
					switch len(args) {
					case 0:
					case 1:
						dir = args[0]
					default:
						return &cli.UsageError{Msg: "fs attach takes at most one DIR"}
					}
					ttl, err := parseFSTTL(attachTTL, fsMountTTL)
					if err != nil {
						return err
					}
					return fsAttach(ctx, attachID, dir, attachMounts, ttl, attachForce)
				},
			},
		},
	}
}

// atMostOneFolderName is the FOLDER a verb takes, "" when none was given.
func atMostOneFolderName(ctx *cli.Context, args []string) (string, error) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	}
	return "", fmt.Errorf("%s takes one folder name (got %q)",
		strings.Join(ctx.Path[1:], " "), strings.Join(args, " "))
}

// parseFSTTL reads a --ttl: a duration, or bare seconds, within the
// engine's ceiling; "" is def.
func parseFSTTL(v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := parseUpTimeout(v)
	if err != nil {
		return 0, &cli.UsageError{Msg: fmt.Sprintf("--ttl: '%s' is not a duration (try 30m or 12h)", v)}
	}
	if d > fsMaxTTL {
		return 0, &cli.UsageError{Msg: fmt.Sprintf("--ttl %s is more than the engine allows (%s)", v, fsMaxTTL)}
	}
	return d, nil
}

// fsSession is what every fs verb opens: the session, the sandbox, its
// filesystem member and a client for the member's control URL.
type fsSession struct {
	s   *session
	sb  *api.Sandbox
	svc *api.ServiceInfo
	tw  *twin.Client
}

// openFS opens the sandbox and finds its filesystem. A sandbox without one
// is the printed error (exit 1) naming the flag that gives it folders.
func openFS(ctx *cli.Context, idFlag string) (*fsSession, error) {
	s, _, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return nil, err
	}
	svc := findService(sb.Services, filesystemService)
	if svc == nil {
		s.ui.Fail("Sandbox %s has no filesystem", sb.ID)
		s.ui.Next("veris up --fs " + fsSpecGrammar)
		return nil, printed(1)
	}
	if svc.ControlURL == "" {
		s.ui.Fail("The filesystem of sandbox %s has no control URL to reach it through", sb.ID)
		return nil, printed(1)
	}
	// Older deployments advertised HTTP even through an HTTPS control
	// plane; the client is built on the upgraded URL so its key rides the
	// origin the requests actually go to.
	tw := s.twin(upgradedControlURL(svc.ControlURL, s.plane().Base))
	return &fsSession{s: s, sb: sb, svc: svc, tw: tw}, nil
}

// fail is the fs verbs' failure line: a 404 on the optional routes is a
// control plane or engine without them, said as such; anything else is
// the session's own report.
func (f *fsSession) fail(verb, noun string, err error) error {
	if errors.Is(err, twin.ErrNotSupported) {
		f.s.ui.Fail("The control plane does not serve filesystem routes for sandbox %s (upgrade services-sandbox)", f.sb.ID)
		return printed(1)
	}
	return f.s.fail(verb, noun, err)
}

// davURL is the WebDAV root with its trailing slash, as rclone and Finder
// want it, upgraded to HTTPS as the control URL is when an HTTPS plane
// advertises http:// for its own host: Basic credentials ride every
// request to it, and push follows no redirect.
func (f *fsSession) davURL() string {
	return strings.TrimSuffix(upgradedControlURL(f.svc.URL, f.s.plane().Base), "/") + "/"
}

// folderURL is one folder's WebDAV directory, trailing slash included.
func (f *fsSession) folderURL(folder string) string {
	return f.davURL() + url.PathEscape(folder) + "/"
}

// layout reads the folder list, or fails in the verbs' voice.
func (f *fsSession) layout(ctx context.Context) (*twin.FSLayout, error) {
	l, err := f.tw.FSLayout(ctx)
	if err != nil {
		return nil, f.fail("read", "layout of the filesystem of sandbox "+f.sb.ID, err)
	}
	return l, nil
}

// token mints a credential, or fails in the verbs' voice.
func (f *fsSession) token(ctx context.Context, ttl time.Duration) (*twin.FSToken, error) {
	tok, err := f.tw.FSToken(ctx, ttl)
	if err != nil {
		return nil, f.fail("mint", "a credential for the filesystem of sandbox "+f.sb.ID, err)
	}
	return tok, nil
}

// chooseFolder is the folder a verb acts on when the command line may not
// have named one, in chooseTwin's shape: a sandbox with one folder has
// answered, a terminal is asked, anything else is told the commands.
func (f *fsSession) chooseFolder(layout *twin.FSLayout, name, idFlag string) (*twin.FSFolder, error) {
	verb := strings.Join(f.s.ctx.Path[1:], " ")
	if name != "" {
		for i := range layout.Folders {
			if layout.Folders[i].Name == name {
				return &layout.Folders[i], nil
			}
		}
		f.s.ui.Fail("No folder named '%s' in sandbox %s (have: %s)", name, f.sb.ID, strings.Join(folderNames(layout), ", "))
		f.s.ui.Next("veris sandbox fs connect")
		return nil, printed(1)
	}
	switch len(layout.Folders) {
	case 0:
		f.s.ui.Fail("The filesystem of sandbox %s has no folders", f.sb.ID)
		return nil, printed(1)
	case 1:
		f.s.ui.Info("%s of %s, the sandbox's only folder", verb, layout.Folders[0].Name)
		return &layout.Folders[0], nil
	}
	if f.s.ui.TTY {
		opts := make([]ui.Option, 0, len(layout.Folders))
		for _, fo := range layout.Folders {
			opts = append(opts, ui.Option{Value: fo.Name, Label: fo.Name, Detail: fo.Source.Where()})
		}
		opt, err := f.s.ui.Select("Which folder?", opts, "FOLDER")
		if err != nil {
			return nil, err
		}
		return f.chooseFolder(layout, opt.Value, idFlag)
	}
	f.s.ui.Fail("%s needs the folder's name. Sandbox %s has %d:", verb, f.sb.ID, len(layout.Folders))
	for _, fo := range layout.Folders {
		f.s.ui.Next(twinCommand(verb, fo.Name, idFlag))
	}
	return nil, printed(1)
}

func folderNames(l *twin.FSLayout) []string {
	names := make([]string, 0, len(l.Folders))
	for _, fo := range l.Folders {
		names = append(names, fo.Name)
	}
	return names
}

// pulling is the folders among names (every folder when none is named)
// whose pull has not finished.
func pulling(l *twin.FSLayout, names ...string) []twin.FSFolder {
	var out []twin.FSFolder
	for _, fo := range l.Folders {
		if fo.Ready {
			continue
		}
		if len(names) == 0 || slices.Contains(names, fo.Name) {
			out = append(out, fo)
		}
	}
	return out
}

// awaitReady holds until the named folders (every folder when none is
// named) have finished pulling, re-reading the layout under a spinner, and
// returns the layout that said so -- the one that carries each folder's
// sha. A folder still pulling at fsReadyTimeout is the printed error naming
// it and its source, exit 1; nothing of the verb has happened by then.
func (f *fsSession) awaitReady(ctx context.Context, layout *twin.FSLayout, names ...string) (*twin.FSLayout, error) {
	waiting := pulling(layout, names...)
	if len(waiting) == 0 {
		return layout, nil
	}
	label := func(fo twin.FSFolder) string {
		return fmt.Sprintf("Waiting for %s to finish pulling (%s)", fo.Name, fo.Source.Where())
	}
	sp := f.s.ui.Spinner(label(waiting[0]))
	deadline := time.Now().Add(fsReadyTimeout)
	for {
		select {
		case <-ctx.Done():
			sp.Stop()
			return nil, ctx.Err()
		case <-time.After(fsReadyPoll):
		}
		fresh, err := f.tw.FSLayout(ctx)
		if err != nil {
			sp.Stop()
			return nil, f.fail("read", "layout of the filesystem of sandbox "+f.sb.ID, err)
		}
		layout = fresh
		if waiting = pulling(layout, names...); len(waiting) == 0 {
			sp.Stop()
			return layout, nil
		}
		if time.Now().After(deadline) {
			sp.Stop()
			f.s.ui.Fail("Folder %s of sandbox %s is still being pulled from %s after %s; try again once it is ready",
				waiting[0].Name, f.sb.ID, waiting[0].Source.Where(), fsReadyTimeout)
			return nil, printed(1)
		}
		sp.Update(label(waiting[0]))
	}
}

// expiryLine is "expires 13:04 MST" for a token, or nothing readable.
func expiryLine(tok *twin.FSToken) string {
	if at, ok := tok.Expiry(); ok {
		return "expires " + at.Local().Format("15:04 MST")
	}
	if tok.ExpiresAt != "" {
		return "expires " + tok.ExpiresAt
	}
	return ""
}

// --- connect ----------------------------------------------------------------

// fsConnect mints a credential and prints it as exports on stdout, with
// the ways to use it on stderr. The password goes to stdout only: stderr
// is where a transcript or a CI log lands.
func fsConnect(ctx *cli.Context, idFlag string, ttl time.Duration) error {
	f, err := openFS(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	layout, err := f.layout(bg)
	if err != nil {
		return err
	}
	tok, err := f.token(bg, ttl)
	if err != nil {
		return err
	}
	if f.s.ctx.Globals.JSON {
		return printJSON(f.s.ctx.Stdout, map[string]any{
			"url": f.davURL(), "user": tok.User, "password": tok.Password, "expires_at": tok.ExpiresAt,
			"folders": folderNames(layout),
		})
	}
	for _, line := range []string{
		"export " + envFSURL + "=" + shellQuote(f.davURL()),
		"export " + envFSUser + "=" + shellQuote(tok.User),
		"export " + envFSPassword + "=" + shellQuote(tok.Password),
	} {
		if _, err := fmt.Fprintln(f.s.ctx.Stdout, line); err != nil {
			return err
		}
	}
	noun := "folders"
	if len(layout.Folders) == 1 {
		noun = "folder"
	}
	f.s.ui.Info("Filesystem of %s: %d %s (%s) · %s", f.sb.ID, len(layout.Folders), noun, strings.Join(folderNames(layout), ", "), expiryLine(tok))
	// The credential opens every folder at once, pulled or not; a folder
	// the engine is still filling is said so a Finder window that looks
	// thin is understood.
	if still := pulling(layout); len(still) > 0 {
		names := make([]string, 0, len(still))
		for _, fo := range still {
			names = append(names, fo.Name)
		}
		f.s.ui.Warn("Still pulling: %s; diff, pull, push and mount wait for them", strings.Join(names, ", "))
	}
	f.s.ui.Detail(`eval "$(veris sandbox fs connect)"   then, for instance:`)
	f.s.ui.Detail(`rclone lsd :webdav,url=$%s,vendor=other,user=$%s,pass=$(rclone obscure "$%s"):`, envFSURL, envFSUser, envFSPassword)
	f.s.ui.Detail("Finder: Go ▸ Connect to Server ▸ $%s, then the user and password", envFSURL)
	return nil
}

// --- diff -------------------------------------------------------------------

// fsDiffStatus is GET /veris/fs/diff?format=status as the engine shapes it.
type fsDiffStatus struct {
	Folder       string `json:"folder"`
	BaselineSHA  string `json:"baseline_sha"`
	FilesChanged int    `json:"files_changed"`
	Insertions   int    `json:"insertions"`
	Deletions    int    `json:"deletions"`
	Files        []struct {
		Path       string `json:"path"`
		Status     string `json:"status"`
		Insertions int    `json:"insertions"`
		Deletions  int    `json:"deletions"`
	} `json:"files"`
}

// fsDiff prints a folder's changes: the status table, or the patch as the
// engine sent it, on stdout. --json passes the status document through.
func fsDiff(ctx *cli.Context, idFlag, name, format string) error {
	f, err := openFS(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	layout, err := f.layout(bg)
	if err != nil {
		return err
	}
	folder, err := f.chooseFolder(layout, name, idFlag)
	if err != nil {
		return err
	}
	if _, err := f.awaitReady(bg, layout, folder.Name); err != nil {
		return err
	}
	dctx, cancel := context.WithTimeout(bg, fsDiffTimeout)
	defer cancel()
	body, truncated, err := f.tw.FSDiff(dctx, folder.Name, format)
	if err != nil {
		return f.fail("diff", "folder "+folder.Name, err)
	}
	if truncated {
		f.s.ui.Warn("The patch was cut at the engine's cap; pull the folder for the whole change")
	}
	if format == twin.FSDiffPatch {
		if len(body) > 0 && body[len(body)-1] != '\n' {
			body = append(body, '\n')
		}
		_, err := f.s.ctx.Stdout.Write(body)
		return err
	}
	if f.s.ctx.Globals.JSON {
		return printJSON(f.s.ctx.Stdout, json.RawMessage(body))
	}
	var st fsDiffStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return f.s.fail("read", "diff of folder "+folder.Name, fmt.Errorf("the answer is not a status document: %w", err))
	}
	return renderDiffStatus(f.s.ctx.Stdout, st)
}

// renderDiffStatus writes the status as git's own --stat would, roughly:
//
//	app · 2 files changed, +12 -3 (baseline 1a2b3c4d)
//	M  src/main.py   +10 -3
//	A  notes.md      +2
func renderDiffStatus(w io.Writer, st fsDiffStatus) error {
	head := fmt.Sprintf("%s · %d files changed, +%d -%d", st.Folder, st.FilesChanged, st.Insertions, st.Deletions)
	if st.FilesChanged == 1 {
		head = fmt.Sprintf("%s · 1 file changed, +%d -%d", st.Folder, st.Insertions, st.Deletions)
	}
	if st.BaselineSHA != "" {
		head += " (baseline " + shortID(st.BaselineSHA) + ")"
	}
	if _, err := fmt.Fprintln(w, head); err != nil {
		return err
	}
	width := 0
	for _, file := range st.Files {
		width = max(width, len(file.Path))
	}
	for _, file := range st.Files {
		counts := ""
		if file.Insertions > 0 {
			counts += fmt.Sprintf(" +%d", file.Insertions)
		}
		if file.Deletions > 0 {
			counts += fmt.Sprintf(" -%d", file.Deletions)
		}
		if _, err := fmt.Fprintf(w, "%-2s %-*s %s\n", file.Status, width, file.Path, strings.TrimSpace(counts)); err != nil {
			return err
		}
	}
	return nil
}

// --- pull -------------------------------------------------------------------

// fsPull exports a folder and unpacks it into dest.
func fsPull(ctx *cli.Context, idFlag, name, dest string, force bool) error {
	f, err := openFS(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	layout, err := f.layout(bg)
	if err != nil {
		return err
	}
	folder, err := f.chooseFolder(layout, name, idFlag)
	if err != nil {
		return err
	}
	if dest == "" {
		dest = folder.Name
	}
	if err := checkDest(dest, force); err != nil {
		f.s.ui.Fail("%v", err)
		f.s.ui.Next(fmt.Sprintf("veris sandbox fs pull %s --dest %s --force", folder.Name, dest))
		return printed(1)
	}
	if _, err := f.awaitReady(bg, layout, folder.Name); err != nil {
		return err
	}
	sum, err := f.pullFolder(bg, folder.Name, dest, nil)
	if err != nil {
		return err
	}
	f.s.ui.Success("Pulled %s: %d %s, %s → %s", folder.Name, sum.Files, plural(sum.Files, "file", "files"), byteSize(sum.Bytes), dest)
	f.s.ui.Link(fmt.Sprintf("veris sandbox fs push %s --src %s   (send changes back)", folder.Name, dest))
	return nil
}

// pullFolder streams one folder's export into dest; the caller has held
// dest to checkDest. The stream has no client timeout of its own (a
// gigabyte export would be cut under the JSON budget), so the transfer
// deadline rides the context, held from the request through the last byte
// unpacked and the body's close.
//
// shadowed names the top-level paths left out of it (see untarShadowing).
func (f *fsSession) pullFolder(ctx context.Context, folder, dest string, shadowed map[string]bool) (tarSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, fsTransferTimeout)
	defer cancel()
	sp := f.s.ui.Spinner(fmt.Sprintf("Pulling %s", folder))
	rc, err := f.tw.FSExport(ctx, folder)
	if err != nil {
		sp.Stop()
		return tarSummary{}, f.fail("export", "folder "+folder, err)
	}
	sum, err := untarShadowing(rc, dest, shadowed)
	rc.Close()
	sp.Stop()
	if err != nil {
		return sum, f.s.fail("unpack", "folder "+folder+" into "+dest, err)
	}
	for _, skipped := range sum.Skipped {
		f.s.ui.Warn("%s: %s is neither a file, a directory nor a link; left out", folder, skipped)
	}
	for _, name := range sum.Shadowed {
		f.s.ui.Warn("%s: %s/ is left out; folder %s lands there, as in the pod", folder, name, name)
	}
	return sum, nil
}

// checkDest is whether dest can be pulled into: absent, an empty
// directory, or anything at all with force.
func checkDest(dest string, force bool) error {
	entries, err := os.ReadDir(dest)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%s: %w", dest, err)
	case len(entries) > 0 && !force:
		return fmt.Errorf("%s is not empty (%d entries); --force writes into it", dest, len(entries))
	}
	return nil
}

// untar unpacks a gzip tar into dest: directories, regular files and
// symlinks, each held inside dest -- a name with .. or a leading /, or a
// link whose target leaves dest, is refused before anything of it lands.
// The check is on the filesystem as well as the name: with --force the
// destination may already hold symlinks (its own, or an earlier pull's),
// and writing through one would reach outside. Every write goes through an
// os.Root opened on dest, which refuses any path that resolves outside it,
// so nothing -- not even a parent directory -- is made beyond dest for an
// entry that is refused; before that, the entry's existing ancestors are
// walked so the refusal names the symlink that leads out. A symlink where
// a file or directory goes is replaced rather than followed, and a file
// already there is replaced too (written beside it and renamed over), so a
// hard link to something outside is never written through; the file takes
// the archive's mode, executable bit included, whatever the old one had.
//
// A link entry's target is held where it would really point: from its
// parent as that resolves on disk (a symlinked directory inside dest may
// sit higher than the entry's name says), and through any symlink already
// in dest that the target goes on into. It is stored clean -- "a/../x" as
// "x" -- so a component that is itself a symlink cannot carry a later ".."
// above dest.
func untar(r io.Reader, dest string) (tarSummary, error) {
	return untarShadowing(r, dest, nil)
}

// untarShadowing is untar leaving out every entry under a top-level name
// in shadowed -- where another folder lands, as the pod's mount of it
// shadows the root folder's own copy -- and naming each in the summary.
func untarShadowing(r io.Reader, dest string, shadowed map[string]bool) (tarSummary, error) {
	var sum tarSummary
	gz, err := gzip.NewReader(r)
	if err != nil {
		return sum, fmt.Errorf("not a gzip stream: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return sum, err
	}
	// Held absolute and resolved, so "." and a destination named through a
	// symlink compare against what their entries' parents resolve to.
	abs, err := filepath.Abs(dest)
	if err != nil {
		return sum, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return sum, err
	}
	root, err := os.OpenRoot(real)
	if err != nil {
		return sum, err
	}
	defer root.Close()
	// made is every link this unpack has made, by where it sits on disk:
	// one that does not resolve yet (its target comes later in the
	// archive) is inside by construction, not a symlink of unknown reach.
	made := map[string]bool{}
	// written is every regular file this unpack wrote, by its name in the
	// archive: what a hard-link entry may name.
	written := map[string]bool{}
	// place holds rel's parent to the destination and makes it, then
	// clears a symlink sitting where the entry goes.
	place := func(rel string) error {
		if err := ancestorsInside(root, real, rel, made); err != nil {
			return err
		}
		if parent := filepath.Dir(rel); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
		if fi, err := root.Lstat(rel); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return root.Remove(rel)
		}
		return nil
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return sum, nil
		}
		if err != nil {
			return sum, err
		}
		slashRel, err := insideDest(hdr.Name)
		if err != nil {
			return sum, err
		}
		if slashRel == "" {
			continue
		}
		if top, _, _ := strings.Cut(slashRel, "/"); shadowed[top] {
			if !slices.Contains(sum.Shadowed, top) {
				sum.Shadowed = append(sum.Shadowed, top)
			}
			continue
		}
		rel := filepath.FromSlash(slashRel)
		mode := hdr.FileInfo().Mode()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := place(rel); err != nil {
				return sum, err
			}
			if err := root.MkdirAll(rel, mode.Perm()|0o700); err != nil {
				return sum, err
			}
		case tar.TypeReg:
			if err := place(rel); err != nil {
				return sum, err
			}
			n, err := replaceFile(root, rel, tr, mode.Perm()|0o600)
			if err != nil {
				return sum, fmt.Errorf("%s: %w", slashRel, err)
			}
			written[slashRel] = true
			sum.Files++
			sum.Bytes += n
		case tar.TypeLink:
			// A tar names a hard link's second and later paths this way,
			// by the archive name of the first. It is unpacked as a copy
			// of that file, and only of a regular file this unpack wrote
			// -- never of something the destination already held.
			from, err := insideDest(hdr.Linkname)
			if err != nil || from == "" || !written[from] {
				return sum, fmt.Errorf("%s: a hard link to %s, which is not a file this archive unpacked, is refused", slashRel, hdr.Linkname)
			}
			if err := place(rel); err != nil {
				return sum, err
			}
			n, err := copyWithin(root, filepath.FromSlash(from), rel)
			if err != nil {
				return sum, fmt.Errorf("%s: %w", slashRel, err)
			}
			written[slashRel] = true
			sum.Files++
			sum.Bytes += n
		case tar.TypeSymlink:
			if path.IsAbs(hdr.Linkname) || filepath.IsAbs(hdr.Linkname) {
				return sum, fmt.Errorf("%s: a symlink to an absolute path (%s) is refused", slashRel, hdr.Linkname)
			}
			if hdr.Linkname == "" || strings.ContainsRune(hdr.Linkname, 0) {
				return sum, fmt.Errorf("%s: a symlink with no target is refused", slashRel)
			}
			target := path.Clean(hdr.Linkname)
			if err := ancestorsInside(root, real, rel, made); err != nil {
				return sum, err
			}
			parent, err := resolveUnder(real, filepath.Dir(rel))
			if err != nil {
				return sum, fmt.Errorf("%s: %w", slashRel, err)
			}
			at, err := insideDest(path.Join(parent, target))
			if err != nil {
				return sum, fmt.Errorf("%s: a symlink leaving the folder (%s) is refused", slashRel, hdr.Linkname)
			}
			if err := pathInside(root, real, filepath.FromSlash(at), slashRel, made); err != nil {
				return sum, err
			}
			if err := place(rel); err != nil {
				return sum, err
			}
			_ = root.Remove(rel)
			if err := root.Symlink(filepath.FromSlash(target), rel); err != nil {
				return sum, err
			}
			delete(written, slashRel)
			made[filepath.Join(real, filepath.FromSlash(parent), filepath.Base(rel))] = true
		default:
			sum.Skipped = append(sum.Skipped, slashRel)
		}
	}
}

// ancestorsInside walks rel's existing ancestors under the root opened on
// real (the destination, absolute and resolved) and refuses one that is a
// symlink leading outside it, naming it. It is the readable refusal; the
// root's own confinement is what holds every write.
func ancestorsInside(root *os.Root, real, rel string, made map[string]bool) error {
	return pathInside(root, real, filepath.Dir(rel), filepath.ToSlash(rel), made)
}

// pathInside walks p (relative to real) and each of its existing
// ancestors, p included, and refuses one that is a symlink leading
// outside real or not resolving at all, naming entry. A link this unpack
// made (made, by its place on disk) may not resolve yet; what lies past it
// is made by the archive, inside.
func pathInside(root *os.Root, real, p, entry string, made map[string]bool) error {
	if p == "." || p == "" {
		return nil
	}
	parts := strings.Split(p, string(filepath.Separator))
	for i := range parts {
		prefix := filepath.Join(parts[:i+1]...)
		fi, err := root.Lstat(prefix)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the rest is made fresh, inside
		}
		if err != nil {
			return fmt.Errorf("%s: %w", entry, err)
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		to, err := filepath.EvalSymlinks(filepath.Join(real, prefix))
		if err != nil {
			if dir, derr := filepath.EvalSymlinks(filepath.Join(real, filepath.Dir(prefix))); derr == nil && made[filepath.Join(dir, filepath.Base(prefix))] {
				return nil
			}
			return fmt.Errorf("%s: a symlink in the destination (%s) does not resolve; the entry is refused", entry, filepath.ToSlash(prefix))
		}
		if !within(real, to) {
			return fmt.Errorf("%s: a symlink in the destination leads outside it (%s); the entry is refused", entry, to)
		}
	}
	return nil
}

// resolveUnder is where dir (relative to real, the destination resolved)
// is on disk, as a slash path relative to real: its longest existing
// ancestor resolved through any symlink, and the rest -- made fresh as
// plain directories -- joined on. The caller has held dir's ancestors to
// ancestorsInside; one that resolves outside is refused all the same.
func resolveUnder(real, dir string) (string, error) {
	p, rest := dir, ""
	for p != "." && p != "" {
		if _, err := os.Lstat(filepath.Join(real, p)); err == nil {
			break
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = filepath.Dir(p)
	}
	to, err := filepath.EvalSymlinks(filepath.Join(real, p))
	if err != nil {
		return "", fmt.Errorf("its parent in the destination does not resolve: %w", err)
	}
	if !within(real, to) {
		return "", fmt.Errorf("a symlink in the destination leads outside it (%s); the entry is refused", to)
	}
	r, err := filepath.Rel(real, filepath.Join(to, rest))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(r), nil
}

// copyWithin is from's content and mode written over to, both under root;
// from must still be a regular file.
func copyWithin(root *os.Root, from, to string) (int64, error) {
	if fi, err := root.Lstat(from); err != nil || !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is no longer a file", filepath.ToSlash(from))
	}
	src, err := root.Open(from)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return 0, err
	}
	return replaceFile(root, to, src, fi.Mode().Perm())
}

// replaceFile writes r to a fresh file beside rel and renames it over
// rel, all within root, so whatever rel was -- a hard link to a file
// outside the destination, a file with other permission bits -- is
// replaced rather than written through. The file ends with perm exactly.
func replaceFile(root *os.Root, rel string, r io.Reader, perm fs.FileMode) (int64, error) {
	var (
		tmp string
		out *os.File
		err error
	)
	for range 8 {
		var b [8]byte
		_, _ = rand.Read(b[:])
		tmp = filepath.Join(filepath.Dir(rel), fmt.Sprintf(".veris-pull-%x", b))
		out, err = root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, r)
	if err == nil {
		err = out.Chmod(perm)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return n, err
	}
	return n, nil
}

// within is whether p is base or under it; both absolute and resolved.
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// insideDest is name cleaned as a slash path under the destination, or
// an error when it would leave it. "" is the destination itself.
func insideDest(name string) (string, error) {
	if strings.HasPrefix(name, "/") || filepath.IsAbs(name) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%s: an absolute path in the archive is refused", name)
	}
	clean := path.Clean(strings.ReplaceAll(name, `\`, "/"))
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%s: a path leaving the folder is refused", name)
	}
	return clean, nil
}

// --- push -------------------------------------------------------------------

// fsPush uploads a local directory into a folder over WebDAV.
func fsPush(ctx *cli.Context, idFlag, name, src string) error {
	f, err := openFS(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	layout, err := f.layout(bg)
	if err != nil {
		return err
	}
	folder, err := f.chooseFolder(layout, name, idFlag)
	if err != nil {
		return err
	}
	if src == "" {
		src = folder.Name
	}
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		f.s.ui.Fail("%s is not a directory to push", src)
		return printed(1)
	}
	// A push into a folder the engine is still cloning would interleave
	// with it; the baseline is recorded only once the pull ends.
	if _, err := f.awaitReady(bg, layout, folder.Name); err != nil {
		return err
	}
	tok, err := f.token(bg, fsConnectTTL)
	if err != nil {
		return err
	}
	sp := f.s.ui.Spinner(fmt.Sprintf("Pushing %s → %s", src, folder.Name))
	sum, err := pushDir(bg, src, f.folderURL(folder.Name), tok)
	sp.Stop()
	if err != nil {
		return f.s.fail("push", src+" into folder "+folder.Name, err)
	}
	for _, skipped := range sum.Skipped {
		f.s.ui.Warn("%s: %s is neither a file, a directory nor a symlink; left out", src, skipped)
	}
	f.s.ui.Success("Pushed %s → %s: %d %s, %s", src, folder.Name, sum.Files, plural(sum.Files, "file", "files"), byteSize(sum.Bytes))
	f.s.ui.Link(fmt.Sprintf("veris sandbox fs diff %s   (what changed against the baseline)", folder.Name))
	return nil
}

// pushExcluded is what push leaves behind: what the upload leaves out, and
// a top-level .git -- the engine keeps the folder's own repository, and a
// local clone's objects written over it would be a repository neither
// side understands.
func pushExcluded(rel string) bool {
	return tarExcluded(rel) || rel == ".git" || strings.HasPrefix(rel, ".git/")
}

// davClient is the WebDAV client for push: a long transfer budget and no
// redirect following, since Basic credentials ride every request.
func davClient() *http.Client {
	return &http.Client{
		Timeout:       fsTransferTimeout,
		Transport:     direct.Transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// pushDir walks src and mirrors it under base: MKCOL for each directory,
// PUT for each file, a symlink as the .rclonelink file the engine's
// --links turns back into a link. src itself is resolved first, as tarDir
// resolves its root: WalkDir reads its root with Lstat, so a source named
// through a symlink (`current -> releases/v3`) would be visited once as a
// link and push would report 0 files sent as success.
//
// A path whose kind changed is replaced, not left beside its other
// spelling: the engine reads x.rclonelink as a link only while x is
// absent, so a file that became a symlink has the file deleted first, and
// a symlink that became a file or directory has its marker deleted. Each
// directory's remote listing (one PROPFIND) says which applies. A
// directory in the folder where the source has a file or a symlink is
// refused -- push deletes no directory.
func pushDir(ctx context.Context, src, base string, tok *twin.FSToken) (tarSummary, error) {
	var sum tarSummary
	src, err := filepath.EvalSymlinks(src)
	if err != nil {
		return sum, err
	}
	hc := davClient()
	lists := map[string]map[string]bool{}
	// remote is whether rel exists in the folder, and is a directory.
	remote := func(rel string) (exists, isDir bool, err error) {
		dir := path.Dir(rel)
		l, ok := lists[dir]
		if !ok {
			endpoint := base
			if dir != "." {
				endpoint += escapePath(dir) + "/"
			}
			if l, err = davList(ctx, hc, endpoint, tok); err != nil {
				return false, false, err
			}
			lists[dir] = l
		}
		isDir, exists = l[path.Base(rel)]
		return exists, isDir, nil
	}
	// replace clears what the folder holds at rel in another spelling:
	// the stem for a link (the marker for a file or directory), and a file
	// where a directory goes.
	replace := func(rel, kind string) error {
		marker := rel + ".rclonelink"
		if kind == "symlink" {
			marker = ""
		}
		if marker != "" {
			if exists, isDir, err := remote(marker); err != nil {
				return err
			} else if exists && isDir {
				return fmt.Errorf("%s is a directory in the folder, where %s's symlink marker would be; push deletes no directory -- remove or rename it in the sandbox first", marker, rel)
			} else if exists {
				if err := davRequest(ctx, hc, http.MethodDelete, base+escapePath(marker), nil, 0, tok, http.StatusNotFound); err != nil {
					return err
				}
			}
		}
		exists, isDir, err := remote(rel)
		switch {
		case err != nil:
			return err
		case !exists:
			return nil
		case isDir && kind != "directory":
			return fmt.Errorf("%s is a directory in the folder and a %s here; push replaces files but deletes no directory -- remove it in the sandbox first", rel, kind)
		case !isDir && kind != "file":
			return davRequest(ctx, hc, http.MethodDelete, base+escapePath(rel), nil, 0, tok, http.StatusNotFound)
		}
		return nil
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if pushExcluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := replace(rel, "directory"); err != nil {
				return err
			}
			return davRequest(ctx, hc, "MKCOL", base+escapePath(rel)+"/", nil, 0, tok, http.StatusMethodNotAllowed)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := replace(rel, "symlink"); err != nil {
				return err
			}
			if err := davRequest(ctx, hc, http.MethodPut, base+escapePath(rel)+".rclonelink", strings.NewReader(link), int64(len(link)), tok); err != nil {
				return err
			}
			sum.Files++
			sum.Bytes += int64(len(link))
			return nil
		case !info.Mode().IsRegular():
			sum.Skipped = append(sum.Skipped, rel)
			return nil
		}
		if err := replace(rel, "file"); err != nil {
			return err
		}
		file, err := os.Open(p)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := davRequest(ctx, hc, http.MethodPut, base+escapePath(rel), file, info.Size(), tok); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		sum.Files++
		sum.Bytes += info.Size()
		return nil
	})
	return sum, err
}

// davMultistatus is the part of a PROPFIND answer davList reads.
type davMultistatus struct {
	Responses []struct {
		Href       string    `xml:"href"`
		Collection *struct{} `xml:"propstat>prop>resourcetype>collection"`
	} `xml:"response"`
}

// davList is a WebDAV collection's children, name → whether each is a
// collection, from one Depth: 1 PROPFIND. A collection that does not exist
// yet has none.
func davList(ctx context.Context, hc *http.Client, endpoint string, tok *twin.FSToken) (map[string]bool, error) {
	const query = `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/></D:prop></D:propfind>`
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", endpoint, strings.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	req.SetBasicAuth(tok.User, tok.Password)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusMultiStatus:
	case http.StatusNotFound:
		return map[string]bool{}, nil
	case http.StatusUnauthorized:
		return nil, errors.New("the WebDAV root refused the credential just minted; the filesystem may have restarted")
	default:
		return nil, fmt.Errorf("PROPFIND %s answered %s", endpoint, resp.Status)
	}
	var ms davMultistatus
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("PROPFIND %s: %w", endpoint, err)
	}
	self, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimRight(self.Path, "/")
	out := map[string]bool{}
	for _, r := range ms.Responses {
		u, err := url.Parse(strings.TrimSpace(r.Href))
		if err != nil {
			continue
		}
		// The collection itself is skipped, also when a proxy in front of
		// the server leaves its own prefix off the href.
		p := strings.TrimRight(u.Path, "/")
		if p == "" || p == dir || strings.HasSuffix(dir, "/"+strings.TrimLeft(p, "/")) {
			continue
		}
		out[path.Base(p)] = r.Collection != nil
	}
	return out, nil
}

// escapePath escapes each segment of a slash path for a URL.
func escapePath(rel string) string {
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return strings.Join(segs, "/")
}

// davRequest sends one WebDAV request with Basic auth; any 2xx, or one of
// the also-acceptable statuses, is success. A 401 is the credential.
func davRequest(ctx context.Context, hc *http.Client, method, endpoint string, body io.Reader, size int64, tok *twin.FSToken, alsoOK ...int) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.SetBasicAuth(tok.User, tok.Password)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	for _, ok := range alsoOK {
		if resp.StatusCode == ok {
			return nil
		}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("the WebDAV root refused the credential just minted; the filesystem may have restarted")
	}
	return fmt.Errorf("%s %s answered %s", method, endpoint, resp.Status)
}

// --- mount ------------------------------------------------------------------

// fsMount mounts the filesystem, or one folder, at mountpoint through
// rclone, holding until Ctrl-C unless daemon.
func fsMount(ctx *cli.Context, idFlag, folder, mountpoint string, ttl time.Duration, daemon bool) error {
	f, err := openFS(ctx, idFlag)
	if err != nil {
		return err
	}
	rclone, err := f.needRclone()
	if err != nil {
		return err
	}
	bg := context.Background()
	layout, err := f.layout(bg)
	if err != nil {
		return err
	}
	var names []string
	if folder != "" {
		if _, err := f.chooseFolder(layout, folder, idFlag); err != nil {
			return err
		}
		names = []string{folder}
	}
	// The whole filesystem waits for every folder; one folder for itself.
	if _, err := f.awaitReady(bg, layout, names...); err != nil {
		return err
	}
	tok, err := f.token(bg, ttl)
	if err != nil {
		return err
	}
	what := "the filesystem of " + f.sb.ID
	if folder != "" {
		what = "folder " + folder
	}
	cmd, err := rcloneMount(rclone, f.davURL(), folder, mountpoint, tok, daemon)
	if err != nil {
		return f.s.fail("mount", what, err)
	}
	if daemon {
		if out, err := cmd.CombinedOutput(); err != nil {
			return f.s.fail("mount", what, fmt.Errorf("%w: %s", err, firstLine(out)))
		}
		f.s.ui.Success("Mounted %s at %s (%s)", what, mountpoint, expiryLine(tok))
		f.s.ui.Next(unmountHint(mountpoint))
		return nil
	}
	cmd.Stderr = f.s.ui.Out
	sigCtx, stop := signal.NotifyContext(bg, os.Interrupt)
	defer stop()
	if err := cmd.Start(); err != nil {
		return f.s.fail("mount", what, err)
	}
	f.s.ui.Info("Mounting %s at %s (%s); Ctrl-C unmounts", what, mountpoint, expiryLine(tok))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-sigCtx.Done():
		procgroup.Terminate(cmd, os.Interrupt)
		<-done
		f.s.ui.Success("Unmounted %s", mountpoint)
		return nil
	case err := <-done:
		code, werr := procgroup.WaitStatus(cmd, err)
		if werr != nil {
			return f.s.fail("mount", what, werr)
		}
		if code != 0 {
			f.s.ui.Fail("rclone exited %d", code)
			return printed(1)
		}
		return nil
	}
}

// needRclone is the rclone binary, or the printed error naming how to get
// one -- and, on Windows, that there is no mount here at all.
func (f *fsSession) needRclone() (string, error) {
	if runtime.GOOS == "windows" {
		f.s.ui.Fail("mount is not available on Windows; pull and push copy the folders instead")
		f.s.ui.Next("veris sandbox fs pull")
		return "", printed(1)
	}
	path, err := lookPath(rcloneBinary)
	if err != nil {
		f.s.ui.Fail("rclone is not on PATH; mount needs it")
		f.s.ui.Next("brew install rclone  (or https://rclone.org/install/)")
		return "", printed(1)
	}
	return path, nil
}

// rcloneMount is the rclone command that mounts folder (or the whole
// filesystem when "") at mountpoint. The backend is configured through
// RCLONE_WEBDAV_* in the child's environment rather than the connection
// string, so the password is not in its argv for `ps` to read.
func rcloneMount(rclone, davURL, folder, mountpoint string, tok *twin.FSToken, daemon bool) (*exec.Cmd, error) {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return nil, err
	}
	remote := ":webdav:"
	if folder != "" {
		remote += folder + "/"
	}
	args, err := rcloneArgs(runtime.GOOS, remote, mountpoint, daemon)
	if err != nil {
		return nil, err
	}
	obscured, err := rcloneObscure(tok.Password)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(rclone, args...) //nolint:gosec // argv built above
	cmd.Env = append(os.Environ(), rcloneEnv(davURL, tok.User, obscured)...)
	procgroup.Isolate(cmd)
	return cmd, nil
}

// rcloneArgs is the mount subcommand per OS: nfsmount on macOS, which
// needs no kernel extension; mount (FUSE) on Linux; nothing on Windows.
// --vfs-cache-mode full lets editors write as they do on a disk; --links
// turns the engine's .rclonelink files back into symlinks.
func rcloneArgs(goos, remote, mountpoint string, daemon bool) ([]string, error) {
	var sub string
	switch goos {
	case "darwin":
		sub = "nfsmount"
	case "linux":
		sub = "mount"
	default:
		return nil, fmt.Errorf("mount is not available on %s", goos)
	}
	args := []string{sub, remote, mountpoint, "--vfs-cache-mode", "full", "--links"}
	if daemon {
		args = append(args, "--daemon")
	}
	return args, nil
}

// rcloneEnv configures rclone's webdav backend for the mount.
func rcloneEnv(davURL, user, obscuredPass string) []string {
	return []string{
		"RCLONE_WEBDAV_URL=" + davURL,
		"RCLONE_WEBDAV_VENDOR=other",
		"RCLONE_WEBDAV_USER=" + user,
		"RCLONE_WEBDAV_PASS=" + obscuredPass,
	}
}

// rcloneCryptKey is the fixed key rclone obscures configured passwords
// with (lib/obscure). It is not a secret -- rclone's own source carries it
// -- and obscuring hides a password from a shoulder, not from an attacker;
// the point here is only that rclone refuses a plaintext pass= value.
var rcloneCryptKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// rcloneObscure is `rclone obscure`: a random IV, AES-CTR under the fixed
// key, base64 URL-safe without padding. Done here rather than by shelling
// out so the password never reaches a child's argv.
func rcloneObscure(password string) (string, error) {
	block, err := aes.NewCipher(rcloneCryptKey)
	if err != nil {
		return "", err
	}
	plain := []byte(password)
	out := make([]byte, aes.BlockSize+len(plain))
	iv := out[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}
	cipher.NewCTR(block, iv).XORKeyStream(out[aes.BlockSize:], plain)
	return base64.RawURLEncoding.EncodeToString(out), nil
}

// rcloneReveal is the inverse, for tests.
func rcloneReveal(obscured string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(obscured)
	if err != nil {
		return "", err
	}
	if len(raw) < aes.BlockSize {
		return "", errors.New("too short")
	}
	block, err := aes.NewCipher(rcloneCryptKey)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCTR(block, raw[:aes.BlockSize]).XORKeyStream(out, raw[aes.BlockSize:])
	return string(out), nil
}

// unmountHint is the command that undoes a mount on this OS.
func unmountHint(mountpoint string) string {
	if runtime.GOOS == "linux" {
		return "fusermount -u " + shellQuote(mountpoint)
	}
	return "umount " + shellQuote(mountpoint)
}

// --- attach -----------------------------------------------------------------

// attachedFolder is one row of WORKSPACE_FOLDERS: where the folder landed,
// how (copy or mount), and what it came from.
type attachedFolder struct {
	Name   string        `json:"name"`
	Path   string        `json:"path"`
	Attach string        `json:"attach"`
	Source twin.FSSource `json:"source"`
	SHA    string        `json:"sha,omitempty"`
}

// fsAttach lays every folder out under dir as the pod lays /workspace out
// and prints the exports an agent run here reads.
func fsAttach(ctx *cli.Context, idFlag, dir string, mounts []string, ttl time.Duration, force bool) error {
	f, err := openFS(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	layout, err := f.layout(bg)
	if err != nil {
		return err
	}
	if len(layout.Folders) == 0 {
		f.s.ui.Fail("The filesystem of sandbox %s has no folders", f.sb.ID)
		return printed(1)
	}
	// Every folder lands, so every pull must have finished; the layout that
	// says so is the one whose shas WORKSPACE_FOLDERS carries.
	if layout, err = f.awaitReady(bg, layout); err != nil {
		return err
	}
	mounted := map[string]bool{}
	for _, name := range mounts {
		fo, err := f.chooseFolder(layout, name, idFlag)
		if err != nil {
			return err
		}
		// The root folder is DIR itself and the others land inside it; a
		// mount there would hide them, or refuse a mountpoint that is not
		// empty. Refused before anything is copied.
		if fo.Root && len(layout.Folders) > 1 {
			f.s.ui.Fail("%s is the root folder, so the other folders land inside it; mounting it would hide them", name)
			f.s.ui.Next("veris sandbox fs mount " + name + " DIR   (alone), or attach without --mount " + name)
			return printed(1)
		}
		mounted[name] = true
	}
	rclone := ""
	if len(mounted) > 0 {
		if rclone, err = f.needRclone(); err != nil {
			return err
		}
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return f.s.fail("choose", "a directory to attach under", err)
		}
		dir = filepath.Join(home, ".veris", "fs", f.sb.ID)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	// The root folder is DIR itself and the others land inside it, so the
	// root goes first -- DIR is held to checkDest while it is still empty
	// -- and every destination is checked before any byte lands, so a
	// refusal leaves nothing half-attached. Copies precede mounts for the
	// same reason; the mounts' one credential is minted once for all.
	folders := append([]twin.FSFolder(nil), layout.Folders...)
	sort.SliceStable(folders, func(i, j int) bool { return folders[i].Root && !folders[j].Root })
	var rows []attachedFolder
	for _, fo := range folders {
		dest := filepath.Join(dir, fo.Name)
		if fo.Root {
			dest = dir
		}
		row := attachedFolder{Name: fo.Name, Path: dest, Attach: "copy", Source: fo.Source, SHA: fo.Source.SHA}
		if mounted[fo.Name] {
			row.Attach = "mount"
		} else if err := checkDest(dest, force); err != nil {
			f.s.ui.Fail("%v", err)
			f.s.ui.Next("veris sandbox fs attach " + shellQuote(dir) + " --force")
			return printed(1)
		}
		rows = append(rows, row)
	}
	// The root folder's own copy of a path another folder lands on is left
	// out, as the pod's mount of that folder shadows it: a copy is not
	// merged with stale files, and a mountpoint stays empty for rclone.
	others := map[string]bool{}
	for _, fo := range folders {
		if !fo.Root {
			others[fo.Name] = true
		}
	}
	for _, row := range rows {
		if row.Attach != "copy" {
			continue
		}
		var shadowed map[string]bool
		if row.Path == dir {
			shadowed = others
		}
		sum, err := f.pullFolder(bg, row.Name, row.Path, shadowed)
		if err != nil {
			return err
		}
		f.s.ui.Success("Pulled %s: %d %s, %s → %s", row.Name, sum.Files, plural(sum.Files, "file", "files"), byteSize(sum.Bytes), row.Path)
	}
	var tok *twin.FSToken
	for _, row := range rows {
		if row.Attach != "mount" {
			continue
		}
		if tok == nil {
			if tok, err = f.token(bg, ttl); err != nil {
				return err
			}
		}
		cmd, err := rcloneMount(rclone, f.davURL(), row.Name, row.Path, tok, true)
		if err != nil {
			return f.s.fail("mount", "folder "+row.Name, err)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return f.s.fail("mount", "folder "+row.Name, fmt.Errorf("%w: %s", err, firstLine(out)))
		}
		f.s.ui.Success("Mounted %s at %s (%s)", row.Name, row.Path, expiryLine(tok))
		f.s.ui.Next(unmountHint(row.Path))
	}
	workspace := filepath.Join(dir, layout.Primary)
	if layout.Root != nil && *layout.Root == layout.Primary {
		workspace = dir
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	for _, line := range []string{
		"export " + envWorkspace + "=" + shellQuote(workspace),
		"export " + envFolders + "=" + shellQuote(string(encoded)),
	} {
		if _, err := fmt.Fprintln(f.s.ctx.Stdout, line); err != nil {
			return err
		}
	}
	f.s.ui.Info("Attached the filesystem of %s under %s", f.sb.ID, dir)
	f.s.ui.Detail(`eval "$(veris sandbox fs attach)"   gives an agent run here the pod's %s and %s`, envWorkspace, envFolders)
	return nil
}
