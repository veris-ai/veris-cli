package callback

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sandbox stands in for a service's control plane, recording the PATCH it was
// sent so the wire shape is pinned rather than assumed.
func sandbox(t *testing.T, probe map[string]any) (*Client, *[]map[string]any) {
	t.Helper()
	var patched []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/veris/data":
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Data struct {
					Client []map[string]any `json:"client"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("unreadable PATCH: %v", err)
			}
			patched = append(patched, req.Data.Client...)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"updated":{"client":1}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/veris/client/probe":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(probe)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, Options{}), &patched
}

func TestRegisterPatchesTheURLAndProbesIt(t *testing.T) {
	c, patched := sandbox(t, map[string]any{
		"probe_state": "answered", "base_url_revision": 1, "probed_revision": 1,
		"default_base_url":  "https://odd-forest.trycloudflare.com",
		"last_probe_result": map[string]any{"outcome": "http_response", "status": 200},
	})

	state, err := c.Register(context.Background(), "https://odd-forest.trycloudflare.com")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !state.Answered() {
		t.Errorf("probe state = %q, want answered", state.State)
	}
	if len(*patched) != 1 {
		t.Fatalf("PATCHes = %d, want 1", len(*patched))
	}
	row := (*patched)[0]
	if row["default_base_url"] != "https://odd-forest.trycloudflare.com" {
		t.Errorf("PATCHed row = %+v", row)
	}
	// The row is a singleton; without the id the write has no target.
	if row["id"] != float64(1) {
		t.Errorf("PATCH should address the singleton row: %+v", row)
	}
}

// A tunnel that is up but whose origin is gone answers from the edge. That is a
// different failure from an app returning an error, and the sandbox already
// tells them apart -- so this must not report it as merely unreachable.
func TestADeadTunnelIsDistinguishedFromAnUnreachableApp(t *testing.T) {
	c, _ := sandbox(t, map[string]any{
		"probe_state": "unreachable", "base_url_revision": 1,
		"last_probe_result": map[string]any{
			"outcome": "http_response", "status": 530,
			"dead_tunnel_signature": "error code: 1033",
		},
	})

	state, err := c.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if state.Answered() {
		t.Error("a dead tunnel is not an answered probe")
	}
	if state.DeadTunnel() != "error code: 1033" {
		t.Errorf("dead tunnel signature = %q", state.DeadTunnel())
	}
}

// Clearing must send an explicit null: leaving a dead hostname registered means
// the dispatcher keeps trying it after the run is gone.
func TestClearUnregistersRatherThanLeavingADeadHostname(t *testing.T) {
	c, patched := sandbox(t, map[string]any{"probe_state": "unknown"})

	if err := c.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(*patched) != 1 {
		t.Fatalf("PATCHes = %d", len(*patched))
	}
	row := (*patched)[0]
	v, ok := row["default_base_url"]
	if !ok {
		t.Fatal("the field must be present and null, not omitted")
	}
	if v != nil {
		t.Errorf("default_base_url = %v, want null", v)
	}
}

// A control plane that refuses the write must not leave the caller believing
// callbacks are registered.
func TestARefusedPatchIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail":[{"msg":"nope"}]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, Options{}).Register(context.Background(), "https://x.trycloudflare.com")
	if err == nil {
		t.Fatal("a 422 must surface")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "nope") {
		t.Errorf("the error should carry the sandbox's own words: %v", err)
	}
}

// A sandbox that authenticates its control plane refuses an unauthenticated
// PATCH, so --expose would fail at registration against exactly the deployed
// sandbox it exists for.
func TestTheSandboxCredentialIsSentOnEveryControlPlaneCall(t *testing.T) {
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"probe_state":"answered","rows":[{}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, Options{AuthValue: "sk_sandbox_123"})
	if _, err := c.Register(context.Background(), "https://x.trycloudflare.com"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := c.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	if err := c.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	for _, call := range []string{
		"PATCH /veris/data", "POST /veris/client/probe", "GET /veris/data",
	} {
		if got := seen[call]; got != "Bearer sk_sandbox_123" {
			t.Errorf("%s sent Authorization %q", call, got)
		}
	}
}
