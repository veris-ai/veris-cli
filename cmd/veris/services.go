package main

import (
	"errors"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// sandboxServicesCommand is `veris sandbox services …`: the twins of a
// sandbox, their status, env hints, row counts and manuals. Milestone 2
// replaces this stub.
func sandboxServicesCommand() *cli.Command {
	return &cli.Command{
		Name:    "services",
		Summary: "The twins of a sandbox: list, get, manual",
		Run: func(ctx *cli.Context, args []string) error {
			return errors.New("sandbox services is not implemented yet")
		},
	}
}
