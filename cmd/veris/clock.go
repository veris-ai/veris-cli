package main

import (
	"errors"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// sandboxClockCommand is `veris sandbox clock`: read or set the one virtual
// clock every twin in a sandbox shares. Milestone 2 replaces this stub.
func sandboxClockCommand() *cli.Command {
	return &cli.Command{
		Name:    "clock",
		Summary: "The sandbox's shared virtual clock: get, set",
		Run: func(ctx *cli.Context, args []string) error {
			return errors.New("sandbox clock is not implemented yet")
		},
	}
}
