package config

import "testing"

func base() *Config {
	return &Config{
		Version:   1,
		Listen:    "127.0.0.1:8080",
		Mode:      ModeStrict,
		SandboxID: "sbx_test",
		Upstream:  Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services: []Service{
			{Name: "stripe", Hosts: []string{"api.stripe.com", "*.stripe.com"}},
			{Name: "sendgrid", Hosts: []string{"api.sendgrid.com"}},
		},
	}
}

func TestResolvePrefersExactOverWildcard(t *testing.T) {
	c := base()
	c.Services[0].Hosts = []string{"*.stripe.com"}
	c.Services = append(c.Services, Service{
		Name:     "stripe-files",
		Hosts:    []string{"files.stripe.com"},
		Upstream: "https://files.example.test/x",
	})

	got, ok := c.Resolve("files.stripe.com", "/")
	if !ok {
		t.Fatal("expected a match for files.stripe.com")
	}
	if got.Service != "stripe-files" {
		t.Fatalf("exact host should win over wildcard, got service %q", got.Service)
	}
}

func TestResolveWildcardMatchesApexAndSubdomain(t *testing.T) {
	c := base()
	c.Services = []Service{{Name: "stripe", Hosts: []string{"*.stripe.com"}}}

	for _, host := range []string{"stripe.com", "api.stripe.com", "a.b.stripe.com"} {
		if _, ok := c.Resolve(host, "/"); !ok {
			t.Errorf("expected *.stripe.com to match %q", host)
		}
	}
	// A wildcard must not match a host that merely ends in the same letters.
	if _, ok := c.Resolve("notstripe.com", "/"); ok {
		t.Error("*.stripe.com must not match notstripe.com")
	}
}

func TestResolveStripsPortAndIsCaseInsensitive(t *testing.T) {
	c := base()
	for _, host := range []string{"api.stripe.com:443", "API.Stripe.COM", "API.STRIPE.COM:8443"} {
		got, ok := c.Resolve(host, "/")
		if !ok {
			t.Fatalf("expected match for %q", host)
		}
		if got.Service != "stripe" {
			t.Fatalf("host %q resolved to %q", host, got.Service)
		}
	}
}

func TestResolveDerivesUpstreamFromBaseURL(t *testing.T) {
	c := base()
	got, ok := c.Resolve("api.sendgrid.com", "/")
	if !ok {
		t.Fatal("expected a match")
	}
	want := "https://sandbox.veris.ai/s/sbx_test/sendgrid"
	if got.Upstream.String() != want {
		t.Fatalf("derived upstream = %q, want %q", got.Upstream.String(), want)
	}
}

func TestResolveUnknownHost(t *testing.T) {
	if _, ok := base().Resolve("api.openai.com", "/"); ok {
		t.Fatal("unmapped host must not resolve")
	}
}

func TestPassthroughAlwaysCoversLoopback(t *testing.T) {
	c := base()
	// No AllowPassthrough configured: loopback must still be exempt, otherwise
	// a test calling its own service under test gets routed to the sandbox.
	for _, host := range []string{"localhost", "localhost:3000", "127.0.0.1", "127.0.0.1:8000", "[::1]:9000"} {
		if !c.IsPassthrough(host) {
			t.Errorf("expected %q to be passthrough by default", host)
		}
	}
	if c.IsPassthrough("api.stripe.com") {
		t.Error("a mapped host must not be passthrough")
	}
}

func TestPassthroughHonoursConfiguredPatterns(t *testing.T) {
	c := base()
	c.AllowPassthrough = []string{"*.internal.example.com"}
	if !c.IsPassthrough("db.internal.example.com") {
		t.Error("expected configured wildcard to allow passthrough")
	}
}

func TestValidateRejectsDuplicateHostAcrossServices(t *testing.T) {
	c := base()
	c.Services = append(c.Services, Service{Name: "other", Hosts: []string{"api.stripe.com"}})
	err := c.Validate()
	if err == nil {
		t.Fatal("two services claiming one host must be rejected, not resolved by declaration order")
	}
}

func TestValidateRejectsEmptyServices(t *testing.T) {
	c := base()
	c.Services = nil
	if err := c.Validate(); err == nil {
		t.Fatal("a config with no services would block every request; it must be rejected")
	}
}

func TestValidateRejectsInteriorWildcard(t *testing.T) {
	c := base()
	c.Services = []Service{{Name: "x", Hosts: []string{"api.*.com"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("only a leading \"*.\" wildcard is supported; api.*.com must be rejected")
	}
}

func TestValidateRejectsUnknownMode(t *testing.T) {
	c := base()
	c.Mode = "loose"
	if err := c.Validate(); err == nil {
		t.Fatal("an unrecognised mode must not silently fall back to a permissive default")
	}
}

func TestValidateRequiresUpstreamSomewhere(t *testing.T) {
	c := base()
	c.Upstream.BaseURL = ""
	if err := c.Validate(); err == nil {
		t.Fatal("a service with no upstream and no base_url must be rejected")
	}
}

func TestDefaultModeIsPassthrough(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Mode != ModePassthrough {
		t.Fatalf("default mode = %q, want passthrough: only the services a sandbox "+
			"provisions are rerouted, and blocking the rest makes adoption a "+
			"configuration project", c.Mode)
	}
}
