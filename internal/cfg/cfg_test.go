package cfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempHome points $HOME at a fresh directory, so GlobalPath lands in it and
// nothing in these tests can touch the developer's own ~/.veris. Git's own
// per-user config moves with it -- every spelling of it, since git also
// reads $XDG_CONFIG_HOME/git/{config,ignore} and $GIT_CONFIG_GLOBAL -- which
// is what keeps a developer's global excludes file (one that ignores .veris/,
// say) from deciding the check-ignore cases.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// writeFile lays down one fixture, making its directory.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project lays out <root>/.veris/twin.yaml with body and returns root.
func project(t *testing.T, root, body string) string {
	t.Helper()
	writeFile(t, filepath.Join(root, ".veris", "twin.yaml"), body)
	return root
}

func mustMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}

// noTempFiles asserts an atomic write cleaned up after itself.
func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", filepath.Join(dir, e.Name()))
		}
	}
}

func TestWriteAtomicReplacesWholeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "f")
	if err := writeAtomic(path, []byte("first, and rather long\n"), 0o644, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("2\n"), 0o600, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "2\n" {
		t.Errorf("content = %q, want a clean replacement", got)
	}
	mustMode(t, path, 0o600)
	noTempFiles(t, filepath.Dir(path))
}
