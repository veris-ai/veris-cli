package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/discovery"
	"github.com/veris-ai/veris-proxy/internal/proxy"
)

// The whole point of the command: a missing precondition stops the work. The
// measured alternative was an agent inventing its own transport around it.
func TestPreflightStopsAtAMissingCredential(t *testing.T) {
	t.Setenv(discovery.EnvAPIKey, "")
	report := runPreflight(preflightSpec{Environment: "env_1"})

	if report.ok() {
		t.Fatal("no API key must fail preflight")
	}
	if len(report.Checks) != 1 {
		t.Fatalf("nothing may be checked past the credential, got %d checks", len(report.Checks))
	}
	detail := report.Checks[0].Detail
	for _, want := range []string{discovery.EnvAPIKey, "X-API-Key"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the fix must name %q: %s", want, detail)
		}
	}
}

// Fail closed means fail closed: the failure text names the fix and never an
// alternative route to a green.
func TestAFailedPreflightSuggestsNoWorkaround(t *testing.T) {
	t.Setenv(discovery.EnvAPIKey, "")
	report := runPreflight(preflightSpec{})
	var out strings.Builder
	report.write(&out, false)

	if !strings.Contains(out.String(), "do not route the code under test by hand") {
		t.Errorf("the stop line is missing:\n%s", out.String())
	}
	for _, forbidden := range []string{"--config", "base URL override", "ngrok"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("preflight offered %q as a way on:\n%s", forbidden, out.String())
		}
	}
}

func TestPreflightPassesAndReportsAnUnpromotedEnvironment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"env_1","services":["stripe","google-calendar"]}`))
	}))
	defer srv.Close()

	report := runPreflight(preflightSpec{
		Environment: "env_1", APIBase: srv.URL, APIKey: "key",
	})

	// Docker is a real check against a real daemon, so its verdict belongs to
	// the machine running the test, not to this assertion.
	for _, c := range report.Checks {
		if c.Name != "docker" && c.Failed {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
	}
	if len(report.Notes) != 1 || !strings.Contains(report.Notes[0], "no promoted world") {
		t.Fatalf("notes = %v", report.Notes)
	}
	if !strings.Contains(report.Notes[0], "--promote-on-success") {
		t.Errorf("the note must name the move that fixes it: %s", report.Notes[0])
	}
}

// An environment id is a precondition, not an optional argument: without one
// the run deploys nothing and the receipt assertion has nothing to assert.
func TestPreflightRefusesAnUnnamedEnvironment(t *testing.T) {
	t.Setenv("VERIS_ENVIRONMENT_ID", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	report := runPreflight(preflightSpec{APIBase: srv.URL, APIKey: "key"})
	if report.ok() {
		t.Fatal("no environment must fail preflight")
	}
	var found bool
	for _, c := range report.Checks {
		if c.Name == "environment" && c.Failed {
			found = true
			if !strings.Contains(c.Detail, "VERIS_ENVIRONMENT_ID") {
				t.Errorf("detail = %q", c.Detail)
			}
		}
	}
	if !found {
		t.Errorf("checks = %+v", report.Checks)
	}
}

func TestPreflightNamesARefusedKeyAsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client, err := discovery.NewClient(srv.URL, "stale")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = reachable(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "refused the API key") {
		t.Fatalf("err = %v", err)
	}
}

func TestPromotedEnvironmentNoteNamesTheCapture(t *testing.T) {
	at := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	note := promotedNote("env_1", &discovery.Environment{
		ID: "env_1",
		Baseline: &discovery.Baseline{
			Image: "repo@sha256:abc", RevisionID: "rev_1",
			PromotedAt: &at, SourceSandbox: "sbx_9",
		},
	})
	for _, want := range []string{"sbx_9", "2026-08-14"} {
		if !strings.Contains(note, want) {
			t.Errorf("note must name %q: %s", want, note)
		}
	}
}

// The guard on the flag: a world nothing reached must never become every
// future run's starting point, whatever the suite's exit code said.
func TestAnEmptyWorldIsNotPromotable(t *testing.T) {
	if why := unpromotable("sbx_9", proxy.Receipt{Total: 0}); why == "" {
		t.Error("an empty receipt must block promotion")
	}
	if why := unpromotable("", proxy.Receipt{Total: 3}); why == "" {
		t.Error("an unknown sandbox id must block promotion")
	}
	if why := unpromotable("sbx_9", proxy.Receipt{Total: 3}); why != "" {
		t.Errorf("a real world must be promotable, got %q", why)
	}
}
