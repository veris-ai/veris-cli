package cfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalPathFollowsHome(t *testing.T) {
	home := tempHome(t)
	if got, want := GlobalPath(), filepath.Join(home, ".veris", "twin.yaml"); got != want {
		t.Errorf("GlobalPath() = %q, want %q", got, want)
	}
}

func TestLoadGlobalMissingIsEmpty(t *testing.T) {
	tempHome(t)
	g, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	if g.Path != GlobalPath() {
		t.Errorf("Path = %q, want %q", g.Path, GlobalPath())
	}
	if g.ActiveProfile != "" || len(g.Profiles) != 0 {
		t.Errorf("missing file loaded as %+v, want empty", g)
	}
	if g.Profiles == nil {
		t.Error("Profiles is nil; a caller adding the first profile would panic")
	}
}

func TestLoadGlobalUnreadableNamesPath(t *testing.T) {
	tempHome(t)
	writeFile(t, GlobalPath(), "profiles: [not, a, map]\n")
	_, err := LoadGlobal()
	if err == nil {
		t.Fatal("bad YAML loaded without error")
	}
	if !strings.Contains(err.Error(), GlobalPath()) {
		t.Errorf("error %q does not name the file", err)
	}
}

func TestGlobalSaveRoundTrip(t *testing.T) {
	home := tempHome(t)
	// The Python CLI's file sits in the same directory and must survive.
	writeFile(t, filepath.Join(home, ".veris", "config.yaml"), "theirs: true\n")

	g := &Global{
		ActiveProfile: "work",
		Profiles: map[string]Profile{
			"work": {
				APIBase:            "https://svc.api.veris.ai",
				APIKey:             "vsk_mi4pa0uo_secret",
				ConsoleURL:         "https://studio.veris.ai",
				KeyID:              "key_1",
				KeyName:            "veris on host · device",
				OrganizationID:     "org_1",
				DefaultEnvironment: "dev",
			},
			"default": {APIKey: "vsk_other"},
		},
	}
	if err := g.Save(); err != nil {
		t.Fatal(err)
	}
	if g.Path != GlobalPath() {
		t.Errorf("Save left Path = %q", g.Path)
	}
	mustMode(t, GlobalPath(), 0o600)
	noTempFiles(t, filepath.Dir(GlobalPath()))

	raw, err := os.ReadFile(GlobalPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"active_profile: work", "api_base:", "api_key:",
		"console_url:", "key_id:", "key_name:", "organization_id:", "default_environment: dev"} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("saved file lacks %q:\n%s", key, raw)
		}
	}
	if strings.Contains(string(raw), "console_url: \"\"") {
		t.Errorf("empty fields are written out:\n%s", raw)
	}

	back, err := LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	if back.ActiveProfile != g.ActiveProfile || len(back.Profiles) != 2 ||
		back.Profiles["work"] != g.Profiles["work"] || back.Profiles["default"] != g.Profiles["default"] {
		t.Errorf("round trip changed the file:\n got %+v\nwant %+v", back, g)
	}

	theirs, err := os.ReadFile(filepath.Join(home, ".veris", "config.yaml"))
	if err != nil || string(theirs) != "theirs: true\n" {
		t.Errorf("config.yaml was touched: %q, %v", theirs, err)
	}
}

func TestGlobalSaveCreatesDirPrivate(t *testing.T) {
	home := tempHome(t)
	g := &Global{Profiles: map[string]Profile{"default": {APIKey: "k"}}}
	if err := g.Save(); err != nil {
		t.Fatal(err)
	}
	mustMode(t, filepath.Join(home, ".veris"), 0o700)
}

func TestGlobalActive(t *testing.T) {
	cases := []struct {
		name     string
		g        Global
		wantName string
		wantOK   bool
	}{
		{"named and present", Global{ActiveProfile: "work",
			Profiles: map[string]Profile{"work": {APIKey: "k"}}}, "work", true},
		{"named and missing", Global{ActiveProfile: "gone",
			Profiles: map[string]Profile{"default": {APIKey: "k"}}}, "gone", false},
		{"unnamed falls back to default", Global{
			Profiles: map[string]Profile{"default": {APIKey: "k"}}}, "default", true},
		{"empty file", Global{}, "default", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, p, ok := tc.g.Active()
			if name != tc.wantName || ok != tc.wantOK {
				t.Errorf("Active() = %q, ok=%v; want %q, ok=%v", name, ok, tc.wantName, tc.wantOK)
			}
			if ok && p.APIKey != "k" {
				t.Errorf("Active() returned profile %+v", p)
			}
		})
	}
}
