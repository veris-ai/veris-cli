package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
)

// exportsBench is the status bench: a logged-in profile against a fake
// plane, this folder pointing at sbID, and the plane answering it ready
// with the stripe and postgres twins.
func exportsBench(t *testing.T) (*bench, *sandboxPlane, *sandboxTwins) {
	t.Helper()
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(true), time.Now().Add(time.Hour)) }
	})
	return b, plane, twins
}

func TestExportsFormats(t *testing.T) {
	_, _, twins := exportsBench(t)
	stripe := twins.srv.URL + "/s/" + sbID + "/stripe"
	pg := "postgresql://app:app@10.0.0.5:5432/sb?sslmode=require"

	wantEnv := "export STRIPE_API_BASE='" + stripe + "'\nexport DATABASE_URL='" + pg + "'\n"
	wantDotenv := "STRIPE_API_BASE=" + stripe + "\nDATABASE_URL=" + pg + "\n"
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"exports", []string{"sandbox", "exports"}, wantEnv},
		{"exports --format env", []string{"sandbox", "exports", "--format", "env"}, wantEnv},
		{"exports --format dotenv", []string{"sandbox", "exports", "--format", "dotenv"}, wantDotenv},
		{"exports --id", []string{"sandbox", "exports", "--id", sbID}, wantEnv},
		{"get --exports", []string{"sandbox", "get", "--exports"}, wantEnv},
		{"get --exports --format dotenv", []string{"sandbox", "get", "--exports", "--format", "dotenv"}, wantDotenv},
		{"get --id --exports", []string{"sandbox", "get", "--id", sbID, "--exports"}, wantEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runSandboxCLI(t, tc.argv...)
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, stderr)
			}
			if stdout != tc.want {
				t.Errorf("stdout:\n%s\nwant:\n%s", stdout, tc.want)
			}
			// eval "$(…)" reads stdout alone, and nothing else belongs there;
			// nothing was worth saying on stderr either.
			if stderr != "" {
				t.Errorf("stderr should be empty, got:\n%s", stderr)
			}
		})
	}

	for _, argv := range [][]string{
		{"sandbox", "exports", "--format", "json"},
		{"sandbox", "exports", "--json"},
		{"sandbox", "get", "--exports", "--json"},
		{"sandbox", "get", "--exports", "--format", "json"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			code, stdout, stderr := runSandboxCLI(t, argv...)
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			var body map[string]string
			if err := json.Unmarshal([]byte(stdout), &body); err != nil {
				t.Fatalf("stdout is not one object: %v\n%s", err, stdout)
			}
			if len(body) != 2 || body["STRIPE_API_BASE"] != stripe || body["DATABASE_URL"] != pg {
				t.Errorf("body = %v", body)
			}
		})
	}

	// --json beside a --format that is not json is refused: under --json
	// stdout carries a JSON body and nothing else, whatever else was asked.
	for _, argv := range [][]string{
		{"sandbox", "exports", "--json", "--format", "dotenv"},
		{"sandbox", "get", "--exports", "--json", "--format", "env"},
	} {
		code, stdout, stderr := runSandboxCLI(t, argv...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "--json is --format json; drop --format") {
			t.Errorf("%s: exit %d, stdout %q:\n%s", strings.Join(argv, " "), code, stdout, stderr)
		}
	}
}

func TestExportsSkipsATwinWithoutAHint(t *testing.T) {
	_, plane, twins := exportsBench(t)
	stripe := twins.srv.URL + "/s/" + sbID + "/stripe"
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox {
			services := append(twins.services(true), api.ServiceInfo{Name: "ledger", Status: "ready", URL: "http://ledger.internal"})
			return readySandbox(services, time.Now().Add(time.Hour))
		}
	})
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "exports")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if strings.Contains(stdout, "ledger") || !strings.Contains(stdout, "export STRIPE_API_BASE='"+stripe+"'\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "! ledger has no env hint, so nothing is exported for it\n") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestExportsRefusals(t *testing.T) {
	b, plane, _ := exportsBench(t)

	code, stdout, stderr := runSandboxCLI(t, "sandbox", "exports", "--format", "yaml")
	if code != 1 || stdout != "" || !strings.Contains(stderr, `--format "yaml" is not one of env, dotenv, json`) {
		t.Errorf("--format yaml: exit %d, stdout %q:\n%s", code, stdout, stderr)
	}

	code, stdout, stderr = runSandboxCLI(t, "sandbox", "get", "--format", "dotenv")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "--format goes with --exports") {
		t.Errorf("get --format without --exports: exit %d, stdout %q:\n%s", code, stdout, stderr)
	}

	// The exports print once; a watch has nothing to redraw, with or
	// without the --json that a plain --watch already refuses.
	for _, argv := range [][]string{
		{"sandbox", "get", "--exports", "--watch"},
		{"sandbox", "get", "--exports", "--watch", "--json"},
	} {
		code, stdout, stderr = runSandboxCLI(t, argv...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "--watch does not go with --exports") {
			t.Errorf("%s: exit %d, stdout %q:\n%s", strings.Join(argv, " "), code, stdout, stderr)
		}
	}

	code, stdout, stderr = runSandboxCLI(t, "sandbox", "exports", "stray")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "sandbox exports takes no arguments") {
		t.Errorf("stray word: exit %d, stdout %q:\n%s", code, stdout, stderr)
	}

	plane.script(func(p *sandboxPlane) { p.answer = nil })
	code, stdout, stderr = runSandboxCLI(t, "sandbox", "exports")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "✗ Failed to read services of sandbox "+sbID+": [404]") {
		t.Errorf("gone: exit %d, stdout %q:\n%s", code, stdout, stderr)
	}

	b.local(cfg.Local{})
	code, stdout, stderr = runSandboxCLI(t, "sandbox", "exports")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "✗ No sandbox for this folder\n→ Next: veris up\n") {
		t.Errorf("no pointer: exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
}

// sandbox get without --exports is untouched by the wrapper: the panel,
// on stderr, with --id still read.
func TestExportsLeavesGetAlone(t *testing.T) {
	_, _, twins := exportsBench(t)
	code, stdout, stderr := runSandboxCLI(t, "sandbox", "get", "--id", sbID)
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "Sandbox "+sbID+"\n", "Status:      ready\n",
		"  stripe    ready   STRIPE_API_BASE  "+twins.srv.URL+"/s/"+sbID+"/stripe")

	// The leaf's help names the flags, and the group lists the new verb.
	code, stdout, _ = runSandboxCLI(t, "sandbox", "get", "--help")
	if code != 0 || !strings.Contains(stdout, "--exports") || !strings.Contains(stdout, "--format") {
		t.Errorf("get --help: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = runSandboxCLI(t, "sandbox", "--help")
	if code != 0 || !strings.Contains(stdout, "  exports ") {
		t.Errorf("sandbox --help: exit %d\n%s", code, stdout)
	}
}

func TestExportsRendering(t *testing.T) {
	services := []api.ServiceInfo{
		{Name: "stripe", EnvHint: "STRIPE_API_BASE", URL: "https://gw/s/x/stripe"},
		{Name: "odd", EnvHint: "ODD_URL", URL: "http://it's here/#frag"},
		{Name: "nohint", URL: "http://nowhere"},
	}
	var out bytes.Buffer
	if err := renderExports(&out, "env", services); err != nil {
		t.Fatal(err)
	}
	want := "export STRIPE_API_BASE='https://gw/s/x/stripe'\nexport ODD_URL='http://it'\\''s here/#frag'\n"
	if out.String() != want {
		t.Errorf("env:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	if err := renderExports(&out, "dotenv", services); err != nil {
		t.Fatal(err)
	}
	want = "STRIPE_API_BASE=https://gw/s/x/stripe\nODD_URL=\"http://it's here/#frag\"\n"
	if out.String() != want {
		t.Errorf("dotenv:\n%s\nwant:\n%s", out.String(), want)
	}
	if got := shellQuote(`a'b`); got != `'a'\''b'` {
		t.Errorf("shellQuote = %s", got)
	}
}
