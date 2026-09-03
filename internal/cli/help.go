package cli

import (
	"flag"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Help renders a node's help text (used by Execute and by tests):
//
//	veris env - Named environments, chosen per folder
//
//	Usage:
//	  veris env <command> [flags]
//
//	Commands:
//	  create   Define a named environment
//	  ...
//
//	Flags:
//	  --json          machine-readable output on stdout
//	  --profile NAME  which login to use, by its NAME
//	  ...
//
// A leaf shows its Usage, its Help when it has one, then its own flags
// followed by the globals, all rendered from the FlagSet the command would
// parse with; a group lists its visible children instead, with its Help after
// the list. A RawArgs leaf lists no flags: it parses with a FlagSet of its
// own that this package never sees, and the globals are not among them.
//
// Help builds that FlagSet, which runs c.Flags and so writes every flag's
// default back into the variables it binds. Call it before parsing, or from
// a Run that has already copied what it needs; Execute itself renders from
// the FlagSet it parsed with.
func Help(path []string, c *Command, g *Globals) string {
	var fs *flag.FlagSet
	if !c.RawArgs {
		fs = newFlagSet(c, g)
	}
	return helpWith(path, c, fs)
}

// helpWith is Help rendered from an existing FlagSet, nil for a node whose
// flags this package does not see.
func helpWith(path []string, c *Command, fs *flag.FlagSet) string {
	var b strings.Builder
	name := strings.Join(path, " ")
	b.WriteString(name)
	if c.Summary != "" {
		b.WriteString(" - " + c.Summary)
	}
	b.WriteString("\n\nUsage:\n  " + usageLine(name, c) + "\n")
	// A leaf's prose follows its usage line; a group's follows its command
	// list, so the list stays next to the usage it explains and the prose
	// (what every command accepts, the exit table) reads as the footnote it is.
	if c.Help != "" && len(c.Sub) == 0 {
		b.WriteString("\n" + strings.TrimRight(c.Help, "\n") + "\n")
	}

	var listed []*Command
	width := 0
	for _, s := range c.Sub {
		if s.Hidden {
			continue
		}
		listed = append(listed, s)
		width = max(width, len(s.Name))
	}
	if len(listed) > 0 {
		b.WriteString("\nCommands:\n")
		for _, s := range listed {
			line := fmt.Sprintf("  %-*s   %s", width, s.Name, s.Summary)
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	if c.Help != "" && len(c.Sub) > 0 {
		b.WriteString("\n" + strings.TrimRight(c.Help, "\n") + "\n")
	}

	rows := flagRows(fs)
	if len(rows) > 0 {
		width = 0
		for _, r := range rows {
			width = max(width, len(r.name))
		}
		b.WriteString("\nFlags:\n")
		for _, r := range rows {
			line := fmt.Sprintf("  %-*s  %s", width, r.name, r.usage)
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	return b.String()
}

// usageLine is the command's Usage, or one derived from its place in the tree.
func usageLine(name string, c *Command) string {
	if c.Usage != "" {
		return c.Usage
	}
	switch {
	case len(c.Sub) > 0 && c.Run != nil:
		return name + " [<command>] [flags]"
	case len(c.Sub) > 0:
		return name + " <command> [flags]"
	}
	return name + " [flags]"
}

// flagRow is one line of the Flags section: the spellings of a flag joined
// (`-q, --quiet`), its value placeholder, and its usage text.
type flagRow struct {
	name   string
	usage  string
	global bool
	sortBy string
}

// flagRows renders the node's FlagSet: the command's own flags first, then the
// globals, each group by name. Two spellings bound to one variable -- the
// short and long form of the same switch -- share a row rather than repeating
// the same usage twice.
func flagRows(fs *flag.FlagSet) []flagRow {
	if fs == nil {
		return nil
	}
	globals := globalFlagNames()

	type group struct {
		names []string
		flag  *flag.Flag
	}
	var groups []*group
	byValue := map[any]*group{}
	fs.VisitAll(func(f *flag.Flag) {
		// Only a pointer identifies "the same variable"; anything else is its
		// own row. Interface keys must be comparable, and pointers always are.
		var key any = f
		if reflect.ValueOf(f.Value).Kind() == reflect.Pointer {
			key = f.Value
		}
		if grp, ok := byValue[key]; ok {
			grp.names = append(grp.names, f.Name)
			return
		}
		grp := &group{names: []string{f.Name}, flag: f}
		byValue[key] = grp
		groups = append(groups, grp)
	})

	rows := make([]flagRow, 0, len(groups))
	for _, grp := range groups {
		sort.Slice(grp.names, func(i, j int) bool {
			if len(grp.names[i]) != len(grp.names[j]) {
				return len(grp.names[i]) < len(grp.names[j])
			}
			return grp.names[i] < grp.names[j]
		})
		spellings := make([]string, 0, len(grp.names))
		for _, n := range grp.names {
			spellings = append(spellings, dashed(n))
		}
		placeholder, usage := flag.UnquoteUsage(grp.flag)
		name := strings.Join(spellings, ", ")
		if placeholder != "" {
			name += " " + strings.ToUpper(placeholder)
		}
		rows = append(rows, flagRow{
			name:   name,
			usage:  usage,
			global: globals[grp.flag.Name],
			sortBy: grp.names[len(grp.names)-1],
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].global != rows[j].global {
			return !rows[i].global
		}
		return rows[i].sortBy < rows[j].sortBy
	})
	return rows
}

// dashed is how a flag is written: one dash for a single letter, two otherwise.
func dashed(name string) string {
	if len(name) == 1 {
		return "-" + name
	}
	return "--" + name
}

// globalFlagNames is the set of names Globals.Bind registers, read off a
// scratch FlagSet so Help cannot drift from Bind.
func globalFlagNames() map[string]bool {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	(&Globals{}).Bind(fs)
	names := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) { names[f.Name] = true })
	return names
}
