package main

import (
	"errors"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// snapshotCommand is `veris snapshot …`: record a sandbox's world as an
// image that later sandboxes can boot from. Milestone 2 replaces this stub.
func snapshotCommand() *cli.Command {
	return &cli.Command{
		Name:    "snapshot",
		Summary: "Recorded worlds: create, list, get, delete",
		Run: func(ctx *cli.Context, args []string) error {
			return errors.New("snapshot is not implemented yet")
		},
	}
}
