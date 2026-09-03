package cfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ProxyConfig is how an environment wants the proxy run: what the run must
// have reached, what to expose for callbacks, which image to test in.
type ProxyConfig struct {
	RequireService  []string `yaml:"require_service,omitempty"`
	RequireCallback []string `yaml:"require_callback,omitempty"`
	Expose          int      `yaml:"expose,omitempty"`
	Image           string   `yaml:"image,omitempty"`
	Strict          bool     `yaml:"strict,omitempty"`
}

// RunConfig is the command `veris run` launches when none is given.
type RunConfig struct {
	Command []string `yaml:"command,omitempty"`
}

// EnvConfig is one named environment as the project describes it.
type EnvConfig struct {
	ID string `yaml:"id,omitempty"`
	// Profile names the login this environment lives on, for a project whose
	// environments are split across control planes. Empty means whichever
	// profile is otherwise selected.
	Profile    string `yaml:"profile,omitempty"`
	TTLMinutes int    `yaml:"ttl_minutes,omitempty"`
	// Boot is bundle, baseline or snapshot; Snapshot names which when it is
	// snapshot.
	Boot        string      `yaml:"boot,omitempty"`
	Snapshot    string      `yaml:"snapshot,omitempty"`
	Data        []string    `yaml:"data,omitempty"`
	CallbackURL string      `yaml:"callback_url,omitempty"`
	Proxy       ProxyConfig `yaml:"proxy,omitempty"`
	Run         RunConfig   `yaml:"run,omitempty"`
}

// Project is .veris/twin.yaml, the committed file.
type Project struct {
	Version      int                  `yaml:"version"`
	Project      string               `yaml:"project,omitempty"`
	Default      string               `yaml:"default,omitempty"`
	Environments map[string]EnvConfig `yaml:"environments,omitempty"`
	// Path is the absolute path of .veris/twin.yaml.
	Path string `yaml:"-"`
}

// FindProject walks up from start to the filesystem root and loads the first
// .veris/twin.yaml it meets, so a command run from a subdirectory finds the
// same project as one run from the top. (nil, nil) means no directory on the
// way up has one -- an ordinary state, not an error. An empty start means the
// working directory.
func FindProject(start string) (*Project, error) {
	if start == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		start = wd
	}
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	// The global file is ~/.veris/twin.yaml, the same name under the same
	// directory name, and a walk from anywhere under $HOME passes $HOME. It
	// is not a project: loading it as one would root a bogus empty project
	// at the home directory and let Local.Save write ~/.veris/twin.local.yaml.
	global := globalDir()
	for {
		if !sameDir(filepath.Join(dir, projectDirName), global) {
			candidate := filepath.Join(dir, projectDirName, projectFileName)
			if fileExists(candidate) {
				return LoadProject(candidate)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil
		}
		dir = parent
	}
}

// LoadProject reads one project file. Unlike FindProject, a missing file here
// is an error: the caller named it.
func LoadProject(path string) (*Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	p := &Project{}
	if err := yaml.Unmarshal(raw, p); err != nil {
		return nil, unreadable(abs, err)
	}
	p.Path = abs
	return p, nil
}

// Save writes the project file atomically at 0644: it is committed and read
// by everyone who checks the repository out, so it carries nothing secret.
func (p *Project) Save() error {
	if p.Path == "" {
		return errors.New("project file has no path")
	}
	body, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode %s: %w", p.Path, err)
	}
	return writeAtomic(p.Path, body, 0o644, 0o755)
}

// Dir is the directory that holds .veris/ -- the project root.
func (p *Project) Dir() string {
	return filepath.Dir(filepath.Dir(p.Path))
}

// LocalPath is where this project's .veris/twin.local.yaml lives, whether or
// not it exists yet.
func (p *Project) LocalPath() string {
	return filepath.Join(filepath.Dir(p.Path), localFileName)
}

// sameDir reports whether a and b are one directory, by identity rather
// than spelling, so a symlinked or differently-cased $HOME still matches.
// A path that does not exist is nobody's directory.
func sameDir(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ia, ib)
}
