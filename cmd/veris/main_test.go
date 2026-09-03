package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/veris-ai/veris-cli/internal/cli"
)

// The dispatcher, as a script sees it: the binary re-invoked on an argv, its
// stdout, stderr and exit code held to the table in the root help. Each row
// is one shape of command line the switch this replaced used to answer.
func TestTheCommandTreeAnswersLikeTheDispatcherDid(t *testing.T) {
	cases := []struct {
		name       string
		argv       []string
		code       int
		stdoutHas  []string
		stderrHas  []string
		stdoutNone bool
		stderrNone bool
	}{
		{
			name: "no command is a usage error on stderr",
			argv: nil,
			code: 1,
			stderrHas: []string{"Usage:\n  veris login", "veris run", "Commands:\n  login",
				"Exit codes:", "veris: no command given\n"},
			stdoutNone: true,
		},
		{
			name:       "help is an answer on stdout",
			argv:       []string{"help"},
			code:       0,
			stdoutHas:  []string{"veris - route code under test", "Usage:", "Commands:", "Flags:"},
			stderrNone: true,
		},
		{
			name:       "--help too",
			argv:       []string{"--help"},
			code:       0,
			stdoutHas:  []string{"Usage:"},
			stderrNone: true,
		},
		{
			name:       "run --help is run's own usage",
			argv:       []string{"run", "--help"},
			code:       0,
			stdoutHas:  []string{"Usage of run:"},
			stderrNone: true,
		},
		{
			name:       "a prefix resolves to run",
			argv:       []string{"ru", "--help"},
			code:       0,
			stdoutHas:  []string{"Usage of run:"},
			stderrNone: true,
		},
		{
			name:       "a prefix resolves to run and hands the flags through",
			argv:       []string{"ru", "--sandbox", "x"},
			code:       1,
			stderrHas:  []string{"veris: run needs a command"},
			stdoutNone: true,
		},
		{
			name:       "a global before a raw command is refused, not dropped",
			argv:       []string{"-q", "run", "--sandbox", "x", "--", "true"},
			code:       1,
			stderrHas:  []string{"veris: --quiet before the command is not read by run", "veris run --help"},
			stdoutNone: true,
		},
		{
			name:       "the same global before a tree command is read",
			argv:       []string{"-q", "version"},
			code:       0,
			stdoutHas:  []string{version + "\n"},
			stderrNone: true,
		},
		{
			name:       "the help word after a raw command is the tree's help",
			argv:       []string{"run", "help"},
			code:       0,
			stdoutHas:  []string{"veris run - Run a command", "Usage:\n  veris run [--sandbox <id>]"},
			stderrNone: true,
		},
		{
			name:       "help of a raw command describes it without flags it does not take",
			argv:       []string{"help", "run"},
			code:       0,
			stdoutHas:  []string{"veris run - Run a command", "Usage:\n  veris run [--sandbox <id>]", "--image runs it in a container"},
			stderrNone: true,
		},
		{
			name:       "an unknown command is refused with the usage",
			argv:       []string{"nope"},
			code:       1,
			stderrHas:  []string{"Usage:", `veris: unknown command "nope"` + "\n"},
			stdoutNone: true,
		},
		{
			name:       "version prints only the version",
			argv:       []string{"version"},
			code:       0,
			stdoutHas:  []string{version + "\n"},
			stderrNone: true,
		},
		{
			name:       "--version still answers as it always did",
			argv:       []string{"--version"},
			code:       0,
			stdoutHas:  []string{version + "\n"},
			stderrNone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := invoke(t, tc.argv...)
			if code != tc.code {
				t.Errorf("%v exited %d, want %d (stderr: %q)", tc.argv, code, tc.code, stderr)
			}
			for _, want := range tc.stdoutHas {
				if !strings.Contains(stdout, want) {
					t.Errorf("%v: stdout lacks %q:\n%s", tc.argv, want, stdout)
				}
			}
			for _, want := range tc.stderrHas {
				if !strings.Contains(stderr, want) {
					t.Errorf("%v: stderr lacks %q:\n%s", tc.argv, want, stderr)
				}
			}
			if tc.stdoutNone && stdout != "" {
				t.Errorf("%v: nothing belongs on stdout, got %q", tc.argv, stdout)
			}
			if tc.stderrNone && stderr != "" {
				t.Errorf("%v: nothing belongs on stderr, got %q", tc.argv, stderr)
			}
			if tc.stdoutNone && tc.stderrNone {
				t.Fatalf("%v: a test that expects nothing anywhere checks nothing", tc.argv)
			}
		})
	}
}

// The tree's commands share no prefix today, so ambiguity is provoked with a
// sibling the test adds: `s` then names both serve and sandbox, and neither
// wins. The message lists the candidates sorted and the exit is 1, the same
// row of the table as an unknown command.
func TestAnAmbiguousPrefixNamesItsCandidates(t *testing.T) {
	r := root()
	var stdout, stderr bytes.Buffer
	err := cli.Execute(r, &cli.Globals{}, []string{"s"}, &stdout, &stderr)
	var ambiguous *cli.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("`veris s` returned %v, want *cli.AmbiguousError", err)
	}
	if want := "'s' is ambiguous — did you mean: sandbox, serve, snapshot, status?"; err.Error() != want {
		t.Errorf("message %q, want %q", err.Error(), want)
	}
	if stdout.Len() != 0 {
		t.Errorf("an ambiguous word must put nothing on stdout, got %q", stdout.String())
	}

	var report bytes.Buffer
	if code := exitStatusTo(&report, err); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if got := report.String(); got != "veris: "+err.Error()+"\n" {
		t.Errorf("reported %q", got)
	}
}

// exitStatus is held to the exit table in the root help, row by row.
func TestExitStatusFollowsTheTable(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   int
		stderr string
	}{
		{name: "nothing wrong", err: nil, code: 0, stderr: ""},
		{name: "help asked for", err: flag.ErrHelp, code: 0, stderr: ""},
		{name: "usage", err: &cli.UsageError{Msg: "no command given"}, code: 1, stderr: "veris: no command given\n"},
		{name: "ambiguous", err: &cli.AmbiguousError{Given: "s", Candidates: []string{"sandbox", "serve"}}, code: 1,
			stderr: "veris: 's' is ambiguous — did you mean: sandbox, serve?\n"},
		{name: "any other error", err: errors.New("boom"), code: 1, stderr: "veris: boom\n"},
		{name: "check failed", err: checkFailure{errors.New("not ours")}, code: 2, stderr: "veris: not ours\n"},
		{name: "requirement unmet", err: exitCode(exitRequirementUnmet), code: 3, stderr: ""},
		{name: "the child's own status", err: exitCode(7), code: 7, stderr: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := exitStatusTo(&stderr, tc.err); code != tc.code {
				t.Errorf("exit %d, want %d", code, tc.code)
			}
			if stderr.String() != tc.stderr {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.stderr)
			}
		})
	}
}

// scriptCase is one command line as a script sees it: the argv, the exit
// code, and what each stream must carry, must not carry, or must lack
// entirely.
type scriptCase struct {
	name       string
	argv       []string
	code       int
	stdoutHas  []string
	stderrHas  []string
	stdoutNot  []string
	stderrNot  []string
	stdoutNone bool
	stderrNone bool
}

// bareMachine runs the rest of the test on an empty HOME with every VERIS_*
// variable cleared, and returns a project-less working directory, so the
// machine running the tests cannot lend a child a login or a project file.
func bareMachine(t *testing.T) (dir string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, v := range []string{"VERIS_API_KEY", "VERIS_API_BASE", "VERIS_PROFILE", "VERIS_ENV",
		"VERIS_SANDBOX_ID", "VERIS_PROXY_CONFIG", "VERIS_ENVIRONMENT_ID"} {
		t.Setenv(v, "")
	}
	return t.TempDir()
}

// runScriptCases re-invokes the binary in dir for every case and holds its
// streams and exit code to the case.
func runScriptCases(t *testing.T, dir string, cases []scriptCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := invokeIn(t, dir, tc.argv...)
			if code != tc.code {
				t.Errorf("%v exited %d, want %d (stderr: %q)", tc.argv, code, tc.code, stderr)
			}
			for _, want := range tc.stdoutHas {
				if !strings.Contains(stdout, want) {
					t.Errorf("%v: stdout lacks %q:\n%s", tc.argv, want, stdout)
				}
			}
			for _, want := range tc.stderrHas {
				if !strings.Contains(stderr, want) {
					t.Errorf("%v: stderr lacks %q:\n%s", tc.argv, want, stderr)
				}
			}
			for _, bad := range tc.stdoutNot {
				if strings.Contains(stdout, bad) {
					t.Errorf("%v: stdout must not carry %q:\n%s", tc.argv, bad, stdout)
				}
			}
			for _, bad := range tc.stderrNot {
				if strings.Contains(stderr, bad) {
					t.Errorf("%v: stderr must not carry %q:\n%s", tc.argv, bad, stderr)
				}
			}
			if tc.stdoutNone && stdout != "" {
				t.Errorf("%v: nothing belongs on stdout, got %q", tc.argv, stdout)
			}
			if tc.stderrNone && stderr != "" {
				t.Errorf("%v: nothing belongs on stderr, got %q", tc.argv, stderr)
			}
		})
	}
}

// The groups Milestone 1 added, as a script sees them: help of a leaf and of
// a group on stdout, a group word with no verb refused with its usage, and a
// prefix chain (`e li`) resolving to env list and failing as env list fails,
// not as the tree does.
func TestTheNewGroupsAnswerAsAScriptSeesThem(t *testing.T) {
	dir := bareMachine(t)
	runScriptCases(t, dir, []scriptCase{
		{
			name:       "login --help is the leaf's help on stdout",
			argv:       []string{"login", "--help"},
			code:       0,
			stdoutHas:  []string{"veris login - Pair this machine", "Usage:\n  veris login [KEY] [--profile NAME]", "--key-stdin"},
			stderrNone: true,
		},
		{
			name:       "env --help lists the verbs",
			argv:       []string{"env", "--help"},
			code:       0,
			stdoutHas:  []string{"veris env - Named environments", "Commands:\n  create", "  list ", "  get ", "  use ", "  delete "},
			stderrNone: true,
		},
		{
			name:       "sandbox with no verb is a usage error on stderr",
			argv:       []string{"sandbox"},
			code:       1,
			stderrHas:  []string{"Usage:\n  veris sandbox <command>", "Commands:\n  get", "veris: no command given\n"},
			stdoutNone: true,
		},
		{
			name: "a prefix chain reaches env list, which fails as env list does",
			argv: []string{"e", "li"},
			code: 1,
			stderrHas: []string{"✗ Not logged in for profile 'default' (no API key)\n",
				"→ Next: veris login --profile default\n"},
			stderrNot:  []string{"panic", "Usage:", "unknown command", "ambiguous"},
			stdoutNone: true,
		},
	})
}

// The groups Milestone 2 added -- snapshot and baseline at the root, and
// services, data, trace and clock under sandbox -- as a script sees them:
// the root help names them, each group's help lists its verbs on stdout,
// a leaf's help carries its own flags, a group word with no verb is refused
// with its usage, and a prefix chain (`sn li`, `ba g`) reaches the leaf and
// fails as that leaf fails on a machine with no login and no project, not
// as the tree does. run's own usage lists the flags the proof added.
func TestTheMilestoneTwoGroupsAnswerAsAScriptSeesThem(t *testing.T) {
	dir := bareMachine(t)
	runScriptCases(t, dir, []scriptCase{
		{
			name: "the root help lists the new groups",
			argv: []string{"--help"},
			code: 0,
			stdoutHas: []string{
				"  veris sandbox   get|list|delete|reset|services|data|trace|clock|exports [--id ID]\n",
				"  veris snapshot  create|list|get|delete\n",
				"  veris baseline  get|promote|set|clear|list\n",
				"  veris run       [--sandbox <id>] [--fresh]",
				"  snapshot   Recorded worlds: create, list, get, delete\n",
				"  baseline   What every new sandbox boots: get, promote, set, clear, list\n",
				"veris snapshot,\nveris baseline",
			},
			stderrNone: true,
		},
		{
			name: "snapshot --help lists the verbs",
			argv: []string{"snapshot", "--help"},
			code: 0,
			stdoutHas: []string{"veris snapshot - Recorded worlds", "Usage:\n  veris snapshot <command>",
				"Commands:\n  create", "  list ", "  get ", "  delete ", "veris up --snapshot"},
			stderrNone: true,
		},
		{
			name: "baseline --help lists the verbs",
			argv: []string{"baseline", "--help"},
			code: 0,
			stdoutHas: []string{"veris baseline - What every new sandbox boots", "Usage:\n  veris baseline <command>",
				"Commands:\n  get", "  promote ", "  set ", "  clear ", "  list "},
			stderrNone: true,
		},
		{
			name: "sandbox --help lists the four new verbs after the lifecycle ones",
			argv: []string{"sandbox", "--help"},
			code: 0,
			stdoutHas: []string{"Commands:\n  get", "  reset ", "  services ", "  data ", "  trace ", "  clock ",
				"Usage:\n  veris sandbox <command> [--id ID]"},
			stderrNone: true,
		},
		{
			name: "sandbox services --help lists its verbs",
			argv: []string{"sandbox", "services", "--help"},
			code: 0,
			stdoutHas: []string{"veris sandbox services - The twins of a sandbox", "Usage:\n  veris sandbox services <command> [--id ID]",
				"Commands:\n  list", "  get ", "  manual "},
			stderrNone: true,
		},
		{
			name: "sandbox data --help lists its verbs",
			argv: []string{"sandbox", "data", "--help"},
			code: 0,
			stdoutHas: []string{"veris sandbox data - A sandbox's data", "Usage:\n  veris sandbox data <command> [--id ID]",
				"Commands:\n  schema", "  get ", "  add ", "GET /veris/schema"},
			stderrNone: true,
		},
		{
			name: "sandbox data add --help is the leaf's help with the file shape",
			argv: []string{"sandbox", "data", "add", "--help"},
			code: 0,
			stdoutHas: []string{"Usage:\n  veris sandbox data add FILE", "{twin: {table: [rows]}}",
				"Flags:\n  --id "},
			stderrNone: true,
		},
		{
			name: "sandbox trace --help is a leaf with its own flags",
			argv: []string{"sandbox", "trace", "--help"},
			code: 0,
			stdoutHas: []string{"veris sandbox trace - What the sandbox received, newest first",
				"Usage:\n  veris sandbox trace [--id ID] [--service NAME] [--tier handler|fault|control|delivery]",
				"  --body ", "  --follow ", "  --since ", "  --tier ", "  --limit "},
			stderrNone: true,
		},
		{
			name: "sandbox clock --help lists set and reads without a verb",
			argv: []string{"sandbox", "clock", "--help"},
			code: 0,
			stdoutHas: []string{"veris sandbox clock - The sandbox's shared virtual clock", "Usage:\n  veris sandbox clock [--id ID]",
				"Commands:\n  set "},
			stderrNone: true,
		},
		{
			name:       "sandbox clock set --help names the three ways to move time",
			argv:       []string{"sandbox", "clock", "set", "--help"},
			code:       0,
			stdoutHas:  []string{"Usage:\n  veris sandbox clock set (--freeze-at", "  --freeze-at ", "  --offset ", "  --live "},
			stderrNone: true,
		},
		{
			name:       "run --help carries the proof's flags",
			argv:       []string{"run", "--help"},
			code:       0,
			stdoutHas:  []string{"Usage of run:", "-fresh\n", "-keep\n", "-ttl minutes", "-receipt file", "-require-callback"},
			stderrNone: true,
		},
		{
			name:       "help run describes --fresh and the sandbox ledger",
			argv:       []string{"help", "run"},
			code:       0,
			stdoutHas:  []string{"Usage:\n  veris run [--sandbox <id>] [--fresh]", "--fresh deploys a sandbox", "--receipt PATH writes"},
			stderrNone: true,
		},
		{
			name:       "snapshot with no verb is a usage error on stderr",
			argv:       []string{"snapshot"},
			code:       1,
			stderrHas:  []string{"Usage:\n  veris snapshot <command>", "Commands:\n  create", "veris: no command given\n"},
			stdoutNone: true,
		},
		{
			name:       "sandbox data with no verb is a usage error on stderr",
			argv:       []string{"sandbox", "data"},
			code:       1,
			stderrHas:  []string{"Usage:\n  veris sandbox data <command>", "Commands:\n  schema", "veris: no command given\n"},
			stdoutNone: true,
		},
		{
			name:       "a prefix chain reaches snapshot list, which fails as snapshot list does",
			argv:       []string{"sn", "li"},
			code:       1,
			stderrHas:  []string{"✗ No environment selected\n", "→ Next: veris env use NAME, or pass --env\n"},
			stderrNot:  []string{"panic", "Usage:", "unknown command", "ambiguous"},
			stdoutNone: true,
		},
		{
			name:       "a prefix chain reaches baseline get, which fails as baseline get does",
			argv:       []string{"ba", "g"},
			code:       1,
			stderrHas:  []string{"✗ No environment selected\n", "→ Next: veris env use NAME, or pass --env\n"},
			stderrNot:  []string{"panic", "Usage:", "unknown command", "ambiguous"},
			stdoutNone: true,
		},
	})
}

// The leaves Milestone 3 added or changed -- --watch on up, status and
// sandbox get; sandbox exports and get --exports; the doctor and version
// polish; --expose in run's host tier -- as a script sees them: the root
// help names them, each leaf's help is on stdout with its flags, and a
// prefix chain reaches the new leaves and fails as they fail on a machine
// with no login and no project, not as the tree does.
func TestTheMilestoneThreeLeavesAnswerAsAScriptSeesThem(t *testing.T) {
	dir := bareMachine(t)
	runScriptCases(t, dir, []scriptCase{
		{
			name: "the root help lists the new leaves and flags",
			argv: []string{"--help"},
			code: 0,
			stdoutHas: []string{
				"  veris up        [NAME] [--ttl N] [--boot bundle|baseline|snapshot] [--watch]\n",
				"  veris status    [--watch] [--json]\n",
				"  veris sandbox   get|list|delete|reset|services|data|trace|clock|exports [--id ID]\n",
				"[--expose PORT] [--require-service <n>] [--require-callback <path>]",
				"  veris doctor    [--env NAME] [--json]\n",
				"up --watch and status --watch keep a live panel",
				"every get and list takes --json",
			},
			stderrNone: true,
		},
		{
			name:       "up --help carries --watch",
			argv:       []string{"up", "--help"},
			code:       0,
			stdoutHas:  []string{"Usage:\n  veris up [NAME | --env NAME]", "[--watch] [--json]", "  --watch "},
			stderrNone: true,
		},
		{
			name:       "status --help carries --watch",
			argv:       []string{"status", "--help"},
			code:       0,
			stdoutHas:  []string{"Usage:\n  veris status [--watch] [--json]", "  --watch "},
			stderrNone: true,
		},
		{
			name:       "sandbox --help lists exports after the milestone 2 verbs",
			argv:       []string{"sandbox", "--help"},
			code:       0,
			stdoutHas:  []string{"Commands:\n  get", "  clock ", "  exports "},
			stderrNone: true,
		},
		{
			name: "sandbox get --help carries --watch and --exports together",
			argv: []string{"sandbox", "get", "--help"},
			code: 0,
			stdoutHas: []string{"Usage:\n  veris sandbox get [--id ID] [--watch] [--json] [--exports [--format env|dotenv|json]]",
				"  --exports ", "  --format ", "  --id ", "  --watch "},
			stderrNone: true,
		},
		{
			name:       "sandbox exports --help is a leaf with its format flag",
			argv:       []string{"sandbox", "exports", "--help"},
			code:       0,
			stdoutHas:  []string{"veris sandbox exports - ", "Usage:\n  veris sandbox exports [--id ID] [--format env|dotenv|json]", "  --format ", "  --id "},
			stderrNone: true,
		},
		{
			name:       "doctor --help carries --env and the globals",
			argv:       []string{"doctor", "--help"},
			code:       0,
			stdoutHas:  []string{"veris doctor - ", "Usage:\n  veris doctor [--env NAME] [--json]", "  --env ", "  --json "},
			stderrNone: true,
		},
		{
			name:       "version --help names --json",
			argv:       []string{"version", "--help"},
			code:       0,
			stdoutHas:  []string{"veris version - ", "Usage:\n  veris version [--json]", "  --json "},
			stderrNone: true,
		},
		{
			name:       "run --help carries the host tier's tunnel flags",
			argv:       []string{"run", "--help"},
			code:       0,
			stdoutHas:  []string{"Usage of run:", "-expose port", "-expose-hostname hostname", "-expose-token token", "-require-callback", "veris serve --expose"},
			stderrNone: true,
		},
		{
			name:       "help run describes --expose composing serve in the host tier",
			argv:       []string{"help", "run"},
			code:       0,
			stdoutHas:  []string{"Usage:\n  veris run [--sandbox <id>] [--fresh]", "[--expose PORT]", "veris serve\n--expose", "VERIS_PUBLIC_URL", "--environment and --ttl-minutes stay with --image"},
			stderrNone: true,
		},
		{
			name:       "a prefix chain reaches sandbox exports, which fails as it does",
			argv:       []string{"sa", "ex"},
			code:       1,
			stderrHas:  []string{"✗ "},
			stderrNot:  []string{"panic", "Usage:", "unknown command", "ambiguous"},
			stdoutNone: true,
		},
	})
}
