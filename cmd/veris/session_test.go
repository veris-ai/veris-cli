package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/api"
	"github.com/veris-ai/veris-proxy/internal/cfg"
	"github.com/veris-ai/veris-proxy/internal/cli"
	"github.com/veris-ai/veris-proxy/internal/ui"
)

const (
	devID = "k3j2v0d8p1q7x9r2m5n8b4c6a"
	ciID  = "c1a2b3d4e5f6g7h8i9j0k1l2m"
	sbID  = "7hqz4m2n9c1v5x8b3k6t0r2p4"
)

// bench lays out a machine for one test: a temp HOME for the profiles, a
// project directory the test is chdir'd into, and every VERIS_* variable
// cleared so the process environment of whoever runs the tests cannot leak
// into the precedence being asserted. Files are written by the callers that
// want them.
type bench struct {
	t       *testing.T
	home    string
	project string // the project directory (holds .veris/), also the cwd
}

func newBench(t *testing.T) *bench {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "proj")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for _, k := range []string{cfg.EnvProfile, cfg.EnvAPIBase, cfg.EnvAPIKey, cfg.EnvEnv, cfg.EnvEnvironmentID, cfg.EnvSandboxID} {
		t.Setenv(k, "")
	}
	t.Chdir(project)
	return &bench{t: t, home: home, project: project}
}

// global writes ~/.veris/twin.yaml.
func (b *bench) global(g cfg.Global) {
	b.t.Helper()
	g.Path = cfg.GlobalPath()
	if err := g.Save(); err != nil {
		b.t.Fatal(err)
	}
}

// projectFile writes .veris/twin.yaml with dev and ci, dev the default.
func (b *bench) projectFile(p cfg.Project) *cfg.Project {
	b.t.Helper()
	p.Version = 1
	p.Path = filepath.Join(b.project, ".veris", "twin.yaml")
	if err := p.Save(); err != nil {
		b.t.Fatal(err)
	}
	return &p
}

func (b *bench) twoEnvs() *cfg.Project {
	return b.projectFile(cfg.Project{
		Project: "proj",
		Default: "dev",
		Environments: map[string]cfg.EnvConfig{
			"dev": {ID: devID, TTLMinutes: 240},
			"ci":  {ID: ciID},
		},
	})
}

// local writes .veris/twin.local.yaml beside an existing project file.
func (b *bench) local(l cfg.Local) {
	b.t.Helper()
	l.Path = filepath.Join(b.project, ".veris", "twin.local.yaml")
	if _, err := l.Save(); err != nil {
		b.t.Fatal(err)
	}
}

// open builds a session the way a command does, off a TTY, and hands back
// the stderr it writes to.
func open(t *testing.T, g cli.Globals, envFlag, sandboxFlag string) (*session, *bytes.Buffer) {
	t.Helper()
	var stderr bytes.Buffer
	ctx := &cli.Context{Globals: &g, Stdout: &bytes.Buffer{}, Stderr: &stderr, Path: []string{"veris", "test"}}
	s, err := newSession(ctx, envFlag, sandboxFlag)
	if err != nil {
		t.Fatalf("newSession: %v", err)
	}
	s.ui.TTY = false
	return s, &stderr
}

func TestSessionPrecedence(t *testing.T) {
	b := newBench(t)
	b.global(cfg.Global{
		ActiveProfile: "default",
		Profiles: map[string]cfg.Profile{
			"default": {APIBase: "https://plane.example/", APIKey: "vsk_profilekey000", ConsoleURL: "https://studio.example"},
			"other":   {APIBase: "https://other.example", APIKey: "vsk_otherkey00000"},
		},
	})
	b.twoEnvs()
	b.local(cfg.Local{Use: "ci", Sandbox: &cfg.SandboxRef{ID: sbID}})

	t.Run("files alone: profile key and base, local use, local sandbox", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, "", "")
		want := []struct{ got, want string }{
			{s.res.ProfileName, "default"},
			{s.res.APIBase, "https://plane.example"},
			{string(s.res.APIBaseSource), "profile"},
			{s.res.APIKey, "vsk_profilekey000"},
			{string(s.res.APIKeySource), "profile"},
			{s.res.EnvName, "ci"},
			{string(s.res.EnvSource), "local"},
			{s.res.SandboxID, sbID},
			{string(s.res.SandboxSource), "local"},
			{s.consoleURL(), "https://studio.example"},
			{s.ver, version},
		}
		for _, w := range want {
			if w.got != w.want {
				t.Errorf("got %q, want %q", w.got, w.want)
			}
		}
		if s.res.Env == nil || s.res.Env.ID != ciID {
			t.Errorf("Env = %+v, want ci's config", s.res.Env)
		}
		if s.res.Project == nil || s.res.Local == nil {
			t.Fatalf("project %v local %v, want both loaded", s.res.Project, s.res.Local)
		}
	})

	t.Run("the command's own flags beat the files", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, "dev", "sb_flag")
		if s.res.EnvName != "dev" || s.res.EnvSource != cfg.SourceFlag || s.res.Env.ID != devID {
			t.Errorf("env %q from %q (%+v)", s.res.EnvName, s.res.EnvSource, s.res.Env)
		}
		if s.res.SandboxID != "sb_flag" || s.res.SandboxSource != cfg.SourceFlag {
			t.Errorf("sandbox %q from %q", s.res.SandboxID, s.res.SandboxSource)
		}
	})

	t.Run("environment variables beat the files and lose to flags", func(t *testing.T) {
		t.Setenv(cfg.EnvEnv, "dev")
		t.Setenv(cfg.EnvSandboxID, "sb_env")
		t.Setenv(cfg.EnvAPIKey, "vsk_envkey0000000")
		t.Setenv(cfg.EnvAPIBase, "https://env.example")
		s, _ := open(t, cli.Globals{}, "", "")
		if s.res.EnvName != "dev" || s.res.EnvSource != cfg.SourceEnv {
			t.Errorf("env %q from %q", s.res.EnvName, s.res.EnvSource)
		}
		if s.res.SandboxID != "sb_env" || s.res.SandboxSource != cfg.SourceEnv {
			t.Errorf("sandbox %q from %q", s.res.SandboxID, s.res.SandboxSource)
		}
		if s.res.APIKey != "vsk_envkey0000000" || s.res.APIKeySource != cfg.SourceEnv || s.res.Profile.APIKey != "" {
			t.Errorf("key %q from %q; profile key %q must be blank", ui.MaskKey(s.res.APIKey), s.res.APIKeySource, s.res.Profile.APIKey)
		}
		if s.res.APIBase != "https://env.example" || s.res.APIBaseSource != cfg.SourceEnv {
			t.Errorf("base %q from %q", s.res.APIBase, s.res.APIBaseSource)
		}
		s, _ = open(t, cli.Globals{APIBase: "https://flag.example"}, "ci", "sb_flag")
		if s.res.APIBase != "https://flag.example" || s.res.EnvName != "ci" || s.res.SandboxID != "sb_flag" {
			t.Errorf("flags lost: base %q env %q sandbox %q", s.res.APIBase, s.res.EnvName, s.res.SandboxID)
		}
	})

	t.Run("--profile picks another login", func(t *testing.T) {
		s, _ := open(t, cli.Globals{Profile: "other"}, "", "")
		if s.res.ProfileName != "other" || s.res.APIBase != "https://other.example" || s.res.APIKey != "vsk_otherkey00000" {
			t.Errorf("profile %q base %q key %q", s.res.ProfileName, s.res.APIBase, ui.MaskKey(s.res.APIKey))
		}
		if s.consoleURL() != "" {
			t.Errorf("consoleURL = %q, want empty for a profile without one", s.consoleURL())
		}
	})

	t.Run("the globals reach the UI", func(t *testing.T) {
		s, _ := open(t, cli.Globals{Quiet: true, Yes: true}, "", "")
		if !s.ui.Quiet || !s.ui.AssumeYes {
			t.Errorf("Quiet %v AssumeYes %v, want both", s.ui.Quiet, s.ui.AssumeYes)
		}
	})

	t.Run("a profile that does not exist is newSession's error", func(t *testing.T) {
		var stderr bytes.Buffer
		ctx := &cli.Context{Globals: &cli.Globals{Profile: "nope"}, Stdout: &bytes.Buffer{}, Stderr: &stderr}
		if _, err := newSession(ctx, "", ""); err == nil || !strings.Contains(err.Error(), `profile "nope" not found`) {
			t.Errorf("err = %v, want profile not found", err)
		}
	})
}

func TestSessionClientNeedsAKey(t *testing.T) {
	b := newBench(t)

	t.Run("a machine that never logged in", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "", "")
		c, err := s.client()
		if c != nil {
			t.Errorf("client = %+v, want nil", c)
		}
		if !errors.Is(err, printed(1)) {
			t.Errorf("err = %#v, want printed(1)", err)
		}
		want := "✗ Not logged in for profile 'default' (no API key)\n→ Next: veris login --profile default\n"
		if stderr.String() != want {
			t.Errorf("stderr %q, want %q", stderr.String(), want)
		}
		if code := exitStatusTo(stderr, err); code != 1 || strings.Count(stderr.String(), "Not logged in") != 1 {
			t.Errorf("exit %d and stderr %q; want 1 with the message printed once", code, stderr.String())
		}
	})

	b.global(cfg.Global{
		ActiveProfile: "dev",
		Profiles: map[string]cfg.Profile{
			"dev":  {APIBase: "https://dev.example"},
			"prod": {APIBase: "https://prod.example", APIKey: "vsk_prodkey000000"},
		},
	})

	t.Run("a profile without a key names itself", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "", "")
		if _, err := s.client(); !errors.Is(err, printed(1)) {
			t.Fatalf("err = %v", err)
		}
		want := "✗ Not logged in for profile 'dev' (no API key)\n→ Next: veris login --profile dev\n"
		if stderr.String() != want {
			t.Errorf("stderr %q, want %q", stderr.String(), want)
		}
		// plane never refuses: the device routes and healthz need no key.
		if p := s.plane(); p.Base != "https://dev.example" || p.Key != "" {
			t.Errorf("plane = %+v", p)
		}
	})

	t.Run("a key makes a client for the resolved plane", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{Profile: "prod"}, "", "")
		c, err := s.client()
		if err != nil {
			t.Fatal(err)
		}
		if c.Base != "https://prod.example" || c.Key != "vsk_prodkey000000" || c.UserAgent != "veris/"+version {
			t.Errorf("client = base %q key %q ua %q", c.Base, ui.MaskKey(c.Key), c.UserAgent)
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr %q, want nothing", stderr.String())
		}
		if tw := s.twin("https://gw.example/s/x/stripe/"); tw.ControlURL != "https://gw.example/s/x/stripe" {
			t.Errorf("twin ControlURL = %q", tw.ControlURL)
		}
	})
}

func TestSessionRequireProject(t *testing.T) {
	b := newBench(t)
	s, stderr := open(t, cli.Globals{}, "", "")
	if p, err := s.requireProject(); p != nil || !errors.Is(err, printed(1)) {
		t.Fatalf("requireProject = %v, %v", p, err)
	}
	want := "✗ No .veris/twin.yaml found (searched up from " + s.cwd + ")\n→ Next: veris env create\n"
	if stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}

	b.twoEnvs()
	// From a subdirectory the same file is found.
	sub := filepath.Join(b.project, "src", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	s, stderr = open(t, cli.Globals{}, "", "")
	p, err := s.requireProject()
	if err != nil || p == nil || p.Project != "proj" || stderr.Len() != 0 {
		t.Errorf("requireProject = %+v, %v, stderr %q", p, err, stderr.String())
	}
}

func TestSessionRequireEnv(t *testing.T) {
	b := newBench(t)

	t.Run("nothing chosen, no project", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "", "")
		_, _, _, err := s.requireEnv()
		if !errors.Is(err, printed(1)) {
			t.Fatalf("err = %v", err)
		}
		if want := "✗ No environment selected\n→ Next: veris env use NAME, or pass --env\n"; stderr.String() != want {
			t.Errorf("stderr %q, want %q", stderr.String(), want)
		}
	})

	t.Run("a bare id from VERIS_ENVIRONMENT_ID has no config", func(t *testing.T) {
		t.Setenv(cfg.EnvEnvironmentID, devID)
		s, _ := open(t, cli.Globals{}, "", "")
		name, id, conf, err := s.requireEnv()
		if err != nil || name != devID || id != devID || conf != nil {
			t.Errorf("requireEnv = %q, %q, %v, %v", name, id, conf, err)
		}
	})

	p := b.twoEnvs()

	t.Run("the project default", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, "", "")
		name, id, conf, err := s.requireEnv()
		if err != nil || name != "dev" || id != devID || conf == nil || conf.TTLMinutes != 240 {
			t.Errorf("requireEnv = %q, %q, %+v, %v", name, id, conf, err)
		}
	})

	t.Run("--env by id lands on the same entry as by name", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, ciID, "")
		name, id, conf, err := s.requireEnv()
		if err != nil || name != ciID || id != ciID || conf == nil {
			t.Errorf("requireEnv = %q, %q, %+v, %v", name, id, conf, err)
		}
	})

	t.Run("a name the file does not know is refused with the names it has", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "stagin", "")
		if _, _, _, err := s.requireEnv(); !errors.Is(err, printed(1)) {
			t.Fatalf("err = %v", err)
		}
		want := "✗ No environment 'stagin' in " + p.Path + " (have: ci, dev)\n→ Next: veris env list\n"
		if stderr.String() != want {
			t.Errorf("stderr %q, want %q", stderr.String(), want)
		}
	})

	t.Run("an id the file does not know is passed to the server", func(t *testing.T) {
		other := "z9y8x7w6v5u4t3s2r1q0p9o8n"
		s, _ := open(t, cli.Globals{}, other, "")
		name, id, conf, err := s.requireEnv()
		if err != nil || name != other || id != other || conf != nil {
			t.Errorf("requireEnv = %q, %q, %v, %v", name, id, conf, err)
		}
	})

	b.projectFile(cfg.Project{Project: "proj", Environments: map[string]cfg.EnvConfig{
		"dev":  {ID: devID},
		"ci":   {ID: ciID},
		"bare": {},
	}})

	t.Run("nothing chosen off a TTY, with a project", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "", "")
		if _, _, _, err := s.requireEnv(); !errors.Is(err, printed(1)) {
			t.Fatalf("err = %v", err)
		}
		if !strings.HasPrefix(stderr.String(), "✗ No environment selected\n") {
			t.Errorf("stderr %q", stderr.String())
		}
	})

	t.Run("nothing chosen on a TTY is a picker over the project's names", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "", "")
		s.ui.TTY = true
		s.ui.In = strings.NewReader("dev\n")
		name, id, conf, err := s.requireEnv()
		if err != nil || name != "dev" || id != devID || conf == nil {
			t.Fatalf("requireEnv = %q, %q, %+v, %v", name, id, conf, err)
		}
		out := stderr.String()
		for _, frag := range []string{"Select an environment:", "bare", "ci", "dev", shortID(devID)} {
			if !strings.Contains(out, frag) {
				t.Errorf("picker output %q lacks %q", out, frag)
			}
		}
		if s.res.EnvName != "dev" || s.res.Env == nil || s.res.EnvSource != cfg.SourceFlag {
			t.Errorf("the choice was not recorded: %q %+v from %q", s.res.EnvName, s.res.Env, s.res.EnvSource)
		}
	})

	t.Run("an entry without an id", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "bare", "")
		if _, _, _, err := s.requireEnv(); !errors.Is(err, printed(1)) {
			t.Fatalf("err = %v", err)
		}
		if !strings.HasPrefix(stderr.String(), "✗ Environment 'bare' in ") || !strings.Contains(stderr.String(), " has no id\n→ Next: veris env create bare") {
			t.Errorf("stderr %q", stderr.String())
		}
	})
}

func TestSessionRequireSandbox(t *testing.T) {
	newBench(t)
	s, stderr := open(t, cli.Globals{}, "", "")
	if id, err := s.requireSandbox(); id != "" || !errors.Is(err, printed(1)) {
		t.Fatalf("requireSandbox = %q, %v", id, err)
	}
	if want := "✗ No sandbox for this folder\n→ Next: veris up\n"; stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
	t.Setenv(cfg.EnvSandboxID, sbID)
	s, _ = open(t, cli.Globals{}, "", "")
	if id, err := s.requireSandbox(); id != sbID || err != nil {
		t.Errorf("requireSandbox = %q, %v", id, err)
	}
}

func TestSessionRememberAndForgetSandbox(t *testing.T) {
	b := newBench(t)
	created := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	sb := &api.Sandbox{ID: sbID, EnvironmentID: devID, CreatedAt: api.Time{Time: created}, ExpiresAt: api.Time{Time: created.Add(2 * time.Hour)}}

	t.Run("without a project file the pointer has no home", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "", "")
		if err := s.rememberSandbox(sb); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(stderr.String(), "! no .veris/twin.yaml here, so sandbox "+sbID+" is not remembered") {
			t.Errorf("stderr %q", stderr.String())
		}
		if err := s.forgetSandbox(); err != nil {
			t.Errorf("forgetSandbox = %v", err)
		}
	})

	b.twoEnvs()
	localPath := filepath.Join(b.project, ".veris", "twin.local.yaml")

	t.Run("remember writes the pointer and makes it current", func(t *testing.T) {
		s, stderr := open(t, cli.Globals{}, "", "")
		if err := s.rememberSandbox(sb); err != nil {
			t.Fatal(err)
		}
		if s.res.SandboxID != sbID || s.res.SandboxSource != cfg.SourceLocal {
			t.Errorf("session sandbox %q from %q", s.res.SandboxID, s.res.SandboxSource)
		}
		l, err := cfg.LoadLocal(localPath)
		if err != nil {
			t.Fatal(err)
		}
		if l.Sandbox == nil || l.Sandbox.ID != sbID || l.Sandbox.EnvironmentID != devID ||
			l.Sandbox.CreatedAt != "2026-03-01T09:00:00Z" || l.Sandbox.ExpiresAt != "2026-03-01T11:00:00Z" {
			t.Errorf("local sandbox = %+v", l.Sandbox)
		}
		if info, err := os.Stat(localPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("local file mode %v, %v; want 0600", info, err)
		}
		// The temp directory is no git repository, so there is nothing to
		// warn about.
		if stderr.Len() != 0 {
			t.Errorf("stderr %q, want nothing", stderr.String())
		}
	})

	t.Run("a new session finds it", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, "", "")
		if id, err := s.requireSandbox(); id != sbID || err != nil {
			t.Errorf("requireSandbox = %q, %v", id, err)
		}
	})

	t.Run("a flag stays current over a remembered pointer", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, "", "sb_flag")
		if err := s.rememberSandbox(&api.Sandbox{ID: "sb_new"}); err != nil {
			t.Fatal(err)
		}
		if s.res.SandboxID != "sb_flag" || s.res.SandboxSource != cfg.SourceFlag {
			t.Errorf("session sandbox %q from %q", s.res.SandboxID, s.res.SandboxSource)
		}
		l, _ := cfg.LoadLocal(localPath)
		if l.Sandbox == nil || l.Sandbox.ID != "sb_new" || l.Sandbox.ExpiresAt != "" {
			t.Errorf("local sandbox = %+v", l.Sandbox)
		}
	})

	t.Run("forget clears the pointer and the session", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, "", "")
		if err := s.forgetSandbox(); err != nil {
			t.Fatal(err)
		}
		if s.res.SandboxID != "" || s.res.SandboxSource != "" {
			t.Errorf("session sandbox %q from %q after forget", s.res.SandboxID, s.res.SandboxSource)
		}
		l, _ := cfg.LoadLocal(localPath)
		if l.Sandbox != nil {
			t.Errorf("local sandbox = %+v after forget", l.Sandbox)
		}
		if err := s.forgetSandbox(); err != nil {
			t.Errorf("a second forget = %v", err)
		}
	})
}

func TestSessionFailRendersTheGrammar(t *testing.T) {
	b := newBench(t)
	b.global(cfg.Global{ActiveProfile: "dev", Profiles: map[string]cfg.Profile{"dev": {APIKey: "vsk_k"}}})
	notFound := &api.Error{Status: http.StatusNotFound, Detail: "environment " + shortID(devID) + " not found"}
	invalid := &api.Error{Status: http.StatusUnprocessableEntity,
		Detail:  "customers[0].email: must be a string; unknown table 'customer'",
		Reasons: []string{"customers[0].email: must be a string", "unknown table 'customer'"}}
	unauth := &api.Error{Status: http.StatusUnauthorized, Detail: "invalid or missing API key"}

	cases := []struct {
		name   string
		err    error
		want   string
		viaUI  bool // the free fail, with no profile to name
		quiet  bool
		passed bool // the error comes back as given
	}{
		{name: "a 404", err: notFound,
			want: "✗ Failed to get environment: [404] environment k3j2v0d8… not found\n"},
		{name: "a 422's reasons, one per line", err: invalid,
			want: "✗ Failed to get environment: [422]\n  customers[0].email: must be a string\n  unknown table 'customer'\n"},
		{name: "reasons survive --quiet", err: invalid, quiet: true,
			want: "✗ Failed to get environment: [422]\n  customers[0].email: must be a string\n  unknown table 'customer'\n"},
		{name: "a 401 with a session", err: unauth,
			want: "✗ Not logged in for profile 'dev': [401] invalid or missing API key\n→ Next: veris login --profile dev\n"},
		{name: "a 401 without a session", err: unauth, viaUI: true,
			want: "✗ Not logged in: [401] invalid or missing API key\n→ Next: veris login\n"},
		{name: "a plain error", err: errors.New("dial tcp: connection refused"),
			want: "✗ Failed to get environment: dial tcp: connection refused\n"},
		{name: "already printed", err: printed(4), want: "", passed: true},
		{name: "the user left the prompt", err: ui.ErrInterrupted, want: "", passed: true},
		{name: "no TTY names its flag", err: &ui.NoTTYError{FlagHint: "--env"}, want: "", passed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, stderr := open(t, cli.Globals{Quiet: tc.quiet}, "", "")
			var err error
			if tc.viaUI {
				err = fail(s.ui, "get", "environment", tc.err)
			} else {
				err = s.fail("get", "environment", tc.err)
			}
			if stderr.String() != tc.want {
				t.Errorf("stderr %q, want %q", stderr.String(), tc.want)
			}
			if tc.passed {
				if err != tc.err {
					t.Errorf("err = %v, want %v back unchanged", err, tc.err)
				}
				return
			}
			if !errors.Is(err, printed(1)) {
				t.Errorf("err = %#v, want printed(1)", err)
			}
		})
	}

	t.Run("main exits without a second line", func(t *testing.T) {
		for _, tc := range []struct {
			err  error
			code int
		}{{printed(1), 1}, {printed(4), 4}, {ui.ErrInterrupted, 1}} {
			var stderr bytes.Buffer
			if code := exitStatusTo(&stderr, tc.err); code != tc.code || stderr.Len() != 0 {
				t.Errorf("%v: exit %d with %q; want %d and nothing", tc.err, code, stderr.String(), tc.code)
			}
		}
		var stderr bytes.Buffer
		if code := exitStatusTo(&stderr, errDeclined); code != 1 || stderr.String() != "veris: declined\n" {
			t.Errorf("declined: exit %d with %q", code, stderr.String())
		}
	})
}

func TestSessionOutputHelpers(t *testing.T) {
	t.Run("printJSON is indented and leaves URLs alone", func(t *testing.T) {
		var out bytes.Buffer
		err := printJSON(&out, map[string]any{"url": "https://x.example/?a=1&b=2", "n": 1})
		want := "{\n  \"n\": 1,\n  \"url\": \"https://x.example/?a=1&b=2\"\n}\n"
		if err != nil || out.String() != want {
			t.Errorf("printJSON = %q, %v; want %q", out.String(), err, want)
		}
	})

	t.Run("shortID", func(t *testing.T) {
		for in, want := range map[string]string{devID: "k3j2v0d8…", "short": "short", "12345678": "12345678", "": ""} {
			if got := shortID(in); got != want {
				t.Errorf("shortID(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("looksLikeID", func(t *testing.T) {
		for in, want := range map[string]bool{devID: true, "stagin": false, strings.ToUpper(devID): false, devID + "x": false} {
			if got := looksLikeID(in); got != want {
				t.Errorf("looksLikeID(%q) = %v", in, got)
			}
		}
	})

	t.Run("confirm", func(t *testing.T) {
		u := ui.New(&bytes.Buffer{}, strings.NewReader(""))
		var noTTY *ui.NoTTYError
		if err := confirm(u, "Delete it?"); !errors.As(err, &noTTY) || noTTY.FlagHint != "--yes" {
			t.Errorf("off a TTY: %v, want NoTTYError naming --yes", err)
		}

		var out bytes.Buffer
		u = ui.New(&out, strings.NewReader(""))
		u.AssumeYes = true
		if err := confirm(u, "Delete it?"); err != nil || out.String() != "Delete it? y\n" {
			t.Errorf("--yes: %v, %q", err, out.String())
		}

		for in, want := range map[string]error{"n\n": errDeclined, "\n": errDeclined, "y\n": nil} {
			out.Reset()
			u = ui.New(&out, strings.NewReader(in))
			u.TTY = true
			if err := confirm(u, "Delete it?"); !errors.Is(err, want) {
				t.Errorf("answer %q: %v, want %v", in, err, want)
			}
			if !strings.HasPrefix(out.String(), "Delete it? [y/N] ") {
				t.Errorf("answer %q: prompt %q", in, out.String())
			}
		}
	})

	t.Run("studioLink", func(t *testing.T) {
		var out bytes.Buffer
		u := ui.New(&out, strings.NewReader(""))
		studioLink(u, "", "environments", devID)
		if out.Len() != 0 {
			t.Errorf("no console printed %q", out.String())
		}
		studioLink(u, "https://studio.example/", "environments", devID)
		if want := "→ https://studio.example/environments/" + devID + "\n"; out.String() != want {
			t.Errorf("link %q, want %q", out.String(), want)
		}
	})
}

func TestSessionFailNamesAShellKey(t *testing.T) {
	b := newBench(t)
	b.global(cfg.Global{ActiveProfile: "dev", Profiles: map[string]cfg.Profile{
		"dev": {APIBase: "https://dev.example", APIKey: "vsk_profilekey000"}}})
	t.Setenv(cfg.EnvAPIKey, "vsk_shellkey00000")
	s, stderr := open(t, cli.Globals{}, "", "")
	err := s.fail("get", "environment", &api.Error{Status: http.StatusUnauthorized, Detail: "invalid or missing API key"})
	want := "✗ VERIS_API_KEY from your shell was rejected by https://dev.example: [401] invalid or missing API key\n" +
		"→ Next: unset VERIS_API_KEY to use profile 'dev', or export a key for https://dev.example\n"
	if stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
	if !errors.Is(err, printed(1)) {
		t.Errorf("err = %#v, want printed(1)", err)
	}
}
