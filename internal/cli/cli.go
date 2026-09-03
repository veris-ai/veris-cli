// Package cli is the veris command tree: a root Command whose Sub commands
// nest into groups and leaves, resolved word by word from the arguments.
//
// It exists so every command is found the same way (exact name, alias, or a
// unique prefix), prints help of the same shape, takes the global flags in the
// same places and fails with the same errors main.go's exit table already
// knows -- instead of a switch that each new command extends by hand. It sits
// on the standard flag package rather than a CLI framework: the proxy's own
// commands already parse with it, and what flag lacks (interspersed
// positionals, prefix matching) is small.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
)

// Globals are the flags every command accepts. They may appear before the
// first command word or among a leaf's own flags; either way they land here.
type Globals struct {
	Profile string // --profile NAME
	APIBase string // --api-base URL
	JSON    bool   // --json: stdout carries only the raw API body; progress stays on stderr
	Yes     bool   // --yes: answer every confirmation
	Quiet   bool   // -q, --quiet
}

// Bind registers the global flags on fs. Execute binds them on the root and on
// every leaf FlagSet, so globals may appear before or after the command words.
//
// Each default is the field's current value, not the zero value: a *Var
// binding writes its default into the variable at once, and the leaf binds
// after the root has already parsed `veris --json env ...`.
func (g *Globals) Bind(fs *flag.FlagSet) {
	fs.StringVar(&g.Profile, "profile", g.Profile, "which login to use, by its `NAME`")
	fs.StringVar(&g.APIBase, "api-base", g.APIBase, "the control plane's `URL`")
	fs.BoolVar(&g.JSON, "json", g.JSON, "machine-readable output on stdout")
	fs.BoolVar(&g.Yes, "yes", g.Yes, "answer every confirmation")
	fs.BoolVar(&g.Quiet, "quiet", g.Quiet, "print only failures and warnings")
	fs.BoolVar(&g.Quiet, "q", g.Quiet, "print only failures and warnings")
}

// Context is what a command's Run receives besides its positionals.
type Context struct {
	Globals *Globals
	Stdout  io.Writer
	Stderr  io.Writer
	Path    []string // canonical words from root to leaf, e.g. ["veris", "env", "create"]
}

// Command is one node of the tree: a group when it has Sub, a leaf otherwise.
type Command struct {
	Name    string   // canonical word, e.g. "create"
	Aliases []string // exact-match aliases only (no prefix matching on aliases)
	Summary string   // one line, shown in the parent's command list
	Usage   string   // e.g. "veris env create [NAME] [flags]"; derived from Path when empty
	Help    string   // longer text shown by this command's --help; optional

	// Flags binds this command's own flags; optional. The globals are bound
	// by Execute and must not be bound again here.
	Flags func(fs *flag.FlagSet)
	// Run receives the positionals left after flag parsing.
	Run func(ctx *Context, args []string) error
	// RawArgs passes the arguments untouched, with no FlagSet: for run, serve
	// and check, which parse their own and answer --help themselves.
	RawArgs bool

	Sub    []*Command
	Hidden bool // not listed in help; matches only by exact name
}

// UsageError is a command line that could not be understood: an unknown
// word, a bad flag, or no command at all. The relevant help has already been
// printed to stderr by the time it is returned; the caller maps it to exit 1.
type UsageError struct {
	Msg string
	Cmd *Command
}

func (e *UsageError) Error() string { return e.Msg }

// AmbiguousError is a prefix that names more than one command. Candidates are
// sorted, so the message reads the same however the tree was declared.
type AmbiguousError struct {
	Given      string
	Candidates []string
	Cmd        *Command
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("'%s' is ambiguous — did you mean: %s?",
		e.Given, strings.Join(e.Candidates, ", "))
}

// Execute resolves args against root and runs the leaf. Help that was asked
// for is printed to stdout and returns flag.ErrHelp (the caller maps it to
// exit 0, as main.go's exitStatus already does). A usage error prints the
// relevant node's help to stderr and returns *UsageError. Never calls os.Exit.
func Execute(root *Command, g *Globals, args []string, stdout, stderr io.Writer) error {
	if g == nil {
		g = &Globals{}
	}
	return execute(root, g, []string{root.Name}, args, stdout, stderr)
}

// execute is one step of the descent: it parses the flags that may precede
// the next word, then either runs this node or hands the rest to a child.
func execute(c *Command, g *Globals, path, args []string, stdout, stderr io.Writer) error {
	if len(c.Sub) == 0 {
		return executeLeaf(c, g, path, args, stdout, stderr)
	}

	// A group parses only up to its first word: the flags that belong to the
	// leaf are unknown here and must follow it. What a group accepts are the
	// globals (and its own flags, when it has a Run of its own).
	fs := newFlagSet(c, g)
	if err := fs.Parse(args); err != nil {
		return flagFailure(err, c, path, fs, stdout, stderr)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		if c.Run != nil {
			return c.Run(newContext(g, path, stdout, stderr), nil)
		}
		fmt.Fprint(stderr, helpWith(path, c, fs))
		return &UsageError{Msg: "no command given", Cmd: c}
	}
	if rest[0] == "help" {
		return showHelp(c, g, path, rest[1:], stdout, stderr)
	}
	child, err := match(c, rest[0])
	if err != nil {
		var usage *UsageError
		if errors.As(err, &usage) {
			fmt.Fprint(stderr, helpWith(path, c, fs))
		}
		return err
	}
	return execute(child, g, append(slices.Clone(path), child.Name), rest[1:], stdout, stderr)
}

// executeLeaf runs a command with no children. The word `help` is the
// tree's to answer at every leaf; a RawArgs leaf sees everything else as
// given, `--help` included, because it parses with a FlagSet of its own and
// already answers that spelling itself.
func executeLeaf(c *Command, g *Globals, path, args []string, stdout, stderr io.Writer) error {
	if c.Run == nil {
		return fmt.Errorf("%s has nothing to run", strings.Join(path, " "))
	}
	ctx := newContext(g, path, stdout, stderr)
	if len(args) > 0 && args[0] == "help" {
		fmt.Fprint(stdout, Help(path, c, g))
		return flag.ErrHelp
	}
	if c.RawArgs {
		return c.Run(ctx, args)
	}
	fs := newFlagSet(c, g)
	positional, err := ParseInterspersed(fs, args)
	if err != nil {
		return flagFailure(err, c, path, fs, stdout, stderr)
	}
	return c.Run(ctx, positional)
}

// showHelp answers the word `help`. Words after it name the node to describe
// (`veris help env create`), resolved the same way a command is; a word
// that looks like a flag ends the descent.
func showHelp(c *Command, g *Globals, path, words []string, stdout, stderr io.Writer) error {
	for _, w := range words {
		if len(c.Sub) == 0 || strings.HasPrefix(w, "-") {
			break
		}
		child, err := match(c, w)
		if err != nil {
			var usage *UsageError
			if errors.As(err, &usage) {
				fmt.Fprint(stderr, Help(path, c, g))
			}
			return err
		}
		c, path = child, append(slices.Clone(path), child.Name)
	}
	fmt.Fprint(stdout, Help(path, c, g))
	return flag.ErrHelp
}

// match picks the child a word names: an exact Name or Alias first, then the
// unique prefix among the visible children's Names. A hidden command is found
// only by its exact spelling, so it never turns a prefix ambiguous.
func match(c *Command, word string) (*Command, error) {
	for _, s := range c.Sub {
		if s.Name == word || slices.Contains(s.Aliases, word) {
			return s, nil
		}
	}
	var found []*Command
	if word != "" {
		for _, s := range c.Sub {
			if !s.Hidden && strings.HasPrefix(s.Name, word) {
				found = append(found, s)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, &UsageError{Msg: fmt.Sprintf("unknown command %q", word), Cmd: c}
	}
	names := make([]string, 0, len(found))
	for _, s := range found {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return nil, &AmbiguousError{Given: word, Candidates: names, Cmd: c}
}

// flagFailure turns what fs.Parse returned into the tree's answer: help that
// was asked for goes to stdout as flag.ErrHelp, a genuine flag error prints
// the node's help to stderr and keeps the flag package's own message. The
// help is rendered from the FlagSet that just parsed, not a fresh one: a
// fresh one would run the command's Flags again and reset every value the
// parse had already stored.
func flagFailure(err error, c *Command, path []string, fs *flag.FlagSet, stdout, stderr io.Writer) error {
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(stdout, helpWith(path, c, fs))
		return flag.ErrHelp
	}
	fmt.Fprint(stderr, helpWith(path, c, fs))
	return &UsageError{Msg: err.Error(), Cmd: c}
}

// newFlagSet is a node's FlagSet: its own flags, then the globals. The flag
// package's own reporting is silenced because Help renders the set instead,
// in one shape for every command.
func newFlagSet(c *Command, g *Globals) *flag.FlagSet {
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if c.Flags != nil {
		c.Flags(fs)
	}
	if g != nil {
		g.Bind(fs)
	}
	return fs
}

func newContext(g *Globals, path []string, stdout, stderr io.Writer) *Context {
	return &Context{Globals: g, Stdout: stdout, Stderr: stderr, Path: path}
}
