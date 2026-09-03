package main

import (
	"errors"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// sandboxTraceCommand is `veris sandbox trace`: the sandbox's own request
// ledger, merged across twins. Milestone 2 replaces this stub.
func sandboxTraceCommand() *cli.Command {
	return &cli.Command{
		Name:    "trace",
		Summary: "What the sandbox received, newest first",
		Run: func(ctx *cli.Context, args []string) error {
			return errors.New("sandbox trace is not implemented yet")
		},
	}
}
