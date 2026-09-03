package main

import (
	"errors"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// sandboxDataCommand is `veris sandbox data …`: schemas, rows and adding
// your own data to a running sandbox. Milestone 2 replaces this stub.
func sandboxDataCommand() *cli.Command {
	return &cli.Command{
		Name:    "data",
		Summary: "A sandbox's data: schema, get, add",
		Run: func(ctx *cli.Context, args []string) error {
			return errors.New("sandbox data is not implemented yet")
		},
	}
}
