package discovery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnvironmentReportsWhetherAWorldWasPromoted(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"env_1","name":"checkout","services":["stripe"],
			"baseline":{"image":"repo@sha256:abc","revision_id":"rev_1",
			"promoted_at":"2026-08-14T10:00:00Z","source_sandbox":"sbx_9"}}`))
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, "key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	env, err := client.Environment(context.Background(), "env_1")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	if path != "/v1/environments/env_1" {
		t.Errorf("path = %q", path)
	}
	if !env.Promoted() {
		t.Fatalf("an environment with a baseline image must read as promoted: %+v", env.Baseline)
	}
	if env.Baseline.SourceSandbox != "sbx_9" {
		t.Errorf("source sandbox = %q", env.Baseline.SourceSandbox)
	}
}

// A control plane that predates the field, and one that pins nothing, are the
// same answer to the only question asked here: nothing is promised, so every
// run rebuilds the world.
func TestAnEnvironmentWithoutABaselineIsNotPromoted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"env_1","services":["stripe"]}`))
	}))
	defer srv.Close()

	client, _ := NewClient(srv.URL, "key")
	env, err := client.Environment(context.Background(), "env_1")
	if err != nil {
		t.Fatalf("Environment: %v", err)
	}
	if env.Promoted() {
		t.Error("no baseline must not read as promoted")
	}
	var nilEnv *Environment
	if nilEnv.Promoted() {
		t.Error("a nil environment must not read as promoted")
	}
}

func TestPromotePostsToTheSandboxScopedPath(t *testing.T) {
	var (
		method, path string
		body         map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"environment_id":"env_1","sandbox_id":"sbx_9",
			"baseline":{"image":"repo@sha256:abc","revision_id":"rev_2",
			"promoted_at":null,"source_sandbox":"sbx_9"},
			"clock_restore":"rebase","size_bytes":4194304,
			"curator_clock_restored":true,
			"scrubbed":{"stripe":["deliveries"]}}`))
	}))
	defer srv.Close()

	client, _ := NewClient(srv.URL, "key")
	result, err := client.Promote(context.Background(), "env_1", "sbx_9", PromoteOptions{})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if method != http.MethodPost || path != "/v1/environments/env_1/sandboxes/sbx_9/promote" {
		t.Errorf("%s %s", method, path)
	}
	// Off unless asked for: promoting somebody else's callback destination
	// points every future sandbox at them.
	if body["keep_external_destinations"] != false {
		t.Errorf("keep_external_destinations = %v, want false", body["keep_external_destinations"])
	}
	// Absent rather than guessed, so the control plane's own default decides.
	if _, sent := body["clock_restore"]; sent {
		t.Errorf("clock_restore must not be sent when unset: %v", body)
	}
	if result.SizeBytes != 4194304 || result.Scrubbed["stripe"][0] != "deliveries" {
		t.Errorf("result = %+v", result)
	}
}

func TestPromoteSurfacesTheControlPlanesRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"sandbox is not ready"}`))
	}))
	defer srv.Close()

	client, _ := NewClient(srv.URL, "key")
	_, err := client.Promote(context.Background(), "env_1", "sbx_9", PromoteOptions{
		ClockRestore: "frozen",
	})
	if err == nil {
		t.Fatal("a 409 must not read as a promoted world")
	}
}
