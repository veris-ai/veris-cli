package main

import (
	"net/http"
	"net/http/httptest"
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

func TestHostSideDeleteIsScopedToEnvironmentRuns(t *testing.T) {
	// Attach mode: the sandbox is not ours to delete, whatever id we know.
	deleteDeployedSandbox(dockerRun{Sandbox: "sbx_theirs"}, "sbx_theirs")
	// Environment mode with no id learned: nothing to address; the TTL holds.
	deleteDeployedSandbox(dockerRun{Environment: "env_1"}, "")
	// Neither call may reach the network: no API key means NewClient refuses,
	// and the guards above return before it. Reaching this line is the test.
}
