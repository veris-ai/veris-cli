package cfg

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadLocalMissingIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".veris", "twin.local.yaml")
	l, err := LoadLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != path || l.Use != "" || l.Sandbox != nil || len(l.Baselines) != 0 {
		t.Errorf("missing file loaded as %+v", l)
	}
}

func TestLoadLocalDecodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".veris", "twin.local.yaml")
	writeFile(t, path, `use: ci
sandbox: {id: sb_1, environment_id: env_1, created_at: "2026-09-03T10:00:00Z", expires_at: "2026-09-03T14:00:00Z"}
callback_url: https://cb.example
baselines:
  - {environment_id: env_1, revision: r1, image: img:1, promoted_at: "2026-09-01T00:00:00Z", source_sandbox: sb_0}
`)
	l, err := LoadLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	want := &Local{
		Use: "ci",
		Sandbox: &SandboxRef{ID: "sb_1", EnvironmentID: "env_1",
			CreatedAt: "2026-09-03T10:00:00Z", ExpiresAt: "2026-09-03T14:00:00Z"},
		CallbackURL: "https://cb.example",
		Baselines: []BaselineRef{{EnvironmentID: "env_1", Revision: "r1", Image: "img:1",
			PromotedAt: "2026-09-01T00:00:00Z", SourceSandbox: "sb_0"}},
		Path: path,
	}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("decoded\n %+v\nwant\n %+v", l, want)
	}
}

func TestLoadLocalUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".veris", "twin.local.yaml")
	writeFile(t, path, "sandbox: [nope]\n")
	if _, err := LoadLocal(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Errorf("bad YAML: err = %v", err)
	}
}

func TestLocalSaveRefusesWithoutProject(t *testing.T) {
	root := t.TempDir()
	l := &Local{Use: "dev", Path: filepath.Join(root, ".veris", "twin.local.yaml")}
	if _, err := l.Save(); err == nil {
		t.Fatal("Save wrote a local file with no project file beside it")
	}
	if _, err := os.Stat(l.Path); !os.IsNotExist(err) {
		t.Errorf("file exists after a refused save: %v", err)
	}
	if _, err := (&Local{}).Save(); err == nil {
		t.Error("Save without a Path succeeded")
	}
}

func TestLocalSaveWritesPrivateAtomic(t *testing.T) {
	tempHome(t)
	root := project(t, t.TempDir(), "version: 1\n")
	l := &Local{Use: "dev", Sandbox: &SandboxRef{ID: "sb_1"},
		Path: filepath.Join(root, ".veris", "twin.local.yaml")}
	if _, err := l.Save(); err != nil {
		t.Fatal(err)
	}
	mustMode(t, l.Path, 0o600)
	noTempFiles(t, filepath.Dir(l.Path))
	raw, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "use: dev") || !strings.Contains(string(raw), "id: sb_1") {
		t.Errorf("saved:\n%s", raw)
	}
	if strings.Contains(string(raw), "baselines") || strings.Contains(string(raw), "callback_url") {
		t.Errorf("zero fields written:\n%s", raw)
	}
	back, err := LoadLocal(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, l) {
		t.Errorf("round trip changed the file:\n got %+v\nwant %+v", back, l)
	}
}

func TestLocalSaveOutsideRepoIsQuiet(t *testing.T) {
	tempHome(t)
	root := project(t, t.TempDir(), "version: 1\n")
	l := &Local{Use: "dev", Path: filepath.Join(root, ".veris", "twin.local.yaml")}
	ignored, err := l.Save()
	if err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Error("outside a repository Save reported not ignored; the caller would warn about nothing")
	}
}

// gitRepo makes a fresh repository holding a project file and returns its
// root. Skips when git is not installed.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	tempHome(t)
	root := t.TempDir()
	out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput()
	if err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return project(t, root, "version: 1\n")
}

func TestLocalSaveReportsGitIgnore(t *testing.T) {
	root := gitRepo(t)
	l := &Local{Use: "dev", Path: filepath.Join(root, ".veris", "twin.local.yaml")}

	ignored, err := l.Save()
	if err != nil {
		t.Fatal(err)
	}
	if ignored {
		t.Error("fresh repository with no .gitignore reported ignored")
	}

	writeFile(t, filepath.Join(root, ".gitignore"), ".veris/twin.local.yaml\n")
	ignored, err = l.Save()
	if err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Error("ignored by .gitignore but reported not ignored")
	}
}

func TestEnsureIgnoredAppendsOnce(t *testing.T) {
	root := gitRepo(t)
	gi := filepath.Join(root, ".gitignore")
	// An existing file without a trailing newline: the line must land on its
	// own line, not glued to the previous one.
	writeFile(t, gi, "node_modules")

	wrote, err := EnsureIgnored(root)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first EnsureIgnored wrote nothing")
	}
	raw, err := os.ReadFile(gi)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "node_modules\n.veris/twin.local.yaml\n" {
		t.Errorf(".gitignore = %q", raw)
	}

	wrote, err = EnsureIgnored(root)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("second EnsureIgnored wrote again")
	}
	after, _ := os.ReadFile(gi)
	if string(after) != string(raw) {
		t.Errorf(".gitignore changed on the second call: %q", after)
	}

	l := &Local{Use: "dev", Path: filepath.Join(root, ".veris", "twin.local.yaml")}
	ignored, err := l.Save()
	if err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Error("Save still reports not ignored after EnsureIgnored")
	}
}

// init passes the working directory, which may well be ".": the relative
// path must relate to git's absolute root without complaint.
func TestEnsureIgnoredRelativeProjectDir(t *testing.T) {
	root := gitRepo(t)
	t.Chdir(root)
	wrote, err := EnsureIgnored(".")
	if err != nil || !wrote {
		t.Fatalf("EnsureIgnored(\".\") = %v, %v", wrote, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil || string(raw) != ".veris/twin.local.yaml\n" {
		t.Errorf(".gitignore = %q, %v", raw, err)
	}
}

func TestEnsureIgnoredCreatesGitignore(t *testing.T) {
	root := gitRepo(t)
	wrote, err := EnsureIgnored(root)
	if err != nil || !wrote {
		t.Fatalf("EnsureIgnored = %v, %v", wrote, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil || string(raw) != ".veris/twin.local.yaml\n" {
		t.Errorf(".gitignore = %q, %v", raw, err)
	}
}

func TestEnsureIgnoredNestedProjectUsesRootRelativePath(t *testing.T) {
	root := gitRepo(t)
	nested := project(t, filepath.Join(root, "services", "checkout"), "version: 1\n")
	wrote, err := EnsureIgnored(nested)
	if err != nil || !wrote {
		t.Fatalf("EnsureIgnored = %v, %v", wrote, err)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil || string(raw) != "services/checkout/.veris/twin.local.yaml\n" {
		t.Errorf(".gitignore = %q, %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(nested, ".gitignore")); !os.IsNotExist(err) {
		t.Error("a .gitignore appeared in the project dir instead of the repository root")
	}
	if !gitIgnores(nested, filepath.Join(nested, ".veris", "twin.local.yaml")) {
		t.Error("git does not ignore the nested local file after EnsureIgnored")
	}
}

func TestEnsureIgnoredOutsideRepoWritesNothing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	tempHome(t)
	root := project(t, t.TempDir(), "version: 1\n")
	wrote, err := EnsureIgnored(root)
	if err != nil || wrote {
		t.Errorf("EnsureIgnored outside a repo = %v, %v; want false, nil", wrote, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Error("a .gitignore was created outside any repository")
	}
}

func TestGitIgnoresWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if !gitIgnores(t.TempDir(), "x") {
		t.Error("without git on PATH, gitIgnores must answer true so nobody is warned")
	}
	if _, ok := gitRoot(t.TempDir()); ok {
		t.Error("without git on PATH, gitRoot found a repository")
	}
}
