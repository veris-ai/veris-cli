package cli

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"
)

// call is what a test tree's leaf records when it runs.
type call struct {
	path    string
	args    []string
	globals Globals
	ttl     int
	name    string
}

// testTree is a small tree with every shape the resolver must handle: groups,
// aliases, a hidden command, RawArgs leaves, and two visible siblings sharing
// a prefix. `serve` is declared before `sandbox` so a sorted candidate list is
// observable.
func testTree(t *testing.T, got *call) *Command {
	t.Helper()
	record := func(c *call, ctx *Context, args []string) error {
		c.path = strings.Join(ctx.Path, " ")
		c.args = args
		c.globals = *ctx.Globals
		return nil
	}
	leaf := func(name, summary string, aliases ...string) *Command {
		return &Command{
			Name: name, Aliases: aliases, Summary: summary,
			Run: func(ctx *Context, args []string) error { return record(got, ctx, args) },
		}
	}
	raw := func(name, summary string) *Command {
		c := leaf(name, summary)
		c.RawArgs = true
		return c
	}
	var ttl int
	var name string
	create := &Command{
		Name: "create", Summary: "Define a named environment",
		Help: "Creates the environment on the control plane.",
		Flags: func(fs *flag.FlagSet) {
			fs.IntVar(&ttl, "ttl", 0, "sandbox lifetime in `MINUTES`")
			fs.StringVar(&name, "name", "", "display name")
		},
		Run: func(ctx *Context, args []string) error {
			got.ttl, got.name = ttl, name
			return record(got, ctx, args)
		},
	}
	return &Command{
		Name:    "veris",
		Summary: "route code under test at a Veris dependency sandbox",
		Sub: []*Command{
			{
				Name: "env", Aliases: []string{"environment"},
				Summary: "Named environments, chosen per folder",
				Sub: []*Command{
					create,
					leaf("list", "Show configured and available environments", "ls"),
					leaf("use", "Choose the environment for this folder"),
				},
			},
			raw("serve", "Run the proxy"),
			{
				Name: "sandbox", Summary: "Running deployments",
				Sub: []*Command{
					leaf("create", "Deploy one"),
					leaf("delete", "Tear one down", "rm"),
				},
			},
			raw("run", "Run a command against a sandbox"),
			raw("check", "Assert a live proxy is ours"),
			leaf("version", "Print the version"),
			func() *Command {
				c := leaf("secret", "Not listed")
				c.Hidden = true
				return c
			}(),
		},
	}
}

func TestExecute(t *testing.T) {
	type want struct {
		path    string   // leaf that ran, "" when none
		args    []string // positionals it received
		ttl     int
		globals Globals
		help    bool   // flag.ErrHelp returned
		usage   string // *UsageError message
		ambig   string // *AmbiguousError message
		stdout  string // substring expected on stdout
		stderr  string // substring expected on stderr
		noOut   bool   // stdout must be empty
		noErr   bool   // stderr must be empty
	}
	cases := []struct {
		name string
		args []string
		want want
	}{
		// resolution
		{"exact", []string{"env", "list"}, want{path: "veris env list", args: nil, noOut: true, noErr: true}},
		{"exact positional", []string{"env", "create", "staging"}, want{path: "veris env create", args: []string{"staging"}}},
		{"alias at group", []string{"environment", "list"}, want{path: "veris env list"}},
		{"alias at leaf", []string{"env", "ls"}, want{path: "veris env list"}},
		{"alias on a second child", []string{"sandbox", "rm", "x"}, want{path: "veris sandbox delete", args: []string{"x"}}},
		{"prefix at two levels", []string{"en", "cr", "staging"}, want{path: "veris env create", args: []string{"staging"}}},
		{"prefix single letter", []string{"v"}, want{path: "veris version"}},
		{"prefix ignores hidden", []string{"se"}, want{path: "veris serve"}},
		{"hidden by exact name", []string{"secret"}, want{path: "veris secret"}},
		{"no prefix on aliases", []string{"env", "l"}, want{path: "veris env list"}},
		{"ambiguous sorted", []string{"s"}, want{ambig: "'s' is ambiguous — did you mean: sandbox, serve?", noOut: true, noErr: true}},
		{"prefix at group", []string{"env", "u"}, want{path: "veris env use"}},
		{"unknown", []string{"bogus"}, want{usage: `unknown command "bogus"`, stderr: "veris - route code under test", noOut: true}},
		{"unknown at group", []string{"env", "bogus"}, want{usage: `unknown command "bogus"`, stderr: "veris env - Named environments", noOut: true}},
		{"empty word", []string{""}, want{usage: `unknown command ""`, stderr: "Commands:"}},
		{"no command at root", nil, want{usage: "no command given", stderr: "Usage:\n  veris <command> [flags]", noOut: true}},
		{"no command at group", []string{"env"}, want{usage: "no command given", stderr: "Usage:\n  veris env <command> [flags]", noOut: true}},

		// help, three spellings, three depths
		{"help word root", []string{"help"}, want{help: true, stdout: "veris - route code under test", noErr: true}},
		{"--help root", []string{"--help"}, want{help: true, stdout: "veris - route code under test", noErr: true}},
		{"-h root", []string{"-h"}, want{help: true, stdout: "veris - route code under test", noErr: true}},
		{"help word group", []string{"env", "help"}, want{help: true, stdout: "veris env - Named environments", noErr: true}},
		{"--help group", []string{"env", "--help"}, want{help: true, stdout: "veris env - Named environments", noErr: true}},
		{"-h group", []string{"env", "-h"}, want{help: true, stdout: "veris env - Named environments", noErr: true}},
		{"help word leaf", []string{"env", "create", "help"}, want{help: true, stdout: "veris env create - Define", noErr: true}},
		{"--help leaf", []string{"env", "create", "--help"}, want{help: true, stdout: "veris env create - Define", noErr: true}},
		{"-h leaf", []string{"env", "create", "-h"}, want{help: true, stdout: "veris env create - Define", noErr: true}},
		{"--help leaf after positional", []string{"env", "create", "staging", "--help"}, want{help: true, stdout: "veris env create - Define"}},
		{"help descends", []string{"help", "env", "cr"}, want{help: true, stdout: "veris env create - Define", noErr: true}},
		{"help descends to unknown", []string{"help", "nope"}, want{usage: `unknown command "nope"`, stderr: "veris - route code", noOut: true}},
		{"help of a raw leaf", []string{"help", "run"}, want{help: true, stdout: "veris run - Run a command", noErr: true}},

		// RawArgs: everything through, --help included
		{"raw passthrough", []string{"run", "--sandbox", "x", "--", "pytest", "-q"}, want{path: "veris run", args: []string{"--sandbox", "x", "--", "pytest", "-q"}}},
		{"raw --help", []string{"run", "--help"}, want{path: "veris run", args: []string{"--help"}, noOut: true, noErr: true}},
		{"raw -h", []string{"check", "-h"}, want{path: "veris check", args: []string{"-h"}, noOut: true}},
		{"raw help word is the tree's", []string{"serve", "help"}, want{help: true, stdout: "veris serve - Run the proxy", noErr: true}},
		{"raw by prefix", []string{"ru", "--sandbox", "x"}, want{path: "veris run", args: []string{"--sandbox", "x"}}},
		{"raw bogus flag passes", []string{"run", "--bogus"}, want{path: "veris run", args: []string{"--bogus"}, noErr: true}},

		// globals before and after the words
		{"globals before", []string{"--json", "--profile", "dev", "env", "create", "x"}, want{path: "veris env create", args: []string{"x"}, globals: Globals{JSON: true, Profile: "dev"}}},
		{"globals between", []string{"env", "--profile=dev", "create", "x"}, want{path: "veris env create", args: []string{"x"}, globals: Globals{Profile: "dev"}}},
		{"globals after", []string{"env", "create", "x", "--json", "--yes", "-q"}, want{path: "veris env create", args: []string{"x"}, globals: Globals{JSON: true, Yes: true, Quiet: true}}},
		{"globals both sides", []string{"--profile", "dev", "env", "create", "--json", "x"}, want{path: "veris env create", args: []string{"x"}, globals: Globals{JSON: true, Profile: "dev"}}},
		{"globals before a raw leaf", []string{"--api-base", "http://x", "run", "--sandbox", "s"}, want{path: "veris run", args: []string{"--sandbox", "s"}, globals: Globals{APIBase: "http://x"}}},
		{"--quiet long form", []string{"--quiet", "version"}, want{path: "veris version", globals: Globals{Quiet: true}}},
		{"leaf flag at group is an error", []string{"env", "--ttl", "20", "create"}, want{usage: "flag provided but not defined: -ttl", stderr: "veris env - Named environments", noOut: true}},

		// interspersed leaf flags and --
		{"leaf flags after positional", []string{"env", "create", "staging", "--ttl", "20"}, want{path: "veris env create", args: []string{"staging"}, ttl: 20}},
		{"leaf flags before positional", []string{"env", "create", "--ttl", "20", "staging"}, want{path: "veris env create", args: []string{"staging"}, ttl: 20}},
		{"leaf flags around positionals", []string{"env", "create", "a", "--ttl=5", "b", "--json", "c"}, want{path: "veris env create", args: []string{"a", "b", "c"}, ttl: 5, globals: Globals{JSON: true}}},
		{"double dash stops parsing", []string{"env", "create", "staging", "--ttl", "20", "--", "--json", "-h", "x"}, want{path: "veris env create", args: []string{"staging", "--json", "-h", "x"}, ttl: 20, noOut: true}},
		{"double dash first", []string{"env", "create", "--", "--ttl"}, want{path: "veris env create", args: []string{"--ttl"}, ttl: 0}},

		// flag errors keep the flag package's message
		{"bad flag on leaf", []string{"env", "create", "--bogus"}, want{usage: "flag provided but not defined: -bogus", stderr: "veris env create - Define", noOut: true}},
		{"missing value on leaf", []string{"env", "create", "x", "--ttl"}, want{usage: "flag needs an argument: -ttl", stderr: "Flags:", noOut: true}},
		{"bad value on leaf", []string{"env", "create", "--ttl", "soon"}, want{usage: `invalid value "soon" for flag -ttl: parse error`, stderr: "veris env create"}},
		{"bad flag at root", []string{"--bogus", "version"}, want{usage: "flag provided but not defined: -bogus", stderr: "veris - route code", noOut: true}},
		{"missing global value at root", []string{"--profile"}, want{usage: "flag needs an argument: -profile", stderr: "Usage:", noOut: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got call
			var stdout, stderr bytes.Buffer
			g := &Globals{}
			err := Execute(testTree(t, &got), g, tc.args, &stdout, &stderr)

			switch {
			case tc.want.help:
				if !errors.Is(err, flag.ErrHelp) {
					t.Fatalf("err = %v, want flag.ErrHelp", err)
				}
			case tc.want.usage != "":
				var ue *UsageError
				if !errors.As(err, &ue) {
					t.Fatalf("err = %v (%T), want *UsageError", err, err)
				}
				if ue.Msg != tc.want.usage || ue.Error() != tc.want.usage {
					t.Fatalf("usage error = %q, want %q", ue.Msg, tc.want.usage)
				}
				if ue.Cmd == nil {
					t.Fatal("UsageError.Cmd is nil")
				}
			case tc.want.ambig != "":
				var ae *AmbiguousError
				if !errors.As(err, &ae) {
					t.Fatalf("err = %v (%T), want *AmbiguousError", err, err)
				}
				if ae.Error() != tc.want.ambig {
					t.Fatalf("ambiguous error = %q, want %q", ae.Error(), tc.want.ambig)
				}
				if ae.Cmd == nil {
					t.Fatal("AmbiguousError.Cmd is nil")
				}
			default:
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
			}

			if got.path != tc.want.path {
				t.Errorf("ran %q, want %q", got.path, tc.want.path)
			}
			if tc.want.path != "" {
				if strings.Join(got.args, "\x00") != strings.Join(tc.want.args, "\x00") {
					t.Errorf("args = %q, want %q", got.args, tc.want.args)
				}
				if got.globals != tc.want.globals {
					t.Errorf("globals = %+v, want %+v", got.globals, tc.want.globals)
				}
				if got.ttl != tc.want.ttl {
					t.Errorf("ttl = %d, want %d", got.ttl, tc.want.ttl)
				}
			}
			if tc.want.stdout != "" && !strings.Contains(stdout.String(), tc.want.stdout) {
				t.Errorf("stdout lacks %q:\n%s", tc.want.stdout, stdout.String())
			}
			if tc.want.stderr != "" && !strings.Contains(stderr.String(), tc.want.stderr) {
				t.Errorf("stderr lacks %q:\n%s", tc.want.stderr, stderr.String())
			}
			if tc.want.noOut && stdout.Len() > 0 {
				t.Errorf("stdout should be empty, got:\n%s", stdout.String())
			}
			if tc.want.noErr && stderr.Len() > 0 {
				t.Errorf("stderr should be empty, got:\n%s", stderr.String())
			}
		})
	}
}

// Globals bound on the leaf must keep what the root already parsed: the
// pointer handed to Execute is the one the leaf reads.
func TestExecuteGlobalsSharedPointer(t *testing.T) {
	var got call
	g := &Globals{Profile: "preset"}
	err := Execute(testTree(t, &got), g, []string{"--json", "env", "create", "--yes", "x"}, new(bytes.Buffer), new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	want := Globals{Profile: "preset", JSON: true, Yes: true}
	if *g != want {
		t.Errorf("globals = %+v, want %+v", *g, want)
	}
	if got.globals != want {
		t.Errorf("leaf saw %+v, want %+v", got.globals, want)
	}
}

func TestExecuteNilGlobals(t *testing.T) {
	var got call
	if err := Execute(testTree(t, &got), nil, []string{"--json", "version"}, new(bytes.Buffer), new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if !got.globals.JSON {
		t.Error("nil Globals: --json was lost")
	}
}

func TestExecuteGroupWithRun(t *testing.T) {
	var ran []string
	root := &Command{
		Name: "veris",
		Sub: []*Command{{
			Name:  "env",
			Flags: func(fs *flag.FlagSet) { fs.Bool("all", false, "every one") },
			Run: func(ctx *Context, args []string) error {
				ran = append(ran, "env:"+strings.Join(args, ","))
				return nil
			},
			Sub: []*Command{{
				Name: "list",
				Run: func(ctx *Context, args []string) error {
					ran = append(ran, "list")
					return nil
				},
			}},
		}},
	}
	var stdout, stderr bytes.Buffer
	if err := Execute(root, &Globals{}, []string{"env", "--all"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if err := Execute(root, &Globals{}, []string{"env", "li"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ran, " "); got != "env: list" {
		t.Errorf("ran %q, want %q", got, "env: list")
	}
	if strings.Contains(Help([]string{"veris", "env"}, root.Sub[0], &Globals{}), "veris env <command>") {
		t.Error("a group with its own Run should show the command as optional")
	}
}

func TestExecuteRunError(t *testing.T) {
	boom := errors.New("boom")
	root := &Command{Name: "veris", Sub: []*Command{{
		Name: "fail",
		Run:  func(ctx *Context, args []string) error { return boom },
	}}}
	err := Execute(root, &Globals{}, []string{"fail"}, new(bytes.Buffer), new(bytes.Buffer))
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the command's own", err)
	}
}

func TestExecuteLeafWithoutRun(t *testing.T) {
	root := &Command{Name: "veris", Sub: []*Command{{Name: "stub"}}}
	err := Execute(root, &Globals{}, []string{"stub"}, new(bytes.Buffer), new(bytes.Buffer))
	if err == nil || !strings.Contains(err.Error(), "veris stub has nothing to run") {
		t.Errorf("err = %v, want 'has nothing to run'", err)
	}
}

func TestHelpGroup(t *testing.T) {
	var got call
	root := testTree(t, &got)
	want := `veris env - Named environments, chosen per folder

Usage:
  veris env <command> [flags]

Commands:
  create   Define a named environment
  list     Show configured and available environments
  use      Choose the environment for this folder

Flags:
  --api-base URL  the control plane's URL
  --json          machine-readable output on stdout
  --profile NAME  which login to use, by its NAME
  -q, --quiet     print only failures and warnings
  --yes           answer every confirmation
`
	if got := Help([]string{"veris", "env"}, root.Sub[0], &Globals{}); got != want {
		t.Errorf("help =\n%s\nwant\n%s", got, want)
	}
}

func TestHelpLeaf(t *testing.T) {
	var got call
	root := testTree(t, &got)
	create := root.Sub[0].Sub[0]
	want := `veris env create - Define a named environment

Usage:
  veris env create [flags]

Creates the environment on the control plane.

Flags:
  --name STRING   display name
  --ttl MINUTES   sandbox lifetime in MINUTES
  --api-base URL  the control plane's URL
  --json          machine-readable output on stdout
  --profile NAME  which login to use, by its NAME
  -q, --quiet     print only failures and warnings
  --yes           answer every confirmation
`
	if got := Help([]string{"veris", "env", "create"}, create, &Globals{}); got != want {
		t.Errorf("help =\n%s\nwant\n%s", got, want)
	}
}

func TestHelpRoot(t *testing.T) {
	var got call
	root := testTree(t, &got)
	h := Help([]string{"veris"}, root, &Globals{})
	for _, line := range []string{
		"veris - route code under test at a Veris dependency sandbox\n",
		"\nUsage:\n  veris <command> [flags]\n",
		"\nCommands:\n  env       Named environments, chosen per folder\n  serve     Run the proxy\n  sandbox   Running deployments\n",
		"  version   Print the version\n\nFlags:\n",
	} {
		if !strings.Contains(h, line) {
			t.Errorf("root help lacks %q:\n%s", line, h)
		}
	}
	if strings.Contains(h, "secret") {
		t.Errorf("root help lists a hidden command:\n%s", h)
	}
}

func TestHelpExplicitUsageAndNoGlobals(t *testing.T) {
	c := &Command{Name: "run", Summary: "Run it", Usage: "veris run [flags] -- <cmd>", RawArgs: true}
	got := Help([]string{"veris", "run"}, c, nil)
	want := "veris run - Run it\n\nUsage:\n  veris run [flags] -- <cmd>\n"
	if got != want {
		t.Errorf("help = %q, want %q", got, want)
	}
	bare := Help([]string{"veris", "x"}, &Command{Name: "x"}, nil)
	if bare != "veris x\n\nUsage:\n  veris x [flags]\n" {
		t.Errorf("bare help = %q", bare)
	}
}

// Rendering help must not disturb flags a leaf has already parsed: Execute
// renders from the FlagSet it parsed with, so a Run that fails on a missing
// positional can print the leaf's help and still report the --ttl it was
// given. The public Help, which builds a fresh FlagSet, is documented as
// resetting them.
func TestHelpFromParsedFlagSetKeepsValues(t *testing.T) {
	var ttl int
	c := &Command{
		Name:  "create",
		Flags: func(fs *flag.FlagSet) { fs.IntVar(&ttl, "ttl", 0, "lifetime in `MINUTES`") },
		Run: func(ctx *Context, args []string) error {
			return &UsageError{Msg: "no name given", Cmd: nil}
		},
	}
	root := &Command{Name: "veris", Sub: []*Command{c}}
	g := &Globals{}
	// A flag error after a parsed value: the help printed on stderr must
	// come from the parsed FlagSet, and ttl must survive.
	var stderr bytes.Buffer
	err := Execute(root, g, []string{"create", "--ttl", "9", "--bogus"}, new(bytes.Buffer), &stderr)
	var ue *UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *UsageError", err)
	}
	if ttl != 9 {
		t.Errorf("ttl = %d after rendering help on a flag error, want 9", ttl)
	}
	if !strings.Contains(stderr.String(), "--ttl MINUTES") {
		t.Errorf("stderr lacks the leaf's flags:\n%s", stderr.String())
	}
	// Help itself rebinds and so resets; that is the documented cost.
	ttl = 9
	_ = Help([]string{"veris", "create"}, c, g)
	if ttl != 0 {
		t.Errorf("Help left ttl = %d; it is documented to rebind the defaults", ttl)
	}
}

func TestParseInterspersed(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     []string
		ttl      int
		label    string
		json     bool
		err      string
		wantHelp bool
	}{
		{name: "empty", args: nil, want: nil},
		{name: "positionals only", args: []string{"a", "b"}, want: []string{"a", "b"}},
		{name: "flags first", args: []string{"--ttl", "20", "a"}, want: []string{"a"}, ttl: 20},
		{name: "flag between", args: []string{"a", "--ttl", "20", "b"}, want: []string{"a", "b"}, ttl: 20},
		{name: "flag last", args: []string{"a", "b", "--ttl=7"}, want: []string{"a", "b"}, ttl: 7},
		{name: "bool then positional", args: []string{"--json", "a"}, want: []string{"a"}, json: true},
		{name: "terminator", args: []string{"a", "--", "--ttl", "20"}, want: []string{"a", "--ttl", "20"}},
		{name: "terminator first", args: []string{"--", "a", "-h"}, want: []string{"a", "-h"}},
		{name: "terminator last", args: []string{"--ttl", "20", "--"}, want: nil, ttl: 20},
		{name: "terminator only", args: []string{"--"}, want: nil},
		{name: "double dash as a value", args: []string{"--label", "--", "x", "--ttl", "3"}, want: []string{"x"}, label: "--", ttl: 3},
		{name: "value then terminator", args: []string{"--label", "--", "--", "--ttl", "3"}, want: []string{"--ttl", "3"}, label: "--"},
		{name: "value by equals", args: []string{"--label=--", "x"}, want: []string{"x"}, label: "--"},
		{name: "value spelled like a flag then terminator", args: []string{"--label", "--label", "--", "x", "--ttl", "3"}, want: []string{"x", "--ttl", "3"}, label: "--label"},
		{name: "value spelled like the terminator's neighbour", args: []string{"--label", "--ttl", "--", "--ttl", "3"}, want: []string{"--ttl", "3"}, label: "--ttl"},
		{name: "positional then value then terminator", args: []string{"a", "--label", "--", "--", "b"}, want: []string{"a", "b"}, label: "--"},
		{name: "lone dash is positional", args: []string{"-", "--ttl", "1"}, want: []string{"-"}, ttl: 1},
		{name: "unknown flag", args: []string{"a", "--bogus"}, err: "flag provided but not defined: -bogus"},
		{name: "missing value", args: []string{"a", "--ttl"}, err: "flag needs an argument: -ttl"},
		{name: "help", args: []string{"a", "-h"}, wantHelp: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(new(bytes.Buffer))
			ttl := fs.Int("ttl", 0, "")
			label := fs.String("label", "", "")
			json := fs.Bool("json", false, "")
			got, err := ParseInterspersed(fs, tc.args)
			switch {
			case tc.wantHelp:
				if !errors.Is(err, flag.ErrHelp) {
					t.Fatalf("err = %v, want flag.ErrHelp", err)
				}
				return
			case tc.err != "":
				if err == nil || err.Error() != tc.err {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("positionals = %q, want %q", got, tc.want)
			}
			if *ttl != tc.ttl || *label != tc.label || *json != tc.json {
				t.Errorf("ttl=%d label=%q json=%v, want %d %q %v", *ttl, *label, *json, tc.ttl, tc.label, tc.json)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	ue := &UsageError{Msg: "no command given"}
	if ue.Error() != "no command given" {
		t.Errorf("UsageError = %q", ue.Error())
	}
	ae := &AmbiguousError{Given: "sb", Candidates: []string{"sandbox", "serve"}}
	if want := "'sb' is ambiguous — did you mean: sandbox, serve?"; ae.Error() != want {
		t.Errorf("AmbiguousError = %q, want %q", ae.Error(), want)
	}
}

func TestGlobalsBindKeepsValues(t *testing.T) {
	g := &Globals{Profile: "dev", JSON: true}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	g.Bind(fs)
	if g.Profile != "dev" || !g.JSON {
		t.Errorf("Bind reset the globals: %+v", *g)
	}
	if err := fs.Parse([]string{"-q", "--api-base", "http://x/"}); err != nil {
		t.Fatal(err)
	}
	if !g.Quiet || g.APIBase != "http://x/" || g.Profile != "dev" {
		t.Errorf("after parse: %+v", *g)
	}
}
