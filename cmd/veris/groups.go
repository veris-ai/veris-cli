package main

import "github.com/veris-ai/veris-cli/internal/cli"

// sandboxCommands is up, status, down and the sandbox group, with the
// group's Milestone 2 verbs (services, data, trace, clock) attached here so
// each of them lives in its own file and the group's literal in sandbox.go
// stays about the lifecycle verbs.
func sandboxCommands() []*cli.Command {
	cmds := sandboxBaseCommands()
	for _, c := range cmds {
		if c.Name == "sandbox" {
			c.Sub = append(c.Sub,
				sandboxServicesCommand(),
				sandboxDataCommand(),
				sandboxTraceCommand(),
				sandboxClockCommand(),
			)
		}
	}
	return cmds
}
