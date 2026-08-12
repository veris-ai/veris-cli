// Package discovery turns a sandbox id into a working proxy configuration by
// asking the Veris control plane what that sandbox is running.
//
// Without it every project keeps a proxy.json listing the vendor hostnames its
// dependencies answer on -- facts the repository already measures and the
// control plane already knows. Naming a sandbox with --sandbox fetches its
// services once, caches the answer under ~/.veris, and lets every later command
// run with no config file at all.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/routes"
)

// DefaultAPIBase is the control plane this binary talks to unless told
// otherwise.
const DefaultAPIBase = "https://api.veris.ai"

// Environment variables. The API key is read from the environment and NEVER
// written to disk: a cached sandbox snapshot is state, not a credential store.
const (
	EnvAPIBase = "VERIS_API_BASE"
	EnvAPIKey  = "VERIS_API_KEY"
	EnvConfig  = "VERIS_PROXY_CONFIG"
	// EnvSandboxID is also set in the environment of anything `run` launches,
	// so a nested run inherits the same sandbox instead of picking its own.
	EnvSandboxID = "VERIS_SANDBOX_ID"
)

// Service is one simulated dependency in a sandbox, as the control plane
// describes it.
type Service struct {
	Name string `json:"name"`
	// URL is where the code under test's traffic must be sent. Used verbatim:
	// the control plane already knows the exact address, and rebuilding it here
	// would be a second implementation of a routing rule it owns.
	URL     string `json:"url"`
	Status  string `json:"status"`
	EnvHint string `json:"env_hint"`
	// Routes are the real hostnames this service answers for, when the control
	// plane serves them. Same provenance as the embedded table -- generated
	// from the measured parity backends, never authored -- but they arrive
	// with the sandbox, so a service added to the platform is routable the day
	// it lands instead of waiting for the next proxy release. Older control
	// planes omit the field; the embedded table then decides.
	Routes []routes.Entry `json:"routes,omitempty"`
}

// Sandbox is the control plane's description of one sandbox.
type Sandbox struct {
	ID            string    `json:"id"`
	EnvironmentID string    `json:"environment_id"`
	Status        string    `json:"status"`
	Services      []Service `json:"services"`
}

// Snapshot is what gets cached: the control plane's answer plus where and when
// it came from, so a stale config can say so rather than mislead.
type Snapshot struct {
	SandboxID     string    `json:"sandbox_id"`
	EnvironmentID string    `json:"environment_id"`
	Status        string    `json:"status"`
	APIBase       string    `json:"api_base"`
	FetchedAt     time.Time `json:"fetched_at"`
	Services      []Service `json:"services"`
}

// Client talks to the Veris control plane.
type Client struct {
	APIBase string
	APIKey  string
	HTTP    *http.Client
}

// NewClient reads the control-plane coordinates from the environment.
func NewClient(apiBase, apiKey string) (*Client, error) {
	if apiBase == "" {
		apiBase = os.Getenv(EnvAPIBase)
	}
	if apiBase == "" {
		apiBase = DefaultAPIBase
	}
	if apiKey == "" {
		apiKey = os.Getenv(EnvAPIKey)
	}
	if apiKey == "" {
		return nil, fmt.Errorf(
			"no API key: set %s, or pass --api-key. It is used to read the "+
				"sandbox and is never written to disk", EnvAPIKey)
	}
	if _, err := url.Parse(apiBase); err != nil {
		return nil, fmt.Errorf("api base %q is not a URL: %w", apiBase, err)
	}
	return &Client{
		APIBase: strings.TrimSuffix(apiBase, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// Fetch reads one sandbox.
func (c *Client) Fetch(ctx context.Context, sandboxID string) (*Sandbox, error) {
	endpoint := fmt.Sprintf("%s/v1/sandboxes/%s", c.APIBase, url.PathEscape(sandboxID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the control plane at %s: %w", c.APIBase, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read sandbox %s: %w", sandboxID, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("no sandbox %s at %s", sandboxID, c.APIBase)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("the control plane refused the API key (%d)", resp.StatusCode)
	case resp.StatusCode >= 300:
		return nil, fmt.Errorf("control plane returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Deliberately NOT DisallowUnknownFields: a control plane must be able to
	// add response fields without breaking every proxy in the field.
	var sandbox Sandbox
	if err := json.Unmarshal(body, &sandbox); err != nil {
		return nil, fmt.Errorf("control plane response is not a sandbox: %w", err)
	}
	if sandbox.ID == "" {
		sandbox.ID = sandboxID
	}
	return &sandbox, nil
}

// --- state on disk ----------------------------------------------------------

// Dir is where cached sandbox descriptions live, beside the CA.
func Dir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".veris")
	}
	return ".veris"
}

func snapshotPath(sandboxID string) string {
	return filepath.Join(Dir(), "sandboxes", safeFileName(sandboxID)+".json")
}

// safeFileName keeps an id from escaping the cache directory. Ids come from a
// flag, an environment variable and a control-plane response, and the cache is
// written while `serve --transparent` is still root -- so "../../etc/x" would
// otherwise name a file outside it.
func safeFileName(id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
	if clean == "" {
		return "unnamed"
	}
	return clean
}

// SnapshotFor is how a command resolves one sandbox id into its services.
//
// The cache is preferred so an ordinary run costs no network call and needs no
// API key -- only the first sight of a sandbox does. Passing refresh, or naming
// a sandbox never seen before, goes to the control plane.
func SnapshotFor(ctx context.Context, sandboxID, apiBase, apiKey string, refresh bool) (*Snapshot, error) {
	if !refresh {
		if snapshot, err := LoadSnapshot(sandboxID); err == nil {
			return snapshot, nil
		}
	}

	client, err := NewClient(apiBase, apiKey)
	if err != nil {
		if refresh {
			return nil, err
		}
		return nil, fmt.Errorf(
			"sandbox %s has not been fetched yet and cannot be: %w", sandboxID, err)
	}
	sandbox, err := client.Fetch(ctx, sandboxID)
	if err != nil {
		return nil, err
	}

	snapshot := &Snapshot{
		SandboxID:     sandbox.ID,
		EnvironmentID: sandbox.EnvironmentID,
		Status:        sandbox.Status,
		APIBase:       client.APIBase,
		FetchedAt:     time.Now().UTC(),
		Services:      sandbox.Services,
	}
	// Cached but NOT selected: naming a sandbox on one command must not change
	// which one every other command uses.
	if err := writeJSON(snapshotPath(snapshot.SandboxID), snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// LoadSnapshot reads the cached description of a sandbox.
func LoadSnapshot(sandboxID string) (*Snapshot, error) {
	raw, err := os.ReadFile(snapshotPath(sandboxID))
	if err != nil {
		return nil, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", snapshotPath(sandboxID), err)
	}
	// The filename is sanitised, so two ids can share one. Reading back the id
	// the file claims turns a silent wrong-sandbox route into a cache miss.
	if snapshot.SandboxID != sandboxID {
		return nil, fmt.Errorf("cached under %s but holds %s",
			sandboxID, snapshot.SandboxID)
	}
	return &snapshot, nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// Written atomically so an interrupted write cannot leave a half-parsed
	// selection behind, and 0600 because it names the user's sandboxes.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// --- turning a snapshot into a config ---------------------------------------

// Unroutable explains why one of a sandbox's services got no interception
// entry, so `use` can say so rather than quietly cover fewer dependencies than
// the sandbox runs.
type Unroutable struct {
	Service string
	Reason  string
}

// ToConfig builds a proxy configuration from a cached sandbox.
//
// Each service's upstream is the URL the control plane returned, used verbatim.
// Which real hostnames map onto it resolves most-explicit first:
//
//	--route overrides  >  routes the control plane served  >  the embedded table
//
// All three carry the same kind of fact; they differ in freshness and intent.
// An override is the operator saying "I know better for this run"; the control
// plane's routes are the platform's current measured record; the embedded
// table is that same record frozen at this binary's release, kept as the
// fallback for control planes that do not serve routes yet.
func ToConfig(snapshot *Snapshot, overrides map[string][]routes.Entry) (*config.Config, []Unroutable, error) {
	cfg := &config.Config{
		Version:   1,
		Listen:    "127.0.0.1:0",
		SandboxID: snapshot.SandboxID,
		EnvID:     snapshot.EnvironmentID,
		Mode:      config.ModePassthrough,
	}

	// Every override must land on a service the sandbox runs. A typo'd
	// --route silently ignored would leave its author believing a dependency
	// was covered -- the exact lie this proxy exists to prevent.
	unclaimed := make(map[string]bool, len(overrides))
	for name := range overrides {
		unclaimed[name] = true
	}

	var skipped []Unroutable
	for _, svc := range snapshot.Services {
		if !isHTTP(svc.URL) {
			// A Postgres DSN is a wire protocol this proxy does not speak. It
			// is reached directly — and the client code that reads it already
			// reads it from an environment variable in production, so the
			// faithful move is to hand it over under that exact name.
			if svc.EnvHint != "" {
				cfg.PassEnv = append(cfg.PassEnv, config.PassEnvVar{
					Name: svc.EnvHint, Value: svc.URL, Service: svc.Name,
				})
				skipped = append(skipped, Unroutable{svc.Name,
					"not proxied (" + schemeOf(svc.URL) +
						" is not http); handed to the command as $" + svc.EnvHint})
				continue
			}
			skipped = append(skipped, Unroutable{svc.Name,
				"not an http service (" + schemeOf(svc.URL) + ")"})
			continue
		}
		entries, source := routesFor(svc, overrides)
		if source == "--route" {
			delete(unclaimed, svc.Name)
		}
		if len(entries) == 0 {
			skipped = append(skipped, Unroutable{svc.Name,
				"no route: the control plane served none and this binary's " +
					"table has no measured hostname for it (--route " +
					svc.Name + "=<host> supplies one for this run)"})
			continue
		}
		for _, entry := range entries {
			cfg.Services = append(cfg.Services, config.Service{
				Name:     svc.Name,
				Hosts:    []string{entry.Host},
				Paths:    entry.Paths,
				Upstream: strings.TrimSuffix(svc.URL, "/"),
			})
		}
	}

	if len(unclaimed) > 0 {
		names := make([]string, 0, len(unclaimed))
		for name := range unclaimed {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, skipped, fmt.Errorf(
			"--route names %s, which sandbox %s does not run (it runs: %s)",
			strings.Join(names, ", "), snapshot.SandboxID, serviceNames(snapshot))
	}

	// A sandbox holding only pass-env services (a lone database) is a real
	// shape: nothing is intercepted, but the run still hands the DSN over and
	// the empty receipt says truthfully that no HTTP reached the sandbox.
	if len(cfg.Services) == 0 && len(cfg.PassEnv) == 0 {
		return nil, skipped, fmt.Errorf(
			"sandbox %s has no service this proxy can route; nothing would be intercepted",
			snapshot.SandboxID)
	}
	if err := cfg.Validate(); err != nil {
		return nil, skipped, fmt.Errorf("the derived config is not valid: %w", err)
	}
	return cfg, skipped, nil
}

// routesFor picks one source of routes for a service and names it. The
// sources never merge: a config assembled from two records could not be
// reasoned about from either, which is the same argument resolveConfig makes
// about its layers.
func routesFor(svc Service, overrides map[string][]routes.Entry) ([]routes.Entry, string) {
	if entries := overrides[svc.Name]; len(entries) > 0 {
		return entries, "--route"
	}
	// Entries without a host are dropped rather than fatal: one malformed row
	// from a newer control plane must not take down every run against it. A
	// service whose rows ALL drop falls through to the embedded table.
	if served := compactRoutes(svc.Routes); len(served) > 0 {
		return served, "control plane"
	}
	if entries, ok := routes.For(svc.Name); ok {
		return entries, "embedded table"
	}
	return nil, ""
}

func compactRoutes(entries []routes.Entry) []routes.Entry {
	kept := make([]routes.Entry, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.Host) != "" {
			kept = append(kept, e)
		}
	}
	return kept
}

func serviceNames(snapshot *Snapshot) string {
	names := make([]string, 0, len(snapshot.Services))
	for _, svc := range snapshot.Services {
		names = append(names, svc.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func schemeOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		return u.Scheme
	}
	return "unknown scheme"
}

func isHTTP(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
