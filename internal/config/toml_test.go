package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleTOML = `
[veris]
env_id = "env123"
api_url = "https://api.veris.ai"

[proxy]
listen = "127.0.0.1:9090"
allow_passthrough = ["@build", "ci.internal.example.com"]

[services.fineract]
hosts = ["demo.mifos.io", "*.fineract.example.com"]

[services.stripe]
hosts = ["api.stripe.com"]
upstream = "https://elsewhere.example.com/stripe"

[run]
mode = "host"
test_cmd = "make integration"
future_key = { nested = true }
`

func writeTOML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".veris.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadTOMLWithOverrides(t *testing.T) {
	p := writeTOML(t, sampleTOML)
	c, err := LoadWithOverrides(p, Overrides{
		SandboxID:       "sbx_run1",
		CanaryToken:     "tok_run1",
		UpstreamBaseURL: "http://svc.dev.api.veris.ai",
	})
	if err != nil {
		t.Fatal(err)
	}

	if c.SandboxID != "sbx_run1" || c.CanaryToken != "tok_run1" {
		t.Errorf("per-run overrides not applied: %+v", c)
	}
	if c.EnvID != "env123" {
		t.Errorf("env_id not mapped: %q", c.EnvID)
	}
	if c.Listen != "127.0.0.1:9090" {
		t.Errorf("listen not mapped: %q", c.Listen)
	}
	if c.Mode != ModeStrict {
		t.Errorf("mode should default to strict, got %q", c.Mode)
	}

	target, ok := c.Resolve("demo.mifos.io")
	if !ok || target.Service != "fineract" {
		t.Fatalf("fineract host not routed: %+v", target)
	}
	if got := target.Upstream.String(); got != "http://svc.dev.api.veris.ai/s/sbx_run1/fineract" {
		t.Errorf("derived upstream wrong: %q", got)
	}
	if target, ok := c.Resolve("api.stripe.com"); !ok || target.Upstream.String() != "https://elsewhere.example.com/stripe" {
		t.Errorf("per-service upstream override lost: %+v", target)
	}
	if !c.IsPassthrough("repo.maven.apache.org") {
		t.Error("@build preset not expanded from TOML")
	}
}

func TestLoadTOMLServicesAreOrdered(t *testing.T) {
	p := writeTOML(t, sampleTOML)
	for i := 0; i < 5; i++ {
		c, err := LoadWithOverrides(p, Overrides{SandboxID: "s", UpstreamBaseURL: "https://u.example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if c.Services[0].Name != "fineract" || c.Services[1].Name != "stripe" {
			t.Fatalf("service order not deterministic: %+v", c.Services)
		}
	}
}

func TestLoadTOMLWithoutSandboxFailsClosed(t *testing.T) {
	p := writeTOML(t, sampleTOML)
	if _, err := LoadWithOverrides(p, Overrides{UpstreamBaseURL: "https://u.example.com"}); err == nil {
		t.Fatal("a TOML config with no sandbox id and no --sandbox-id must not validate")
	}
}

func TestLoadTOMLRunTableIgnored(t *testing.T) {
	// [run] belongs to the skill layer; unknown keys in it must never break
	// an older proxy. sampleTOML carries a nested future_key for this.
	p := writeTOML(t, sampleTOML)
	if _, err := LoadWithOverrides(p, Overrides{SandboxID: "s", UpstreamBaseURL: "https://u.example.com"}); err != nil {
		t.Fatalf("unknown [run] keys broke loading: %v", err)
	}
}
