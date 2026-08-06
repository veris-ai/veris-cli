package config

import (
	"strings"
	"testing"
)

func presetBase() *Config {
	return &Config{
		Version:   1,
		Listen:    "127.0.0.1:8080",
		SandboxID: "sbx_test",
		Mode:      ModeStrict,
		Upstream:  Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services:  []Service{{Name: "stripe", Hosts: []string{"api.stripe.com"}}},
	}
}

func TestBuildPresetExpansion(t *testing.T) {
	c := presetBase()
	c.AllowPassthrough = []string{"@build", "*.internal.example.com"}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// The preset expands to real registry hosts and keeps neighbours intact.
	for _, host := range []string{"repo.maven.apache.org", "services.gradle.org", "registry.npmjs.org", "pypi.org"} {
		if !c.IsPassthrough(host) {
			t.Errorf("IsPassthrough(%q) = false after @build expansion", host)
		}
	}
	if !c.IsPassthrough("ci.internal.example.com") {
		t.Error("explicit passthrough entry lost during preset expansion")
	}
	// Expansion must not loosen strictness for anything else.
	if c.IsPassthrough("api.stripe.com") || c.IsPassthrough("example.com") {
		t.Error("@build expansion made unrelated hosts passthrough")
	}
	for _, p := range c.AllowPassthrough {
		if p == "@build" {
			t.Error("@build left unexpanded in AllowPassthrough")
		}
	}
}

func TestUnknownPresetRejected(t *testing.T) {
	c := presetBase()
	c.AllowPassthrough = []string{"@bulid"}
	c.applyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Fatalf("want unknown-preset error, got %v", err)
	}
}
