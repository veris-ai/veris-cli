package cfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Profile is one login to one control plane. A user who works against
// production and a staging plane keeps two, and picks with --profile.
type Profile struct {
	APIBase string `yaml:"api_base,omitempty"`
	APIKey  string `yaml:"api_key,omitempty"`
	// ConsoleURL comes from the device-code response and is used only to
	// print "→" links; nothing is fetched from it.
	ConsoleURL     string `yaml:"console_url,omitempty"`
	KeyID          string `yaml:"key_id,omitempty"`
	KeyName        string `yaml:"key_name,omitempty"`
	OrganizationID string `yaml:"organization_id,omitempty"`
	// DefaultEnvironment is the environment name or id used in a folder that
	// has no project file of its own.
	DefaultEnvironment string `yaml:"default_environment,omitempty"`
}

// Global is ~/.veris/twin.yaml.
type Global struct {
	ActiveProfile string             `yaml:"active_profile,omitempty"`
	Profiles      map[string]Profile `yaml:"profiles,omitempty"`
	// Path is where this was read from and will be saved to.
	Path string `yaml:"-"`
}

// GlobalPath is where the profiles live. It resolves exactly as the engine's
// discovery.Dir does -- $HOME/.veris, else .veris relative -- so both halves
// of the binary agree on which directory is "the user's".
func GlobalPath() string {
	return filepath.Join(globalDir(), globalFileName)
}

func globalDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, projectDirName)
	}
	return projectDirName
}

// LoadGlobal reads the profiles file. A missing file is not an error: a fresh
// machine has none, and every command that needs a login says so itself with
// a more useful message than "no such file". A file that exists but does not
// parse is an error naming the path, because silently treating it as empty
// would send the user to log in again over a stray tab.
func LoadGlobal() (*Global, error) {
	path := GlobalPath()
	g := &Global{Path: path, Profiles: map[string]Profile{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return g, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(raw, g); err != nil {
		return nil, unreadable(path, err)
	}
	if g.Profiles == nil {
		g.Profiles = map[string]Profile{}
	}
	g.Path = path
	return g, nil
}

// Save writes the profiles file atomically at 0600 and makes ~/.veris 0700 if
// it is missing. Only twin.yaml is touched: the same directory holds the
// Python CLI's config.yaml and the engine's CA and sandbox cache, none of
// which this package may read or write.
func (g *Global) Save() error {
	if g.Path == "" {
		g.Path = GlobalPath()
	}
	body, err := yaml.Marshal(g)
	if err != nil {
		return fmt.Errorf("encode %s: %w", g.Path, err)
	}
	return writeAtomic(g.Path, body, 0o600, 0o700)
}

// Active is the profile active_profile names, or "default" when it names
// none. ok is false when that profile does not exist, which is the state of
// a machine that has never logged in.
func (g *Global) Active() (name string, p Profile, ok bool) {
	name = g.ActiveProfile
	if name == "" {
		name = "default"
	}
	p, ok = g.Profiles[name]
	return name, p, ok
}
