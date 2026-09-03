package main

import (
	"bytes"
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/veris-ai/veris-proxy/internal/cli"
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
			stderrHas: []string{"Usage:\n  veris serve", "veris run", "Commands:\n  run",
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
	r.Sub = append(r.Sub, &cli.Command{
		Name:    "sandbox",
		Summary: "Running deployments",
		Run:     func(*cli.Context, []string) error { return nil },
	})
	var stdout, stderr bytes.Buffer
	err := cli.Execute(r, &cli.Globals{}, []string{"s"}, &stdout, &stderr)
	var ambiguous *cli.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("`veris s` returned %v, want *cli.AmbiguousError", err)
	}
	if want := "'s' is ambiguous — did you mean: sandbox, serve?"; err.Error() != want {
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
