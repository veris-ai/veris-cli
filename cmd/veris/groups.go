package main

import (
	"strings"

	"github.com/veris-ai/veris-cli/internal/cli"
)

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
				sandboxFilesCommand(),
				sandboxTraceCommand(),
				sandboxClockCommand(),
			)
			attachMilestoneThree(c)
			c.Sub = append(c.Sub, folderAliases(cmds)...)
		}
	}
	return cmds
}

// folderAliases puts up, status and down under `veris sandbox` as well as at
// the root, so a reader who learned the group first is not told its own
// lifecycle verbs live somewhere else.
//
// They stay at the root because that is where they are typed all day, and
// because the split is real: the root verbs act on THIS FOLDER's sandbox,
// while the group's verbs take --id and act on any. The aliases are hidden,
// which in this tree means found only by their exact spelling -- so the
// group's help still lists the --id verbs alone, and `veris sandbox u` does
// not become ambiguous.
//
// Each alias is a shallow copy with its own Usage. The copy shares the
// original's Flags and Run closures, and so the variables they bind; one
// command runs per process, so the two can never be in flight at once.
func folderAliases(cmds []*cli.Command) []*cli.Command {
	var aliases []*cli.Command
	for _, c := range cmds {
		if c.Name == "sandbox" {
			continue
		}
		alias := *c
		alias.Hidden = true
		alias.Usage = strings.Replace(alias.Usage, "veris "+c.Name, "veris sandbox "+c.Name, 1)
		aliases = append(aliases, &alias)
	}
	return aliases
}
