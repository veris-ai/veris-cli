package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/discovery"
)

// Naming a sandbox on one command must not change what any other command uses.
// That is the whole reason --sandbox exists next to `use`: two suites against
// two sandboxes have to be able to run at once, and CI should not depend on a
// file in someone's home directory.

// isolateHome points ~/.veris at a temp dir and clears every environment
// source, so a test starts from "nothing selected".
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv(discovery.EnvConfig, "")
	t.Setenv(discovery.EnvSandboxID, "")
	t.Setenv(discovery.EnvAPIKey, "")
	return home
}

// cacheSandbox writes a snapshot the way SnapshotFor would, so resolution needs
// no control plane.
func cacheSandbox(t *testing.T, id string, services ...string) {
	t.Helper()
	snapshot := discovery.Snapshot{
		SandboxID: id,
		Status:    "ready",
		APIBase:   "http://control.test",
		FetchedAt: time.Now().UTC(),
	}
	for _, name := range services {
		snapshot.Services = append(snapshot.Services, discovery.Service{
			Name: name, URL: "http://sandbox.test/s/" + id + "/" + name, Status: "ready",
		})
	}
	body, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(discovery.Dir(), "sandboxes", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxFlagRoutesWithNoStoredStateAtAll(t *testing.T) {
	isolateHome(t)
	cacheSandbox(t, "sbx_one", "stripe")

	cfg, source, err := resolveConfig(configSources{Sandbox: "sbx_one"})
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.SandboxID != "sbx_one" {
		t.Errorf("sandbox_id = %q", cfg.SandboxID)
	}
	if !strings.Contains(source, "--sandbox") {
		t.Errorf("source = %q, should name the flag it came from", source)
	}

	// Nothing persists between commands: a second command with no flag has
	// nothing to fall back on, which is the point.
	if _, _, err := resolveConfig(configSources{}); err == nil {
		t.Fatal("a --sandbox run left state behind for the next command")
	}
}

// Two runs against two sandboxes, at the same time, with no shared state.
func TestTwoSandboxesResolveIndependently(t *testing.T) {
	isolateHome(t)
	cacheSandbox(t, "sbx_a", "stripe")
	cacheSandbox(t, "sbx_b", "airtable")

	a, _, err := resolveConfig(configSources{Sandbox: "sbx_a"})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := resolveConfig(configSources{Sandbox: "sbx_b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.SandboxID == b.SandboxID {
		t.Fatal("two --sandbox resolutions collided")
	}
	if a.Services[0].Name != "stripe" || b.Services[0].Name != "airtable" {
		t.Errorf("services crossed over: %q and %q", a.Services[0].Name, b.Services[0].Name)
	}
}

func TestPrecedenceIsMostExplicitFirst(t *testing.T) {
	home := isolateHome(t)
	cacheSandbox(t, "sbx_flag", "stripe")
	cacheSandbox(t, "sbx_env", "airtable")

	// A file for --config and for VERIS_PROXY_CONFIG.
	writeFile := func(name, sandbox string) string {
		path := filepath.Join(home, name)
		body := `{"version":1,"listen":"127.0.0.1:0","sandbox_id":"` + sandbox + `",
			"upstream":{"base_url":"http://x.test"},
			"services":[{"name":"stripe","hosts":["api.stripe.com"]}]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	flagFile := writeFile("flag.json", "sbx_file_flag")
	envFile := writeFile("env.json", "sbx_file_env")

	t.Setenv(discovery.EnvConfig, envFile)
	t.Setenv(discovery.EnvSandboxID, "sbx_env")

	// Everything is set at once; each layer wins over the ones below it.
	for _, tc := range []struct {
		name string
		src  configSources
		want string
	}{
		{"--config beats all", configSources{File: flagFile, Sandbox: "sbx_flag"}, "sbx_file_flag"},
		{"--sandbox beats the environment", configSources{Sandbox: "sbx_flag"}, "sbx_flag"},
		{"VERIS_PROXY_CONFIG beats VERIS_SANDBOX_ID", configSources{}, "sbx_file_env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := resolveConfig(tc.src)
			if err != nil {
				t.Fatalf("resolveConfig: %v", err)
			}
			if cfg.SandboxID != tc.want {
				t.Errorf("sandbox_id = %q, want %q", cfg.SandboxID, tc.want)
			}
		})
	}

	// With the config file gone, the sandbox environment variable is what is
	// left.
	t.Setenv(discovery.EnvConfig, "")
	cfg, _, err := resolveConfig(configSources{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SandboxID != "sbx_env" {
		t.Errorf("sandbox_id = %q, want the environment's sbx_env", cfg.SandboxID)
	}
}

// With nothing set anywhere, the error has to name the ways out rather than
// just failing.
func TestNothingToRouteSaysHowToFixIt(t *testing.T) {
	isolateHome(t)
	_, _, err := resolveConfig(configSources{})
	if err == nil {
		t.Fatal("expected an error with no config and no sandbox")
	}
	for _, want := range []string{"--sandbox", "--config", discovery.EnvSandboxID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// An unknown sandbox with no cached copy and no API key must say both things,
// not just "no API key".
func TestAnUnknownSandboxExplainsItself(t *testing.T) {
	isolateHome(t)
	_, _, err := resolveConfig(configSources{Sandbox: "sbx_never_seen"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "sbx_never_seen") {
		t.Errorf("error does not name the sandbox: %v", err)
	}
}
