package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// veris services is the catalog the env create picker reads, as a table for
// a person and as JSON for a script; a twin with no hostnames says it is not
// intercepted rather than claiming to be a data plane, which the catalog --
// carrying no URL -- cannot tell it apart from a twin not measured yet.
func TestServicesListsTheCatalog(t *testing.T) {
	b := newEnvBench(t)

	stdout, stderr, code := b.run("services")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty without --json, got %q", stdout)
	}
	for _, want := range []string{
		"Available on " + b.srv.URL + " (5 twins)",
		"stripe", "Stripe payments API", "api.stripe.com",
		"postgres", "— (not intercepted)",
		"github", "api.github.com",
		// The name carries the issuer the service brings along, and a
		// footnote says what the marker means.
		"google-calendar (+google-identity)",
		"(+name) is added automatically with the service beside it",
		"→ Next: veris env create --services name,name",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}

	stdout, stderr, code = b.run("services", "--json")
	if code != 0 {
		t.Fatalf("--json exit %d\n%s", code, stderr)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if len(got) != 5 || got[0]["name"] != "stripe" {
		t.Errorf("json = %v", got)
	}
	if strings.Contains(stderr, "{") {
		t.Errorf("JSON leaked to stderr:\n%s", stderr)
	}

	if _, stderr, code := b.run("services", "extra"); code != 1 || !strings.Contains(stderr, "services takes no arguments") {
		t.Errorf("a stray word: exit %d, stderr %q", code, stderr)
	}
}
