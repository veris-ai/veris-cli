package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/config"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/routes"
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

// --- --route overrides ---

func TestParseRouteFlagReadsServiceHostAndPrefix(t *testing.T) {
	for _, tc := range []struct {
		in      string
		service string
		host    string
		paths   []string
	}{
		{"stripe=api.stripe.com", "stripe", "api.stripe.com", nil},
		{"google-calendar=www.googleapis.com/calendar/", "google-calendar",
			"www.googleapis.com", []string{"/calendar/"}},
		{"acme=*.acme.example", "acme", "*.acme.example", nil},
	} {
		service, entry, err := parseRouteFlag(tc.in)
		if err != nil {
			t.Fatalf("parseRouteFlag(%q): %v", tc.in, err)
		}
		if service != tc.service || entry.Host != tc.host {
			t.Errorf("parseRouteFlag(%q) = %q %q", tc.in, service, entry.Host)
		}
		if len(entry.Paths) != len(tc.paths) {
			t.Errorf("parseRouteFlag(%q) paths = %v, want %v", tc.in, entry.Paths, tc.paths)
		}
		for i := range tc.paths {
			if entry.Paths[i] != tc.paths[i] {
				t.Errorf("parseRouteFlag(%q) paths = %v, want %v", tc.in, entry.Paths, tc.paths)
			}
		}
	}
}

func TestParseRouteFlagRefusesShapesThatRouteNothing(t *testing.T) {
	for _, in := range []string{"", "stripe", "stripe=", "=api.stripe.com", "stripe=/v1"} {
		if _, _, err := parseRouteFlag(in); err == nil {
			t.Errorf("parseRouteFlag(%q) accepted", in)
		}
	}
}

// An override on a file config keeps the file's upstream for that service --
// the file is the only place the upstream lives.
func TestFileConfigOverrideKeepsTheUpstream(t *testing.T) {
	cfg := &config.Config{
		Version: 1, Listen: "127.0.0.1:0", SandboxID: "sbx_f", Mode: config.ModePassthrough,
		Services: []config.Service{
			{Name: "stripe", Hosts: []string{"api.stripe.com"}, Upstream: "http://sbx/stripe"},
			{Name: "other", Hosts: []string{"api.other.example"}, Upstream: "http://sbx/other"},
		},
	}
	err := applyOverridesToFileConfig(cfg,
		map[string][]routes.Entry{"stripe": {{Host: "api.stripe.dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	var stripeHost, stripeUpstream string
	var otherKept bool
	for _, svc := range cfg.Services {
		switch svc.Name {
		case "stripe":
			stripeHost, stripeUpstream = svc.Hosts[0], svc.Upstream
		case "other":
			otherKept = true
		}
	}
	if stripeHost != "api.stripe.dev" || stripeUpstream != "http://sbx/stripe" {
		t.Errorf("stripe = %s -> %s, want the new host on the file's upstream",
			stripeHost, stripeUpstream)
	}
	if !otherKept {
		t.Error("the untouched service was dropped")
	}
}

func TestFileConfigOverrideForAnUnknownServiceIsAnError(t *testing.T) {
	cfg := &config.Config{
		Version: 1, Listen: "127.0.0.1:0", Mode: config.ModePassthrough,
		Services: []config.Service{
			{Name: "stripe", Hosts: []string{"api.stripe.com"}, Upstream: "http://sbx/stripe"},
		},
	}
	err := applyOverridesToFileConfig(cfg,
		map[string][]routes.Entry{"ghost": {{Host: "api.ghost.example"}}})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want a refusal naming the service", err)
	}
}

// The folder's sandbox pointer (configSources.Local, which run sets from
// .veris/twin.local.yaml) is consulted after every explicit source, and the
// resolved source names the file so a surprise can be traced to it.
func TestRunDefaultsPointerIsTheLastResort(t *testing.T) {
	isolateHome(t)
	for _, id := range []string{"sbx_local", "sbx_env", "sbx_flag"} {
		cacheSandbox(t, id, "stripe")
	}

	cfg, source, err := resolveConfig(configSources{Local: "sbx_local"})
	if err != nil {
		t.Fatalf("pointer alone: %v", err)
	}
	if cfg.SandboxID != "sbx_local" || !strings.Contains(source, ".veris/twin.local.yaml") {
		t.Errorf("pointer alone: sandbox %q from %q", cfg.SandboxID, source)
	}
	if got := sandboxForContainer(configSources{Local: "sbx_local"}); got != "sbx_local" {
		t.Errorf("container tier ignores the pointer: %q", got)
	}

	cfg, _, err = resolveConfig(configSources{Sandbox: "sbx_flag", Local: "sbx_local"})
	if err != nil || cfg.SandboxID != "sbx_flag" {
		t.Errorf("--sandbox beats the pointer: %v %+v", err, cfg)
	}

	t.Setenv(discovery.EnvSandboxID, "sbx_env")
	cfg, _, err = resolveConfig(configSources{Local: "sbx_local"})
	if err != nil || cfg.SandboxID != "sbx_env" {
		t.Errorf("$VERIS_SANDBOX_ID beats the pointer: %v %+v", err, cfg)
	}
	if got := sandboxForContainer(configSources{Local: "sbx_local"}); got != "sbx_env" {
		t.Errorf("container tier: $VERIS_SANDBOX_ID beats the pointer, got %q", got)
	}

	file := writeConfig(t, "http://upstream.test")
	cfg, _, err = resolveConfig(configSources{File: file, Local: "sbx_local"})
	if err != nil || cfg.SandboxID != "sbx_run" {
		t.Errorf("--config beats the pointer: %v %+v", err, cfg)
	}
	if got := sandboxForContainer(configSources{File: file, Local: "sbx_local"}); got != "" {
		t.Errorf("container tier: --config leaves no sandbox to name, got %q", got)
	}
}
