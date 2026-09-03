package main

import (
	"flag"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// attachMilestoneThree adds the Milestone 3 verbs to the sandbox group:
// the exports leaf, and --exports / --format on sandbox get, which reach the
// same code. get's own flags and Run are wrapped rather than edited, so the
// group's literal in sandbox.go stays about the lifecycle verbs; the wrapper
// reads --id back from the FlagSet get bound it on, since the variable it
// binds is get's own.
func attachMilestoneThree(sandbox *cli.Command) {
	for _, c := range sandbox.Sub {
		if c.Name == "get" {
			addExportsFlag(c)
		}
	}
	sandbox.Sub = append(sandbox.Sub, sandboxExportsCommand())
}

// addExportsFlag gives get --exports and --format. With --exports the leaf
// is sandbox exports; without it, get runs as before.
func addExportsFlag(get *cli.Command) {
	var (
		exports bool
		format  string
		parsed  *flag.FlagSet
	)
	flags, run := get.Flags, get.Run
	get.Usage = "veris sandbox get [--id ID] [--watch] [--json] [--exports [--format env|dotenv|json]]"
	get.Flags = func(fs *flag.FlagSet) {
		if flags != nil {
			flags(fs)
		}
		fs.BoolVar(&exports, "exports", false, "print the twins' env hints as shell exports (sandbox exports)")
		fs.StringVar(&format, "format", "", "with --exports: env, dotenv or json (default env)")
		parsed = fs
	}
	get.Run = func(ctx *cli.Context, args []string) error {
		if !exports {
			if format != "" {
				return &cli.UsageError{Msg: "--format goes with --exports"}
			}
			return run(ctx, args)
		}
		if err := noPositionals(ctx, args); err != nil {
			return err
		}
		// The exports print once and exit; a watch has nothing to redraw.
		if f := parsed.Lookup("watch"); f != nil && f.Value.String() == "true" {
			return &cli.UsageError{Msg: "--watch does not go with --exports"}
		}
		id := ""
		if f := parsed.Lookup("id"); f != nil {
			id = f.Value.String()
		}
		return sandboxExports(ctx, id, format)
	}
}
