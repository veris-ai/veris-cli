package cfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fullProject = `version: 1
project: checkout-svc
default: dev
environments:
  dev:
    id: k3j2v0d8p1q7x9r2m5n8b4c6a
    profile: work
    ttl_minutes: 240
    boot: baseline
    snapshot: nightly
    data: [data/dev-customers.json]
    callback_url: https://cb.example
    proxy: {require_service: [stripe], require_callback: [/hook], expose: 8080, image: app:test, strict: true}
    run: {command: [pytest, -q]}
  ci: {}
`

func TestLoadProjectDecodesEveryField(t *testing.T) {
	root := project(t, t.TempDir(), fullProject)
	p, err := LoadProject(filepath.Join(root, ".veris", "twin.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := EnvConfig{
		ID: "k3j2v0d8p1q7x9r2m5n8b4c6a", Profile: "work", TTLMinutes: 240,
		Boot: "baseline", Snapshot: "nightly", Data: []string{"data/dev-customers.json"},
		CallbackURL: "https://cb.example",
		Proxy: ProxyConfig{RequireService: []string{"stripe"}, RequireCallback: []string{"/hook"},
			Expose: 8080, Image: "app:test", Strict: true},
		Run: RunConfig{Command: []string{"pytest", "-q"}},
	}
	if p.Version != 1 || p.Project != "checkout-svc" || p.Default != "dev" {
		t.Errorf("header = %d %q %q", p.Version, p.Project, p.Default)
	}
	if got := p.Environments["dev"]; !reflect.DeepEqual(got, want) {
		t.Errorf("dev =\n %+v\nwant\n %+v", got, want)
	}
	if _, ok := p.Environments["ci"]; !ok {
		t.Error("empty environment ci dropped")
	}
	if p.Path != filepath.Join(root, ".veris", "twin.yaml") {
		t.Errorf("Path = %q", p.Path)
	}
	if p.Dir() != root {
		t.Errorf("Dir() = %q, want %q", p.Dir(), root)
	}
	if want := filepath.Join(root, ".veris", "twin.local.yaml"); p.LocalPath() != want {
		t.Errorf("LocalPath() = %q, want %q", p.LocalPath(), want)
	}
}

func TestLoadProjectErrors(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadProject(filepath.Join(root, ".veris", "twin.yaml")); err == nil {
		t.Error("missing file loaded without error")
	}
	project(t, root, "environments: 3\n")
	_, err := LoadProject(filepath.Join(root, ".veris", "twin.yaml"))
	if err == nil || !strings.Contains(err.Error(), "twin.yaml is unreadable") {
		t.Errorf("bad YAML: err = %v", err)
	}
}

func TestFindProjectWalksUp(t *testing.T) {
	outer := project(t, t.TempDir(), "version: 1\nproject: outer\n")
	inner := project(t, filepath.Join(outer, "svc", "checkout"), "version: 1\nproject: inner\n")
	deep := filepath.Join(inner, "tests", "unit")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, start, want string
	}{
		{"at the root", outer, "outer"},
		{"one below the outer file", filepath.Join(outer, "svc"), "outer"},
		{"at the inner file", inner, "inner"},
		{"nearest wins from deep below", deep, "inner"},
		{"relative start", ".", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := tc.start
			if start == "." {
				// A relative start resolves against the working directory, and
				// this test's working directory is the package, which has none
				// on the way up to the repository root... except that the
				// repository itself may. So chdir into a bare temp dir.
				t.Chdir(t.TempDir())
			}
			p, err := FindProject(start)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if p != nil {
					t.Errorf("found %s, want none", p.Path)
				}
				return
			}
			if p == nil || p.Project != tc.want {
				t.Errorf("found %+v, want project %q", p, tc.want)
			}
		})
	}
}

func TestFindProjectNoneIsNilNil(t *testing.T) {
	p, err := FindProject(t.TempDir())
	if p != nil || err != nil {
		t.Errorf("FindProject = %v, %v; want nil, nil", p, err)
	}
}

// The global file lives at ~/.veris/twin.yaml, the same name a project file
// has, and a walk from a folder under $HOME passes $HOME. It must not be
// taken for a project: nothing would then stop Local.Save from writing
// ~/.veris/twin.local.yaml, a file this package must never create.
func TestFindProjectSkipsTheGlobalFile(t *testing.T) {
	home := tempHome(t)
	writeFile(t, GlobalPath(), "active_profile: default\nprofiles:\n  default:\n    api_key: vsk_x\n")
	cwd := filepath.Join(home, "work", "foo")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	p, err := FindProject(cwd)
	if p != nil || err != nil {
		t.Fatalf("FindProject under $HOME = %+v, %v; want nil, nil", p, err)
	}
	r, err := Resolve(Inputs{Cwd: cwd, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	if r.Project != nil || r.Local != nil {
		t.Errorf("Resolve found a project rooted at $HOME: Project=%+v Local=%+v", r.Project, r.Local)
	}
	if r.APIKey != "vsk_x" {
		t.Errorf("the global file itself was still read for the profile: APIKey=%q", r.APIKey)
	}
	// A project anywhere between cwd and $HOME is still found.
	project(t, filepath.Join(home, "work"), "version: 1\nproject: work\n")
	p, err = FindProject(cwd)
	if err != nil || p == nil || p.Project != "work" {
		t.Errorf("FindProject with a project below $HOME = %+v, %v", p, err)
	}

	entries, err := os.ReadDir(filepath.Dir(GlobalPath()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "twin.yaml" {
			t.Errorf("~/.veris gained %s", e.Name())
		}
	}
}

func TestFindProjectEmptyStartIsCwd(t *testing.T) {
	root := project(t, t.TempDir(), "version: 1\nproject: here\n")
	t.Chdir(filepath.Join(root))
	p, err := FindProject("")
	if err != nil || p == nil || p.Project != "here" {
		t.Errorf("FindProject(\"\") = %+v, %v", p, err)
	}
	if !filepath.IsAbs(p.Path) {
		t.Errorf("Path %q is not absolute", p.Path)
	}
}

func TestProjectSaveRoundTrip(t *testing.T) {
	root := t.TempDir()
	p := &Project{
		Version: 1, Project: "checkout-svc", Default: "dev",
		Environments: map[string]EnvConfig{
			"dev": {ID: "k3j2", TTLMinutes: 240, Boot: "baseline",
				Proxy: ProxyConfig{RequireService: []string{"stripe"}},
				Run:   RunConfig{Command: []string{"pytest", "-q"}}},
		},
		Path: filepath.Join(root, ".veris", "twin.yaml"),
	}
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}
	mustMode(t, p.Path, 0o644)
	noTempFiles(t, filepath.Dir(p.Path))

	raw, err := os.ReadFile(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version: 1", "project: checkout-svc", "default: dev",
		"ttl_minutes: 240", "require_service:", "command:"} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("saved file lacks %q:\n%s", key, raw)
		}
	}
	for _, absent := range []string{"callback_url", "snapshot", "strict", "expose"} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("zero field %q written out:\n%s", absent, raw)
		}
	}
	back, err := LoadProject(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, p) {
		t.Errorf("round trip changed the file:\n got %+v\nwant %+v", back, p)
	}
}

func TestProjectSaveNeedsPath(t *testing.T) {
	if err := (&Project{Version: 1}).Save(); err == nil {
		t.Error("Save without a Path succeeded")
	}
}
