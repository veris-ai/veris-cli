package cfg

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SandboxRef is the sandbox this checkout last created, kept so `run` can
// reuse it and `sandbox delete` knows which one is ours.
type SandboxRef struct {
	ID            string `yaml:"id,omitempty"`
	EnvironmentID string `yaml:"environment_id,omitempty"`
	CreatedAt     string `yaml:"created_at,omitempty"`
	ExpiresAt     string `yaml:"expires_at,omitempty"`
}

// BaselineRef records one promotion: which sandbox became which environment's
// baseline, and when, so `baseline list` can show history the control plane
// does not keep.
type BaselineRef struct {
	EnvironmentID string `yaml:"environment_id,omitempty"`
	Revision      string `yaml:"revision,omitempty"`
	Image         string `yaml:"image,omitempty"`
	PromotedAt    string `yaml:"promoted_at,omitempty"`
	SourceSandbox string `yaml:"source_sandbox,omitempty"`
}

// Local is .veris/twin.local.yaml: what is true of this checkout and nobody
// else's. It is 0600 because a sandbox id is a capability, and gitignored
// because committing it would hand one developer's sandbox to every other.
type Local struct {
	// Use is the environment chosen for this checkout with `veris env use`.
	Use         string        `yaml:"use,omitempty"`
	Sandbox     *SandboxRef   `yaml:"sandbox,omitempty"`
	CallbackURL string        `yaml:"callback_url,omitempty"`
	Baselines   []BaselineRef `yaml:"baselines,omitempty"`
	// Path is where this was read from and will be saved to.
	Path string `yaml:"-"`
}

// LoadLocal reads the local file. Missing is the common case -- nothing has
// been chosen or created here yet -- and yields an empty Local that saves to
// the same path.
func LoadLocal(path string) (*Local, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	l := &Local{Path: abs}
	raw, err := os.ReadFile(abs)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(raw, l); err != nil {
		return nil, unreadable(abs, err)
	}
	l.Path = abs
	return l, nil
}

// Save writes the local file atomically at 0600 and reports whether git
// ignores it. It refuses where no project file sits beside it: a local file
// with no project is a .veris/ directory that nothing will ever read, left in
// whatever folder the command happened to run in.
//
// ignored is true when git is not installed or the directory is not a
// repository, so the only time a caller warns is when a commit could really
// pick the file up. The caller prints the warning; this package never writes
// to a terminal.
func (l *Local) Save() (ignored bool, err error) {
	if l.Path == "" {
		return false, errors.New("local file has no path")
	}
	project := filepath.Join(filepath.Dir(l.Path), projectFileName)
	if !fileExists(project) {
		return false, fmt.Errorf(
			"no project file at %s; run veris init before anything is chosen here", project)
	}
	body, err := yaml.Marshal(l)
	if err != nil {
		return false, fmt.Errorf("encode %s: %w", l.Path, err)
	}
	if err := writeAtomic(l.Path, body, 0o600, 0o700); err != nil {
		return false, err
	}
	return gitIgnores(filepath.Dir(l.Path), l.Path), nil
}

// EnsureIgnored adds this project's twin.local.yaml to the repository's root
// .gitignore when git does not already ignore it, and reports whether it
// wrote. Outside a repository, or without git, there is nothing to protect
// against and it writes nothing. The line is the path relative to the
// repository root, which is the literal ".veris/twin.local.yaml" when the
// project sits at the root and stays correct when it does not: a pattern with
// a slash in it is anchored where the .gitignore lives.
func EnsureIgnored(projectDir string) (wrote bool, err error) {
	root, ok := gitRoot(projectDir)
	if !ok {
		return false, nil
	}
	local := filepath.Join(projectDir, projectDirName, localFileName)
	if gitIgnores(projectDir, local) {
		return false, nil
	}
	// git reports the root absolute with symlinks resolved (/private/var on
	// macOS, for a /var path); the project dir must be made the same shape
	// or Rel has nothing to relate: EvalSymlinks keeps a relative path
	// relative, and Rel refuses to mix.
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return false, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, filepath.Join(real, projectDirName, localFileName))
	if err != nil {
		return false, err
	}
	line := filepath.ToSlash(rel)

	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// The line may already be present and overridden by a later negation;
	// adding it again would change nothing and grow the file on every call.
	for _, have := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(have) == line {
			return false, nil
		}
	}
	var add bytes.Buffer
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		add.WriteByte('\n')
	}
	add.WriteString(line + "\n")
	// Appended in place rather than rewritten: .gitignore is the user's
	// file. A rename through writeAtomic would drop whatever mode and
	// ownership they gave it, and a truncate-then-write would leave it empty
	// if the write failed halfway. O_APPEND touches nothing that is there.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(add.Bytes()); err != nil {
		f.Close()
		return false, err
	}
	return true, f.Close()
}

// gitIgnores asks git whether it would ignore path. Every answer but a clean
// "no" is "yes": git missing, no repository, an unreadable index -- none of
// those can end in the file being committed, so none deserve a warning.
func gitIgnores(dir, path string) bool {
	git, err := exec.LookPath("git")
	if err != nil {
		return true
	}
	err = exec.Command(git, "-C", dir, "check-ignore", "-q", "--", path).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false
	}
	return true
}

// gitRoot is the top of the repository dir is in, and false when it is in
// none or git is not installed.
func gitRoot(dir string) (string, bool) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(git, "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", false
	}
	return root, true
}
