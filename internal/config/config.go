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

	"github.com/veris-ai/veris-proxy/internal/hostport"
)

// Mode controls what happens to a host with no matching service.
type Mode string

const (
	// ModeStrict blocks unmapped hosts. Opt in when a run must prove the code
	// under test reached nothing but the sandbox.
	ModeStrict Mode = "strict"

	// ModePassthrough forwards unmapped hosts to their real destination, and is
	// the default: only the services a sandbox actually provisions are
	// rerouted, and everything else -- telemetry, package registries, an
	// internal API -- behaves as it always did. A proxy that blocks whatever it
	// was not told about makes adoption a configuration project.
	//
	// What strict mode was protecting against is real, but the receipt answers
	// it directly rather than by blocking: the run reports what the sandbox
	// received, so a suite that quietly talked to the real vendor is visible
	// without having to forbid every host nobody listed.
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

	// PassEnv is the sandbox surface the proxy cannot intercept: a database
	// service's URL is a DSN, a wire protocol this proxy does not speak, and
	// the client code that consumes it already reads it from an environment
	// variable in production. So the value is HANDED to the command under
	// that exact variable (the platform's env_hint) instead of routed.
	PassEnv []PassEnvVar `json:"pass_env,omitempty"`
}

// PassEnvVar is one environment variable handed to the command under test.
type PassEnvVar struct {
	// Name is the variable the client's code already reads (DATABASE_URL).
	Name string `json:"name"`
	// Value is the sandbox's own connection string, verbatim.
	Value string `json:"value"`
	// Service names where the value came from, for the announcement.
	Service string `json:"service"`
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

	// Paths narrows this entry to request paths under one of these prefixes.
	// A vendor that fronts several services on one hostname needs it: Google
	// serves Calendar, Drive and its identity endpoints on
	// www.googleapis.com, told apart only by prefix. Empty means the entry
	// claims the whole host.
	Paths []string `json:"paths,omitempty"`

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
		c.Mode = ModePassthrough
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
	if len(c.Services) == 0 && len(c.PassEnv) == 0 {
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
			// Two services claiming the same host AND prefix is always a
			// config bug, and resolving it by declaration order would be
			// arbitrary. Different prefixes on one host are the point.
			for _, key := range claimKeys(h, s.Paths) {
				if prev, dup := seen[key]; dup {
					return fmt.Errorf("%s is claimed by both %q and %q", key, prev, s.Name)
				}
				seen[key] = s.Name
			}
		}
	}
	return nil
}

// claimKeys is what a service entry occupies: one key per host, or per
// host+prefix when the entry narrows to prefixes. Prefixes are normalized
// first, because "/v2" and "/v2/" route identically and so must collide.
func claimKeys(host string, paths []string) []string {
	wholeHost := fmt.Sprintf("host %q", host)
	if len(paths) == 0 {
		return []string{wholeHost}
	}
	keys := make([]string, 0, len(paths))
	for _, p := range paths {
		stem := normalizePrefix(p)
		if stem == "/" {
			// A root prefix claims everything on the host, exactly like an
			// entry with no prefixes at all, so it takes the same key. A
			// separate one would let two services claim one host and leave
			// Resolve picking whichever happened to be declared first.
			keys = append(keys, wholeHost)
			continue
		}
		keys = append(keys, fmt.Sprintf("host %q path %q", host, stem))
	}
	return keys
}

// normalizePrefix reduces a declared prefix to the stem the matcher uses.
func normalizePrefix(prefix string) string {
	stem := strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	if stem == "" {
		return "/"
	}
	return stem
}

// Target is the resolved routing decision for a single request.
type Target struct {
	Service  string
	Upstream *url.URL

	// Prefix is the path prefix that matched, or "/" when the entry claims the
	// whole host. The run receipt records it, so a client can require that a
	// specific service on a shared hostname was actually exercised.
	Prefix string
}

// Resolve maps a request host onto a service upstream.
//
// Exact matches always beat wildcards, and among wildcards the longest suffix
// wins, so "api.stripe.com" can be routed differently from "*.stripe.com".
func (c *Config) Resolve(host, path string) (*Target, bool) {
	name := strings.ToLower(hostport.StripPort(host))
	if path == "" {
		path = "/"
	}

	var (
		best       *Service
		bestHost   int
		bestPath   int
		bestPrefix string
		haveMatch  bool
	)
	for i := range c.Services {
		s := &c.Services[i]
		for _, pattern := range s.Hosts {
			pattern = strings.ToLower(strings.TrimSpace(pattern))
			hostScore, ok := matchScore(pattern, name)
			if !ok {
				continue
			}
			prefix, pScore, ok := s.matchPath(path)
			if !ok {
				continue
			}
			// Host specificity outranks path length, so an exact host with a
			// narrow prefix still beats a wildcard that claims everything.
			// Within one host, the longer prefix wins.
			if !haveMatch || hostScore > bestHost ||
				(hostScore == bestHost && pScore > bestPath) {
				best, bestHost, bestPath, bestPrefix, haveMatch = s, hostScore, pScore, prefix, true
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
	return &Target{Service: best.Name, Upstream: u, Prefix: bestPrefix}, true
}

// ServiceEndpoints is each service's base URL, which is also where its
// /veris/* control plane lives.
//
// Derived here rather than by the caller: the sandbox-and-name path shape is
// this file's knowledge, and a second copy of it would be a second thing to
// keep right when the platform moves it.
func (c *Config) ServiceEndpoints() []ServiceEndpoint {
	out := make([]ServiceEndpoint, 0, len(c.Services))
	for _, s := range c.Services {
		raw := s.Upstream
		if raw == "" {
			if c.Upstream.BaseURL == "" {
				continue
			}
			raw = strings.TrimSuffix(c.Upstream.BaseURL, "/") +
				"/s/" + url.PathEscape(c.SandboxID) +
				"/" + url.PathEscape(s.Name)
		}
		out = append(out, ServiceEndpoint{Name: s.Name, BaseURL: raw})
	}
	return out
}

// ServiceEndpoint names one service and where to reach it.
type ServiceEndpoint struct {
	Name    string
	BaseURL string
}

// matchPath reports which of the service's prefixes claims path, and how
// specific that match is. A service with no prefixes claims the whole host and
// scores zero, so any explicit prefix on the same host outranks it.
func (s *Service) matchPath(path string) (string, int, bool) {
	if len(s.Paths) == 0 {
		return "/", 0, true
	}
	var (
		bestPrefix string
		best       int
		ok         bool
	)
	for _, prefix := range s.Paths {
		stem := normalizePrefix(prefix)
		score, hit := prefixScore(stem, path)
		if hit && (!ok || score > best) {
			bestPrefix, best, ok = stem, score, true
		}
	}
	return bestPrefix, best, ok
}

// prefixScore matches on a segment boundary, so "/userinfo" claims
// "/userinfo/v2/me" but not "/userinfoXYZ", and no entry needs a hand-tuned
// trailing slash to behave. stem must already be normalized.
func prefixScore(stem, path string) (int, bool) {
	if stem == "/" {
		return 0, true
	}
	if path == stem || strings.HasPrefix(path, stem+"/") {
		return len(stem), true
	}
	return 0, false
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

// HostIsMapped reports whether any service claims host, on any path. A TLS
// handshake carries no path, so this is the question a failed one poses: was
// this a name the run was supposed to exercise.
func (c *Config) HostIsMapped(host string) bool {
	name := strings.ToLower(hostport.StripPort(host))
	for i := range c.Services {
		for _, pattern := range c.Services[i].Hosts {
			if _, ok := matchScore(strings.ToLower(strings.TrimSpace(pattern)), name); ok {
				return true
			}
		}
	}
	return false
}

// IsPassthrough reports whether host should bypass interception entirely.
// Loopback is always allowed so that a test hitting its own service under test
// is never routed to Veris.
func (c *Config) IsPassthrough(host string) bool {
	name := strings.ToLower(hostport.StripPort(host))
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
