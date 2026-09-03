package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestFetchSandboxIDReadsTheStatusEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"veris_proxy":true,"sandbox_id":"sbx_from_env","mode":"passthrough"}`))
	}))
	defer srv.Close()

	if got := fetchSandboxID(srv.URL); got != "sbx_from_env" {
		t.Fatalf("fetchSandboxID = %q, want sbx_from_env", got)
	}
	// Best-effort by design: an unreachable proxy costs only the host-side
	// backstop, never the run.
	if got := fetchSandboxID("http://127.0.0.1:1"); got != "" {
		t.Fatalf("unreachable status must yield \"\", got %q", got)
	}
}

// The line the integration-testing skill tells its readers to look for. It
// existed only in serve's log inside the container, so a run on the host
// never showed it (#4). The id is the one read back from the proxy, and an
// unknown id is never printed as if it were one.
func TestTheRoutedSandboxIsAnnouncedFromTheStatusEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"veris_proxy":true,"sandbox_id":"sbx_deployed","mode":"passthrough"}`))
	}))
	defer srv.Close()

	var out, logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	announceSandbox(&out, log, fetchSandboxID(srv.URL))
	if got, want := out.String(), "veris: sandbox ready sandbox_id=sbx_deployed\n"; got != want {
		t.Fatalf("announced %q, want %q", got, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("a known id must not also log the unknown-id line: %q", logs.String())
	}

	// Unknown: nothing on the output stream, and the reason at info level.
	out.Reset()
	announceSandbox(&out, log, fetchSandboxID("http://127.0.0.1:1"))
	if out.Len() != 0 {
		t.Fatalf("an unknown id must print nothing on the announcement stream, got %q", out.String())
	}
	if !strings.Contains(logs.String(), "sandbox id unknown") {
		t.Fatalf("the unknown id should be said at info level, got %q", logs.String())
	}
	// And at the run default, warn, not even that.
	logs.Reset()
	quiet := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	announceSandbox(&out, quiet, "")
	if out.Len() != 0 || logs.Len() != 0 {
		t.Fatalf("at warn level an unknown id says nothing at all: out=%q logs=%q", out.String(), logs.String())
	}
}

func TestHostSideDeleteIsScopedToEnvironmentRuns(t *testing.T) {
	// Attach mode: the sandbox is not ours to delete, whatever id we know.
	deleteDeployedSandbox(dockerRun{Sandbox: "sbx_theirs"}, "sbx_theirs")
	// Environment mode with no id learned: nothing to address; the TTL holds.
	deleteDeployedSandbox(dockerRun{Environment: "env_1"}, "")
	// Neither call may reach the network: no API key means NewClient refuses,
	// and the guards above return before it. Reaching this line is the test.
}

// The workload is hardened by default and --cap-add hands back exactly what
// it names, in docker's own order: the drop first, then each add (#5). The
// capability never reaches the proxy container, whose one capability is
// NET_ADMIN for the redirect.
func TestCapAddFollowsTheDropAndStaysOffTheProxy(t *testing.T) {
	spec := dockerRun{
		Image: "img:test", ProxyImage: "proxy:test",
		Sandbox: "sbx_1", CapAdd: []string{"SETUID", "CAP_SETGID"},
	}
	args := workloadArgs(spec, "veris-proxy-1", "veris-workload-1", "/share", nil, false)

	drop := slices.Index(args, "--cap-drop=ALL")
	setuid := slices.Index(args, "--cap-add=SETUID")
	setgid := slices.Index(args, "--cap-add=CAP_SETGID")
	image := slices.Index(args, "img:test")
	if drop < 0 || setuid < 0 || setgid < 0 || image < 0 {
		t.Fatalf("missing an expected argument in %q", args)
	}
	if !(drop < setuid && setuid < setgid && setgid < image) {
		t.Fatalf("want --cap-drop=ALL, then each --cap-add as written, then the image; got %q", args)
	}

	// Without the flag the hardened default stands alone.
	spec.CapAdd = nil
	for _, a := range workloadArgs(spec, "veris-proxy-1", "veris-workload-1", "/share", nil, false) {
		if strings.HasPrefix(a, "--cap-add") {
			t.Fatalf("no --cap-add was asked for, yet %q was passed", a)
		}
	}

	// The proxy container is not widened by the workload's request.
	spec.CapAdd = []string{"SETUID"}
	proxyArgs, err := proxyContainerArgs(spec, "veris-proxy-1", "veris-net-1", "/share")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range proxyArgs {
		if strings.HasPrefix(a, "--cap-add") && a != "--cap-add=NET_ADMIN" {
			t.Fatalf("the workload's --cap-add leaked onto the proxy container: %q", proxyArgs)
		}
	}
	if !slices.Contains(proxyArgs, "--cap-add=NET_ADMIN") {
		t.Fatalf("the proxy container lost NET_ADMIN: %q", proxyArgs)
	}
}

// Ordinary capabilities pass, in either spelling; the two that would hand back
// the isolation itself are refused with a reason, as is anything that is not
// shaped like a capability name.
func TestCapAddRefusesWhatWouldDefeatTheIsolation(t *testing.T) {
	for _, ok := range []string{"SETUID", "SETGID", "CHOWN", "DAC_OVERRIDE",
		"FOWNER", "NET_BIND_SERVICE", "CAP_SETUID", " CAP_CHOWN "} {
		got, err := parseCapability(ok)
		if err != nil {
			t.Errorf("parseCapability(%q) refused: %v", ok, err)
		}
		if got != strings.TrimSpace(ok) {
			t.Errorf("parseCapability(%q) = %q", ok, got)
		}
	}
	for _, bad := range []struct{ in, why string }{
		{"ALL", "every capability"},
		{"CAP_ALL", "every capability"},
		{"SYS_ADMIN", "root in all but name"},
		{"CAP_SYS_ADMIN", "root in all but name"},
		{"setuid", "upper-case"},
		{"", "upper-case"},
		{"SETUID,SETGID", "upper-case"},
		{"--privileged", "upper-case"},
	} {
		_, err := parseCapability(bad.in)
		if err == nil {
			t.Errorf("parseCapability(%q) accepted", bad.in)
			continue
		}
		if !strings.Contains(err.Error(), bad.why) {
			t.Errorf("parseCapability(%q) refused for the wrong reason: %v", bad.in, err)
		}
	}
}
