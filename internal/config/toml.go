package config

// .veris.toml is the committed, team-shared face of the same configuration
// proxy.json carries on the wire. The split is deliberate:
//
//   - .veris.toml holds what is durable and shareable: the environment id,
//     the service-to-hostname map, passthrough rules, and a [run] table that
//     records once-per-project decisions (where tests execute, the test
//     command). No secrets, safe to commit.
//   - What is per-run — sandbox_id and the canary token — must never be
//     committed: a committed canary would defeat stale-proxy detection.
//     Those arrive as --sandbox-id and --canary flags on serve and env.
//
// The [run] table is owned by the integration-testing skill, not by this
// binary; it is parsed permissively and ignored so the skill can grow
// metadata without breaking older proxies.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type tomlFile struct {
	Veris struct {
		EnvID  string `toml:"env_id"`
		APIURL string `toml:"api_url"` // control plane; not used by the proxy
	} `toml:"veris"`
	Proxy struct {
		Listen             string   `toml:"listen"`
		Mode               string   `toml:"mode"`
		CADir              string   `toml:"ca_dir"`
		AllowPassthrough   []string `toml:"allow_passthrough"`
		UpstreamBaseURL    string   `toml:"upstream_base_url"`
		AuthValueEnv       string   `toml:"auth_value_env"`
		InsecureSkipVerify bool     `toml:"insecure_skip_verify"`
	} `toml:"proxy"`
	Services map[string]struct {
		Hosts    []string `toml:"hosts"`
		Upstream string   `toml:"upstream"`
	} `toml:"services"`
	// Owned by the skill layer; tolerated and ignored here.
	Run map[string]any `toml:"run"`
}

func parseTOML(raw []byte, path string) (*Config, error) {
	var f tomlFile
	if err := toml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	c := &Config{
		Version:          1,
		Listen:           f.Proxy.Listen,
		EnvID:            f.Veris.EnvID,
		Mode:             Mode(f.Proxy.Mode),
		CADir:            f.Proxy.CADir,
		AllowPassthrough: f.Proxy.AllowPassthrough,
		Upstream: Upstream{
			BaseURL:            f.Proxy.UpstreamBaseURL,
			AuthValueEnv:       f.Proxy.AuthValueEnv,
			InsecureSkipVerify: f.Proxy.InsecureSkipVerify,
		},
	}

	// Map order is random; sort so error messages, logs and routing tables
	// come out the same on every load.
	names := make([]string, 0, len(f.Services))
	for name := range f.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s := f.Services[name]
		c.Services = append(c.Services, Service{Name: name, Hosts: s.Hosts, Upstream: s.Upstream})
	}
	return c, nil
}

func isTOMLPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".toml")
}
