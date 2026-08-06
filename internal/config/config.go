// Package config defines the on-disk contract between the Veris CLI and the
// proxy.
//
// The format is JSON rather than YAML on purpose: the CLI owns the
// human-facing veris.yaml and compiles it down to this, which keeps the proxy
// binary free of third-party dependencies and keeps the wire format
// unambiguous.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Mode controls what happens to a host with no matching service.
type Mode string

const (
	// ModeStrict blocks unmapped hosts. This is the default, and it is the
	// only mode that makes a passing test run meaningful: if the code under
	// test can still reach the real internet, a green run proves nothing.
	ModeStrict Mode = "strict"

	// ModePassthrough forwards unmapped hosts to their real destination. Useful
	// while a developer is still discovering which dependencies a service has,
	// but it must never be the default.
	ModePassthrough Mode = "passthrough"
)

// Config is the complete proxy configuration.
type Config struct {
	Version int `json:"version"`

	// Listen is the address the proxy binds. Defaults to 127.0.0.1:8080 on the
	// host; the container runner overrides it to 0.0.0.0.
	Listen string `json:"listen"`

	// SandboxID identifies the Veris dependency sandbox this proxy routes to.
	SandboxID string `json:"sandbox_id"`

	// EnvID is the durable per-project environment. Carried through for
	// logging and for the canary response so a developer can confirm which
	// environment a run actually used.
	EnvID string `json:"env_id,omitempty"`

	Mode Mode `json:"mode"`

	// CADir holds the interception CA. Defaults to ~/.veris/ca.
	CADir string `json:"ca_dir"`

	Upstream Upstream `json:"upstream"`

	Services []Service `json:"services"`

	// AllowPassthrough lists hosts that bypass interception entirely even in
	// strict mode. Loopback is always included; anything else is the operator's
	// explicit choice.
	AllowPassthrough []string `json:"allow_passthrough,omitempty"`

	// CanaryToken is minted per run by the CLI. The test process asserts on it
	// to prove interception is live for *this* configuration, not a stale proxy
	// left running from an earlier run.
	CanaryToken string `json:"canary_token,omitempty"`
}

// Upstream describes how to reach the Veris sandbox.
type Upstream struct {
	// BaseURL is the Veris sandbox ingress, e.g. https://sandbox.veris.ai.
	BaseURL string `json:"base_url"`

	// AuthValueEnv names an environment variable holding the credential. The
	// value itself is deliberately never written to the config file, so the
	// config can be committed or logged without leaking anything.
	AuthValueEnv string `json:"auth_value_env,omitempty"`

	// InsecureSkipVerify disables upstream TLS verification. Only for pointing
	// at a local sandbox with a self-signed cert during development.
	InsecureSkipVerify bool `json:"insecure_skip_verify,omitempty"`
}

// Service maps one or more real hostnames onto a simulated dependency.
type Service struct {
	// Name matches the service name in veris.yaml.
	Name string `json:"name"`

	// Hosts are the real hostnames the code under test believes it is calling.
	// Exact matches and a single leading "*." wildcard are supported.
	Hosts []string `json:"hosts"`

	// Upstream optionally overrides the derived target for this service.
	Upstream string `json:"upstream,omitempty"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.Mode == "" {
		c.Mode = ModeStrict
	}
	c.AllowPassthrough = expandPresets(c.AllowPassthrough)
}

// BuildHostsPreset is what "@build" in allow_passthrough expands to: the
// package registries a build tool needs during dependency resolution.
//
// Without this, running tests behind the strict proxy needs a two-phase
// dance — resolve dependencies with interception off, then run with the
// build tool forced offline — because the registries are unmapped hosts and
// strict mode rightly blocks them. Listing them here keeps the strict-mode
// guarantee auditable: these exact hosts and nothing else bypass the sandbox.
//
// The list is deliberately conservative: first-party registry hosts only, no
// CDN wildcards and no cloud-storage buckets that also serve arbitrary
// content. A project on a private registry adds its own host next to the
// preset.
var BuildHostsPreset = []string{
	// Maven / Gradle. plugins.gradle.org answers metadata but redirects
	// artifact downloads to plugins-artifacts.gradle.org; missing either one
	// fails resolution with a 502 that names the host.
	"repo.maven.apache.org", "repo1.maven.org",
	"services.gradle.org", "downloads.gradle.org",
	"plugins.gradle.org", "plugins-artifacts.gradle.org",
	// npm / yarn
	"registry.npmjs.org", "registry.yarnpkg.com",
	// Python
	"pypi.org", "files.pythonhosted.org",
	// Go
	"proxy.golang.org", "sum.golang.org",
	// Rust
	"crates.io", "index.crates.io", "static.crates.io",
	// Ruby
	"rubygems.org", "index.rubygems.org",
	// .NET
	"api.nuget.org",
	// PHP
	"repo.packagist.org",
}

func expandPresets(entries []string) []string {
	var out []string
	for _, e := range entries {
		if strings.TrimSpace(e) == "@build" {
			out = append(out, BuildHostsPreset...)
			continue
		}
		out = append(out, e)
	}
	return out
}

// Validate rejects configurations that would silently misbehave at runtime.
// Every check here exists because the corresponding failure would otherwise
// surface as a confusing TLS or DNS error deep inside a test run.
func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d (this binary understands version 1)", c.Version)
	}
	if c.Mode != ModeStrict && c.Mode != ModePassthrough {
		return fmt.Errorf("mode must be %q or %q, got %q", ModeStrict, ModePassthrough, c.Mode)
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q is not host:port: %w", c.Listen, err)
	}
	if c.SandboxID == "" {
		return fmt.Errorf("sandbox_id is required")
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("no services configured: the proxy would block every request")
	}

	if c.Upstream.BaseURL != "" {
		u, err := url.Parse(c.Upstream.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("upstream.base_url %q is not an absolute URL", c.Upstream.BaseURL)
		}
	}

	// applyDefaults has already expanded known presets, so any "@" entry left
	// is a typo, and a typo here would silently fail open or closed at runtime.
	for _, p := range c.AllowPassthrough {
		if strings.HasPrefix(strings.TrimSpace(p), "@") {
			return fmt.Errorf("allow_passthrough entry %q: unknown preset (only \"@build\" is defined)", p)
		}
	}

	seen := map[string]string{}
	for i, s := range c.Services {
		if s.Name == "" {
			return fmt.Errorf("services[%d]: name is required", i)
		}
		if len(s.Hosts) == 0 {
			return fmt.Errorf("service %q: at least one host is required", s.Name)
		}
		if s.Upstream == "" && c.Upstream.BaseURL == "" {
			return fmt.Errorf("service %q: no upstream, and upstream.base_url is unset", s.Name)
		}
		if s.Upstream != "" {
			u, err := url.Parse(s.Upstream)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("service %q: upstream %q is not an absolute URL", s.Name, s.Upstream)
			}
		}
		for _, h := range s.Hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				return fmt.Errorf("service %q: empty host entry", s.Name)
			}
			if strings.Contains(h, "*") && !strings.HasPrefix(h, "*.") {
				return fmt.Errorf("service %q: host %q may only use a wildcard as a leading \"*.\"", s.Name, h)
			}
			// Two services claiming the same hostname is always a config bug,
			// and resolving it by declaration order would be arbitrary.
			if prev, dup := seen[h]; dup {
				return fmt.Errorf("host %q is claimed by both %q and %q", h, prev, s.Name)
			}
			seen[h] = s.Name
		}
	}
	return nil
}

// Target is the resolved routing decision for a single hostname.
type Target struct {
	Service  string
	Upstream *url.URL
}

// Resolve maps a request host onto a service upstream.
//
// Exact matches always beat wildcards, and among wildcards the longest suffix
// wins, so "api.stripe.com" can be routed differently from "*.stripe.com".
func (c *Config) Resolve(host string) (*Target, bool) {
	name := strings.ToLower(stripPort(host))

	var (
		best     *Service
		bestSpec int // -1 exact, otherwise negative wildcard length; higher is better
	)
	for i := range c.Services {
		s := &c.Services[i]
		for _, pattern := range s.Hosts {
			pattern = strings.ToLower(strings.TrimSpace(pattern))
			score, ok := matchScore(pattern, name)
			if !ok {
				continue
			}
			if best == nil || score > bestSpec {
				best, bestSpec = s, score
			}
		}
	}
	if best == nil {
		return nil, false
	}

	raw := best.Upstream
	if raw == "" {
		raw = strings.TrimSuffix(c.Upstream.BaseURL, "/") +
			"/s/" + url.PathEscape(c.SandboxID) +
			"/" + url.PathEscape(best.Name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Validate already rejected malformed upstreams, so this is
		// unreachable in practice.
		return nil, false
	}
	return &Target{Service: best.Name, Upstream: u}, true
}

// matchScore reports whether pattern matches host, and how specific the match
// is. Larger is more specific.
func matchScore(pattern, host string) (int, bool) {
	if pattern == host {
		return 1 << 30, true
	}
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		// "*.stripe.com" matches "api.stripe.com" and also bare "stripe.com",
		// which is what people mean when they write it.
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return len(suffix), true
		}
	}
	return 0, false
}

// IsPassthrough reports whether host should bypass interception entirely.
// Loopback is always allowed so that a test hitting its own service under test
// is never routed to Veris.
func (c *Config) IsPassthrough(host string) bool {
	name := strings.ToLower(stripPort(host))
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return true
	}
	if ip := net.ParseIP(name); ip != nil && ip.IsLoopback() {
		return true
	}
	for _, p := range c.AllowPassthrough {
		if _, ok := matchScore(strings.ToLower(strings.TrimSpace(p)), name); ok {
			return true
		}
	}
	return false
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
