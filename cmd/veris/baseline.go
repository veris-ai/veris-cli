package main

import (
	"errors"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// baselineCommand is `veris baseline …`: the one snapshot pinned as what
// every new sandbox of an environment boots. Milestone 2 replaces this stub.
func baselineCommand() *cli.Command {
	return &cli.Command{
		Name:    "baseline",
		Summary: "What every new sandbox boots: get, promote, set, clear, list",
		Run: func(ctx *cli.Context, args []string) error {
			return errors.New("baseline is not implemented yet")
		},
	}
}
