package discovery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The whole point of creating the sandbox rather than attaching is that the
// callback URL exists first and is handed over at creation -- so the sandbox is
// never alive without knowing where to deliver.
func TestCreateCarriesTheCallbackURLAndTTL(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/environments/env_x/sandboxes" && r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sbx_new","status":"provisioning"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "key")
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := c.Create(context.Background(), "env_x", CreateOptions{
		ClientBaseURL: "https://odd-forest.trycloudflare.com", TTLMinutes: 60,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sandbox.ID != "sbx_new" {
		t.Errorf("sandbox = %+v", sandbox)
	}
	if body["client_base_url"] != "https://odd-forest.trycloudflare.com" {
		t.Errorf("client_base_url was not sent at creation: %+v", body)
	}
	if body["ttl_minutes"] != float64(60) {
		t.Errorf("ttl_minutes = %v; without it a crashed run leaks a sandbox", body["ttl_minutes"])
	}
}

// Create returns while the sandbox is still provisioning. Routing at it then
// would fail a suite against a sandbox that was merely still starting.
func TestWaitReadyPollsUntilReady(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		status := "provisioning"
		if calls > 1 {
			status = "ready"
		}
		_, _ = w.Write([]byte(`{"id":"sbx_new","status":"` + status + `"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "key")
	sandbox, err := c.WaitReady(context.Background(), "sbx_new", 30*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if sandbox.Status != "ready" {
		t.Errorf("status = %q", sandbox.Status)
	}
	if calls < 2 {
		t.Errorf("returned after %d call(s); it must not accept provisioning", calls)
	}
}

// A sandbox stuck provisioning must fail loudly rather than be routed at.
func TestWaitReadyGivesUpAndSaysWhatItSaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sbx_new","status":"provisioning"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "key")
	_, err := c.WaitReady(context.Background(), "sbx_new", 1*time.Millisecond)
	if err == nil {
		t.Fatal("a sandbox that never became ready must be an error")
	}
	if !strings.Contains(err.Error(), "provisioning") {
		t.Errorf("the error should name the state it was stuck in: %v", err)
	}
}

func TestDeleteAddressesTheEnvironmentScopedPath(t *testing.T) {
	var path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, method = r.URL.Path, r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "key")
	if err := c.Delete(context.Background(), "env_x", "sbx_new"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if method != http.MethodDelete || path != "/v1/environments/env_x/sandboxes/sbx_new" {
		t.Errorf("%s %s", method, path)
	}
}

// A sandbox that failed cannot become ready, so polling it for the full
// timeout only delays the report and leaves the tunnel open meanwhile.
func TestWaitReadyStopsOnATerminalState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sbx_bad","status":"failed"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "key")
	start := time.Now()
	_, err := c.WaitReady(context.Background(), "sbx_bad", 5*time.Minute)
	if err == nil {
		t.Fatal("a failed sandbox must be reported, not waited on")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("the error should name the state: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("waited %s on a terminal state", elapsed)
	}
}
