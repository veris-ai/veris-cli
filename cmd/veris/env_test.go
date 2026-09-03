package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/api"
	"github.com/veris-ai/veris-proxy/internal/cfg"
	"github.com/veris-ai/veris-proxy/internal/cli"
	"github.com/veris-ai/veris-proxy/internal/routes"
)

const (
	envTestKey = "vsk_envtestkey000000000"
	probeID    = "n8crn4cvq1w2e3r4t5y6u7i8o"
)

// envPlane is a fake control plane for the env verbs: the catalog, the
// environments with their sandboxes, and /v1/me. Every mutation is recorded
// so a test can assert what was sent, and a failure can be scripted per
// sandbox delete and per sandbox listing.
type envPlane struct {
	mu sync.Mutex

	catalog   []api.CatalogService
	envs      []api.Environment
	sandboxes map[string][]api.Sandbox

	created        []api.CreateEnvironmentRequest
	deletedEnvs    []string
	deletedSandbox []string
	failDeleteSB   map[string]bool // sandbox id → 500
	failListSB     map[string]bool // environment id → 403
	meStatus       int             // non-zero: /v1/me answers this
	seq            int
	requests       []string
}

func newEnvPlane(t *testing.T) *envPlane {
	t.Helper()
	return &envPlane{
		catalog: []api.CatalogService{
			{Name: "stripe", Description: "Stripe payments API", Routes: []routes.Entry{{Host: "api.stripe.com"}}},
			{Name: "postgres", Description: "Postgres data plane (DSN)"},
			{Name: "github", Description: "GitHub REST + webhooks", Routes: []routes.Entry{{Host: "api.github.com"}}},
		},
		sandboxes:    map[string][]api.Sandbox{},
		failDeleteSB: map[string]bool{},
		failListSB:   map[string]bool{},
	}
}

func (f *envPlane) addEnv(id, name string, services []string, baselineRev string) {
	env := api.Environment{ID: id, Name: name, Services: services, CreatedAt: api.Time{Time: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}}
	if baselineRev != "" {
		env.Baseline = &api.EnvironmentBaseline{Image: "repo@sha256:abc", RevisionID: baselineRev, SourceSandbox: sbID}
	}
	f.envs = append(f.envs, env)
}

func (f *envPlane) addSandbox(envID, id, status string) {
	f.sandboxes[envID] = append(f.sandboxes[envID], api.Sandbox{ID: id, EnvironmentID: envID, Status: status})
}

func (f *envPlane) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *envPlane) detail(w http.ResponseWriter, status int, msg string) {
	f.reply(w, status, map[string]string{"detail": msg})
}

func (f *envPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("X-API-Key") != envTestKey {
		f.detail(w, 401, "invalid or missing API key")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/v1/services" && r.Method == http.MethodGet:
		f.reply(w, 200, f.catalog)
	case r.URL.Path == "/v1/me":
		if f.meStatus != 0 {
			f.detail(w, f.meStatus, "me is broken")
			return
		}
		f.reply(w, 200, api.Me{Kind: "api_key", OrganizationID: "org_1", Organizations: []api.Organization{{ID: "org_1", Name: "Acme", Kind: "team"}}})
	case r.URL.Path == "/v1/environments" && r.Method == http.MethodGet:
		f.reply(w, 200, f.envs)
	case r.URL.Path == "/v1/environments" && r.Method == http.MethodPost:
		var req api.CreateEnvironmentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			f.detail(w, 400, err.Error())
			return
		}
		f.created = append(f.created, req)
		f.seq++
		env := api.Environment{ID: fmt.Sprintf("e%024d", f.seq), Name: req.Name, Services: req.Services, CreatedAt: api.Time{Time: time.Now()}}
		f.envs = append(f.envs, env)
		f.reply(w, 201, env)
	case len(parts) == 3 && parts[1] == "environments":
		i := f.find(parts[2])
		if i < 0 {
			f.detail(w, 404, "environment "+parts[2]+" not found")
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.reply(w, 200, f.envs[i])
		case http.MethodDelete:
			f.deletedEnvs = append(f.deletedEnvs, parts[2])
			f.envs = append(f.envs[:i], f.envs[i+1:]...)
			w.WriteHeader(204)
		default:
			f.detail(w, 405, "method not allowed")
		}
	case len(parts) == 4 && parts[1] == "environments" && parts[3] == "sandboxes" && r.Method == http.MethodGet:
		if f.failListSB[parts[2]] {
			f.detail(w, 403, "not yours")
			return
		}
		if f.find(parts[2]) < 0 {
			f.detail(w, 404, "environment "+parts[2]+" not found")
			return
		}
		sbs := f.sandboxes[parts[2]]
		if sbs == nil {
			sbs = []api.Sandbox{}
		}
		f.reply(w, 200, sbs)
	case len(parts) == 5 && parts[3] == "sandboxes" && r.Method == http.MethodDelete:
		if f.failDeleteSB[parts[4]] {
			f.detail(w, 500, "node unreachable")
			return
		}
		sbs := f.sandboxes[parts[2]]
		for i, sb := range sbs {
			if sb.ID == parts[4] {
				f.deletedSandbox = append(f.deletedSandbox, sb.ID)
				f.sandboxes[parts[2]] = append(sbs[:i], sbs[i+1:]...)
				w.WriteHeader(204)
				return
			}
		}
		f.detail(w, 404, "sandbox "+parts[4]+" not found")
	default:
		f.detail(w, 404, "no route "+r.URL.Path)
	}
}

func (f *envPlane) find(id string) int {
	for i, env := range f.envs {
		if env.ID == id {
			return i
		}
	}
	return -1
}

// envBench is a bench logged in to a fake plane.
type envBench struct {
	*bench
	plane *envPlane
	srv   *httptest.Server
}

func newEnvBench(t *testing.T) *envBench {
	t.Helper()
	b := newBench(t)
	plane := newEnvPlane(t)
	srv := httptest.NewServer(plane)
	t.Cleanup(srv.Close)
	b.global(cfg.Global{
		ActiveProfile: "default",
		Profiles: map[string]cfg.Profile{
			"default": {APIBase: srv.URL, APIKey: envTestKey, ConsoleURL: "https://studio.example"},
		},
	})
	return &envBench{bench: b, plane: plane, srv: srv}
}

// run drives one command line end to end through the tree, off a TTY, and
// returns stdout, stderr and the exit status main would have produced.
func (b *envBench) run(args ...string) (string, string, int) {
	b.t.Helper()
	return b.runWith("", false, args...)
}

// runTTY runs with a forced TTY reading input as the prompts' answers.
func (b *envBench) runTTY(input string, args ...string) (string, string, int) {
	b.t.Helper()
	return b.runWith(input, true, args...)
}

func (b *envBench) runWith(input string, tty bool, args ...string) (string, string, int) {
	b.t.Helper()
	prevIn, prevHook := stdin, newSessionHook
	stdin = strings.NewReader(input)
	newSessionHook = func(s *session) { s.ui.TTY = tty }
	defer func() { stdin, newSessionHook = prevIn, prevHook }()
	var stdout, stderr bytes.Buffer
	err := cli.Execute(root(), &cli.Globals{}, args, &stdout, &stderr)
	code := exitStatusTo(&stderr, err)
	return stdout.String(), stderr.String(), code
}

// loadProject reads the bench's project file, failing if it is not there.
func (b *envBench) loadProject() *cfg.Project {
	b.t.Helper()
	p, err := cfg.LoadProject(filepath.Join(b.project, ".veris", "twin.yaml"))
	if err != nil {
		b.t.Fatalf("load project: %v", err)
	}
	return p
}

func (b *envBench) loadLocal() *cfg.Local {
	b.t.Helper()
	l, err := cfg.LoadLocal(filepath.Join(b.project, ".veris", "twin.local.yaml"))
	if err != nil {
		b.t.Fatalf("load local: %v", err)
	}
	return l
}

// gitInit makes the project a repository, so EnsureIgnored has a .gitignore
// to write; skipped where git is not installed.
func (b *envBench) gitInit() {
	b.t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		b.t.Skip("git not installed")
	}
	if out, err := exec.Command("git", "-C", b.project, "init", "-q").CombinedOutput(); err != nil {
		b.t.Fatalf("git init: %v: %s", err, out)
	}
}

// squash collapses runs of whitespace so a table row can be matched as
// words, however the columns were padded.
func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func wantLines(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l) {
			t.Errorf("output lacks %q:\n%s", l, out)
		}
	}
}

func wantNot(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(out, l) {
			t.Errorf("output has %q, want not:\n%s", l, out)
		}
	}
}

func TestEnvCreateFlagDriven(t *testing.T) {
	b := newEnvBench(t)

	stdout, stderr, code := b.run("env", "create", "ci", "--services", "stripe", "--ttl", "20", "--boot", "bundle",
		"--data", "data/ci.json", "--command", "pytest -q tests/integration", "--json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if len(b.plane.created) != 1 || b.plane.created[0].Name != "ci" || !reflect.DeepEqual(b.plane.created[0].Services, []string{"stripe"}) {
		t.Errorf("POST /v1/environments got %+v", b.plane.created)
	}
	id := b.plane.envs[0].ID
	wantLines(t, stderr,
		"✓ Environment created: "+id+" (ci: stripe)",
		"✓ Added 'ci' to .veris/twin.yaml as the default",
		"→ https://studio.example/environments/"+id,
		"→ Next: veris up")
	var got envCreated
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	want := envCreated{Name: "ci", ID: id, Services: []string{"stripe"}, TTLMinutes: 20, Boot: "bundle",
		Data: []string{"data/ci.json"}, Command: []string{"pytest", "-q", "tests/integration"}, Default: true,
		ProjectFile: filepath.Join(b.project, ".veris", "twin.yaml")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("json = %+v, want %+v", got, want)
	}

	p := b.loadProject()
	if p.Version != 1 || p.Project != "proj" || p.Default != "ci" {
		t.Errorf("project file: version %d project %q default %q", p.Version, p.Project, p.Default)
	}
	conf := p.Environments["ci"]
	wantConf := cfg.EnvConfig{ID: id, TTLMinutes: 20, Boot: "bundle", Data: []string{"data/ci.json"}, Run: cfg.RunConfig{Command: []string{"pytest", "-q", "tests/integration"}}}
	if !reflect.DeepEqual(conf, wantConf) {
		t.Errorf("ci config = %+v, want %+v", conf, wantConf)
	}

	t.Run("the second environment is not the default and empty answers are kept", func(t *testing.T) {
		_, stderr, code := b.run("env", "create", "dev", "--services", "stripe,postgres", "--ttl", "240", "--boot", "baseline",
			"--data", "", "--command", "")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "✓ Added 'dev' to .veris/twin.yaml\n")
		wantNot(t, stderr, "'dev' to .veris/twin.yaml as the default")
		p := b.loadProject()
		if p.Default != "ci" {
			t.Errorf("default = %q, want ci kept", p.Default)
		}
		dev := p.Environments["dev"]
		if dev.TTLMinutes != 240 || dev.Boot != "baseline" || len(dev.Data) != 0 || len(dev.Run.Command) != 0 {
			t.Errorf("dev config = %+v", dev)
		}
		if !reflect.DeepEqual(b.plane.created[1].Services, []string{"stripe", "postgres"}) {
			t.Errorf("services sent = %v", b.plane.created[1].Services)
		}
	})

	t.Run("a taken name is refused before anything is sent", func(t *testing.T) {
		posts := len(b.plane.created)
		_, stderr, code := b.run("env", "create", "dev", "--services", "github", "--ttl", "5", "--boot", "bundle", "--data", "", "--command", "")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ Environment 'dev' already exists in .veris/twin.yaml", "→ Next: veris env create dev --force")
		if len(b.plane.created) != posts {
			t.Errorf("a POST was made for a refused name")
		}
	})

	t.Run("--force replaces it", func(t *testing.T) {
		_, stderr, code := b.run("env", "create", "dev", "--force", "--services", "github", "--ttl", "5", "--boot", "bundle", "--data", "", "--command", "")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		if dev := b.loadProject().Environments["dev"]; dev.TTLMinutes != 5 || dev.ID == id {
			t.Errorf("dev config after --force = %+v", dev)
		}
	})
}

func TestEnvCreateRefusesUnknownServices(t *testing.T) {
	b := newEnvBench(t)
	_, stderr, code := b.run("env", "create", "ci", "--services", "stripe,foo,bar", "--ttl", "20", "--boot", "bundle", "--data", "", "--command", "")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	wantLines(t, stderr, "✗ Unknown service(s): foo, bar\n  Available: stripe, postgres, github\n")
	if len(b.plane.created) != 0 {
		t.Errorf("POST made despite unknown services: %+v", b.plane.created)
	}
	if _, err := os.Stat(filepath.Join(b.project, ".veris", "twin.yaml")); err == nil {
		t.Errorf("project file written for a refused create")
	}
}

func TestEnvCreateFromAdoptsAServerEnvironment(t *testing.T) {
	b := newEnvBench(t)
	b.plane.addEnv(devID, "checkout-svc", []string{"stripe", "postgres"}, "")
	_, stderr, code := b.run("env", "create", "dev", "--from", devID, "--ttl", "240", "--boot", "bundle", "--data", "", "--command", "")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	wantLines(t, stderr,
		"✓ Adopted existing environment k3j2v0d8… (checkout-svc: stripe, postgres) as 'dev'",
		"✓ Added 'dev' to .veris/twin.yaml as the default")
	if len(b.plane.created) != 0 {
		t.Errorf("POST made for --from: %+v", b.plane.created)
	}
	if dev := b.loadProject().Environments["dev"]; dev.ID != devID || dev.TTLMinutes != 240 {
		t.Errorf("dev config = %+v", dev)
	}

	t.Run("an id the server does not know", func(t *testing.T) {
		_, stderr, code := b.run("env", "create", "x", "--from", probeID, "--ttl", "1", "--boot", "bundle", "--data", "", "--command", "")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ Failed to adopt environment "+probeID+": [404] environment "+probeID+" not found")
	})

	t.Run("--services beside --from is refused, not dropped", func(t *testing.T) {
		_, stderr, code := b.run("env", "create", "x", "--from", devID, "--services", "stripe,github", "--ttl", "1", "--boot", "bundle", "--data", "", "--command", "")
		if code != 1 || !strings.Contains(stderr, "--services cannot change an adopted environment") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestEnvCreateOffATTYNamesTheMissingFlag(t *testing.T) {
	b := newEnvBench(t)
	cases := []struct {
		args []string
		hint string
	}{
		{[]string{}, "NAME"},
		{[]string{"ci"}, "--services"},
		{[]string{"ci", "--services", "stripe"}, "--ttl"},
		{[]string{"ci", "--services", "stripe", "--ttl", "20"}, "--boot"},
		{[]string{"ci", "--services", "stripe", "--ttl", "20", "--boot", "snapshot"}, "--snapshot"},
		{[]string{"ci", "--services", "stripe", "--ttl", "20", "--boot", "bundle"}, "--data"},
		{[]string{"ci", "--services", "stripe", "--ttl", "20", "--boot", "bundle", "--data", ""}, "--command"},
	}
	for _, c := range cases {
		_, stderr, code := b.run(append([]string{"env", "create"}, c.args...)...)
		if code != 1 {
			t.Errorf("%v: exit %d, want 1", c.args, code)
		}
		wantLines(t, stderr, "Interactive prompt requires a TTY. Pass "+c.hint+" instead.")
	}
	if len(b.plane.created) != 0 {
		t.Errorf("POST made: %+v", b.plane.created)
	}
	t.Run("usage errors", func(t *testing.T) {
		_, stderr, code := b.run("env", "create", "ci", "--boot", "floppy")
		if code != 1 || !strings.Contains(stderr, "--boot must be one of bundle, baseline, snapshot") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		_, stderr, code = b.run("env", "create", "ci", "--command", "pytest 'unterminated")
		if code != 1 || !strings.Contains(stderr, "--command: unterminated ' quote") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestEnvCreateInterview(t *testing.T) {
	b := newEnvBench(t)
	// name, services, TTL (blank keeps 240), boot (2 = baseline), data,
	// command, default (blank keeps yes: it is the first environment).
	stdout, stderr, code := b.runTTY("staging-like\nstripe,postgres\n\n2\ndata/dev-customers.json\npytest -q\n\n", "env", "create")
	if code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, stdout, stderr)
	}
	wantLines(t, stderr,
		"? Environment name (proj): ",
		"? Select services:",
		"1) ◻ stripe  Stripe payments API  api.stripe.com",
		"2) ◻ postgres  Postgres data plane (DSN)  —",
		"? Sandbox TTL in minutes (240): ",
		"? Boot from:",
		"2) baseline  this environment's promoted snapshot (none yet)",
		"? Data files to add after boot (blank for none): ",
		"? Test command (runs through the proxy): ",
		"Make 'staging-like' this project's default environment? [Y/n] ",
		"✓ Environment created: "+b.plane.envs[0].ID+" (staging-like: stripe, postgres)",
		"✓ Added 'staging-like' to .veris/twin.yaml as the default")
	p := b.loadProject()
	conf := p.Environments["staging-like"]
	want := cfg.EnvConfig{ID: b.plane.envs[0].ID, TTLMinutes: 240, Boot: "baseline", Data: []string{"data/dev-customers.json"}, Run: cfg.RunConfig{Command: []string{"pytest", "-q"}}}
	if p.Default != "staging-like" || !reflect.DeepEqual(conf, want) {
		t.Errorf("default %q config %+v, want %+v", p.Default, conf, want)
	}

	t.Run("a bad TTL is asked again and the default question leans to no once there is a default", func(t *testing.T) {
		_, stderr, code := b.runTTY("abc\n30\ny\n", "env", "create", "ci", "--services", "stripe", "--boot", "bundle", "--data", "", "--command", "")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "! 'abc' is not a number of minutes", "Make 'ci' this project's default environment? [y/N] ")
		p := b.loadProject()
		if p.Default != "ci" || p.Environments["ci"].TTLMinutes != 30 {
			t.Errorf("default %q ttl %d", p.Default, p.Environments["ci"].TTLMinutes)
		}
	})

	t.Run("no keeps the default where it was", func(t *testing.T) {
		_, stderr, code := b.runTTY("n\n", "env", "create", "qa", "--services", "stripe", "--ttl", "9", "--boot", "bundle", "--data", "", "--command", "")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		if p := b.loadProject(); p.Default != "ci" {
			t.Errorf("default = %q, want ci", p.Default)
		}
	})

	t.Run("no services picked is refused", func(t *testing.T) {
		_, stderr, code := b.runTTY("\n", "env", "create", "empty", "--ttl", "9", "--boot", "bundle", "--data", "", "--command", "")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ An environment needs at least one service")
	})
}

func TestInitIsCreateDefault(t *testing.T) {
	b := newEnvBench(t)
	b.twoEnvs()
	_, stderr, code := b.run("init", "ci2", "--services", "stripe", "--ttl", "20", "--boot", "bundle", "--data", "", "--command", "")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	wantLines(t, stderr, "✓ Added 'ci2' to .veris/twin.yaml as the default")
	if p := b.loadProject(); p.Default != "ci2" || len(p.Environments) != 3 {
		t.Errorf("default %q, %d environments", p.Default, len(p.Environments))
	}
	t.Run("init is hidden from the root help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		_ = cli.Execute(root(), &cli.Globals{}, []string{"--help"}, &stdout, &stderr)
		if strings.Contains(stdout.String(), "\n  init ") {
			t.Errorf("init listed in root help:\n%s", stdout.String())
		}
	})
}

func TestEnvList(t *testing.T) {
	b := newEnvBench(t)
	b.plane.addEnv(devID, "checkout-svc", []string{"stripe", "postgres"}, "wrld-c9d2f4h6")
	b.plane.addEnv(ciID, "checkout-ci", []string{"stripe"}, "")
	b.plane.addEnv(probeID, "probe", []string{"stripe"}, "")
	b.plane.addSandbox(devID, sbID, api.StatusReady)
	b.plane.addSandbox(devID, "2b8x1k5m9p3r7t0v4y6z8a1c3", api.StatusFailed)
	b.plane.failListSB[probeID] = true

	t.Run("without a project file only the available block prints", func(t *testing.T) {
		_, stderr, code := b.run("env", "list")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantNot(t, stderr, "Configured")
		wantLines(t, stderr, "Available on "+b.srv.URL+" (Acme)")
	})

	b.projectFile(cfg.Project{
		Project: "proj",
		Default: "dev",
		Environments: map[string]cfg.EnvConfig{
			"dev": {ID: devID, TTLMinutes: 240, Boot: "baseline", Data: []string{"data/a.json", "data/b.json"}},
			"ci":  {ID: ciID, TTLMinutes: 20, Boot: "bundle"},
		},
	})

	t.Run("both blocks as the transcript has them", func(t *testing.T) {
		_, stderr, code := b.run("env", "list")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		out := squash(stderr)
		wantLines(t, out,
			"Configured (proj/.veris/twin.yaml)",
			"★ dev k3j2v0d8… stripe, postgres baseline wrld-c9d… ttl 240 data 2 files",
			"ci c1a2b3d4… stripe bundle ttl 20 —",
			"Available on "+b.srv.URL+" (Acme)",
			"k3j2v0d8… checkout-svc stripe, postgres → dev 1 live sandbox",
			"c1a2b3d4… checkout-ci stripe → ci —",
			"n8crn4cv… probe stripe — ? env create NAME --from n8crn4cv…",
			"! could not list the sandboxes of "+probeID+": [403] not yours")
		if i, j := strings.Index(stderr, "Configured"), strings.Index(stderr, "Available"); i > j {
			t.Errorf("Configured block after Available")
		}
	})

	t.Run("the local use is marked in use and the default keeps its star", func(t *testing.T) {
		b.local(cfg.Local{Use: "ci"})
		_, stderr, code := b.run("env", "list")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, squash(stderr), "★ dev k3j2v0d8…", "● ci c1a2b3d4…")
	})

	t.Run("--json carries both blocks and the counts", func(t *testing.T) {
		stdout, stderr, code := b.run("env", "list", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		var got envListed
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
		}
		if got.Org != "Acme" || len(got.Configured) != 2 || len(got.Available) != 3 {
			t.Errorf("json = %+v", got)
		}
		if got.Configured[0].Name != "ci" || !got.Configured[0].InUse || got.Configured[1].Name != "dev" || !got.Configured[1].Default {
			t.Errorf("configured = %+v", got.Configured)
		}
		if got.Available[0].Config != "dev" || got.Available[0].LiveSandboxes == nil || *got.Available[0].LiveSandboxes != 1 {
			t.Errorf("available[0] = %+v", got.Available[0])
		}
		if got.Available[2].LiveSandboxes != nil || got.Failed[probeID] == "" {
			t.Errorf("available[2] = %+v failed %v", got.Available[2], got.Failed)
		}
		// The config's keys are snake_case like every sibling's, so a script
		// written against env create --json reads env list --json too.
		var raw struct {
			Configured []struct {
				Config map[string]any `json:"config"`
			} `json:"configured"`
		}
		if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
			t.Fatal(err)
		}
		if ttl, ok := raw.Configured[0].Config["ttl_minutes"]; !ok || ttl != float64(20) {
			t.Errorf("configured[0].config.ttl_minutes = %v (%v); keys %v", ttl, ok, raw.Configured[0].Config)
		}
		if strings.Contains(stdout, "TTLMinutes") {
			t.Errorf("PascalCase keys in --json:\n%s", stdout)
		}
	})

	t.Run("a broken /v1/me is a warning, a 401 is not logged in", func(t *testing.T) {
		b.plane.meStatus = 403
		_, stderr, code := b.run("env", "list")
		b.plane.meStatus = 0
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "! could not read /v1/me", "Available on "+b.srv.URL+"\n")
		t.Setenv(cfg.EnvAPIKey, "vsk_wrong")
		_, stderr, code = b.run("env", "list")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ Not logged in for profile 'default': [401] invalid or missing API key", "→ Next: veris login --profile default")
	})
}

func TestEnvUse(t *testing.T) {
	b := newEnvBench(t)
	b.gitInit()
	b.twoEnvs()
	b.plane.addEnv(devID, "checkout-svc", []string{"stripe", "postgres"}, "")
	b.plane.addEnv(probeID, "probe", []string{"stripe"}, "")
	b.plane.addEnv("d1u2p3d4u5p6d7u8p9d0u1p2d", "dup", []string{"stripe"}, "")
	b.plane.addEnv("d2u2p3d4u5p6d7u8p9d0u1p2d", "dup", []string{"github"}, "")
	gitignore := filepath.Join(b.project, ".gitignore")

	t.Run("a project name writes the local file and ignores it", func(t *testing.T) {
		requests := len(b.plane.requests)
		_, stderr, code := b.run("env", "use", "ci")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr,
			"✓ Added .veris/twin.local.yaml to .gitignore",
			"✓ This folder now uses 'ci' (c1a2b3d4…) — written to .veris/twin.local.yaml",
			"→ Next: veris up")
		wantNot(t, stderr, "is not ignored by git")
		if l := b.loadLocal(); l.Use != "ci" {
			t.Errorf("local use = %q", l.Use)
		}
		if len(b.plane.requests) != requests {
			t.Errorf("the server was asked about a name the project file knows: %v", b.plane.requests[requests:])
		}
		body, _ := os.ReadFile(gitignore)
		if string(body) != ".veris/twin.local.yaml\n" {
			t.Errorf(".gitignore = %q", body)
		}
	})

	t.Run("a second use appends nothing more", func(t *testing.T) {
		_, stderr, code := b.run("env", "use", "dev")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantNot(t, stderr, "to .gitignore")
		body, _ := os.ReadFile(gitignore)
		if string(body) != ".veris/twin.local.yaml\n" {
			t.Errorf(".gitignore = %q", body)
		}
		if l := b.loadLocal(); l.Use != "dev" {
			t.Errorf("local use = %q", l.Use)
		}
	})

	t.Run("a server environment by id or exact name is kept by id", func(t *testing.T) {
		_, stderr, code := b.run("env", "use", "probe")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "✓ This folder now uses 'probe' (n8crn4cv…)")
		if l := b.loadLocal(); l.Use != probeID {
			t.Errorf("local use = %q, want the id", l.Use)
		}
		s, _ := open(t, cli.Globals{}, "", "")
		if s.res.EnvName != probeID || s.res.EnvSource != cfg.SourceLocal {
			t.Errorf("a new session resolves %q from %q", s.res.EnvName, s.res.EnvSource)
		}
	})

	t.Run("an unknown name and an ambiguous one", func(t *testing.T) {
		_, stderr, code := b.run("env", "use", "nosuch")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ No environment 'nosuch' in .veris/twin.yaml or on "+b.srv.URL, "→ Next: veris env list")
		_, stderr, code = b.run("env", "use", "dup")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ 'dup' names 2 environments on "+b.srv.URL+": d1u2p3d4u5p6d7u8p9d0u1p2d (stripe), d2u2p3d4u5p6d7u8p9d0u1p2d (github)", "→ Next: veris env use ID")
		if l := b.loadLocal(); l.Use != probeID {
			t.Errorf("local use changed to %q", l.Use)
		}
	})

	t.Run("the id of a configured environment is that entry", func(t *testing.T) {
		requests := len(b.plane.requests)
		_, stderr, code := b.run("env", "use", ciID)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "✓ This folder now uses 'ci' (c1a2b3d4…)")
		if l := b.loadLocal(); l.Use != "ci" {
			t.Errorf("local use = %q, want the config's name", l.Use)
		}
		if len(b.plane.requests) != requests {
			t.Errorf("the server was asked about an id the project file knows: %v", b.plane.requests[requests:])
		}
	})

	t.Run("--global writes the profile's default by id", func(t *testing.T) {
		_, stderr, code := b.run("env", "use", "ci", "--global")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "✓ Profile 'default' now defaults to 'ci' (c1a2b3d4…) — written to "+cfg.GlobalPath())
		g, err := cfg.LoadGlobal()
		if err != nil {
			t.Fatal(err)
		}
		if g.Profiles["default"].DefaultEnvironment != ciID || g.Profiles["default"].APIKey != envTestKey {
			t.Errorf("profile = %+v", g.Profiles["default"])
		}
	})

	t.Run("no NAME: a picker on a TTY, the argument otherwise", func(t *testing.T) {
		_, stderr, code := b.runTTY("dev\n", "env", "use")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "? Select an environment:", "1) ci  c1a2b3d4…", "2) dev  k3j2v0d8…", "✓ This folder now uses 'dev'")
		_, stderr, code = b.run("env", "use")
		if code != 1 || !strings.Contains(stderr, "Pass NAME instead.") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestEnvUseWithoutAProjectFile(t *testing.T) {
	b := newEnvBench(t)
	b.plane.addEnv(probeID, "probe", []string{"stripe"}, "")
	requests := len(b.plane.requests)
	_, stderr, code := b.run("env", "use", "probe")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	wantLines(t, stderr,
		"✗ No .veris/twin.yaml found (searched up from "+b.project+")",
		"→ Next: veris env create, or veris env use NAME --global")
	if len(b.plane.requests) != requests {
		t.Errorf("the server was asked before the missing project file was noticed: %v", b.plane.requests[requests:])
	}
	// Without a key the blocker is still the project file, not the login.
	t.Setenv(cfg.EnvAPIKey, "")
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{"default": {APIBase: b.srv.URL}}})
	_, stderr, code = b.run("env", "use", "probe")
	if code != 1 || strings.Contains(stderr, "Not logged in") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	// --global is the form for this folder.
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{"default": {APIBase: b.srv.URL, APIKey: envTestKey}}})
	_, stderr, code = b.run("env", "use", "probe", "--global")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	wantLines(t, stderr, "✓ Profile 'default' now defaults to 'probe' (n8crn4cv…)")
}

func TestEnvGet(t *testing.T) {
	b := newEnvBench(t)
	b.projectFile(cfg.Project{
		Project: "proj",
		Default: "dev",
		Environments: map[string]cfg.EnvConfig{
			"dev": {ID: devID, TTLMinutes: 240, Boot: "baseline", Data: []string{"data/a.json"},
				Proxy: cfg.ProxyConfig{RequireService: []string{"stripe"}, Strict: true}, Run: cfg.RunConfig{Command: []string{"pytest", "-q"}}},
			"ci": {ID: ciID},
		},
	})
	b.plane.addEnv(devID, "checkout-svc", []string{"stripe", "postgres"}, "wrld-c9d2f4h6")
	b.plane.addEnv(ciID, "checkout-ci", []string{"stripe"}, "")

	t.Run("the default, with every setting's source", func(t *testing.T) {
		_, stderr, code := b.run("env", "get")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		out := squash(stderr)
		wantLines(t, out,
			"Environment dev ("+devID+") project default (.veris/twin.yaml)",
			"Profile default → "+b.srv.URL+" active profile",
			"TTL 240 min .veris/twin.yaml",
			"Boot baseline .veris/twin.yaml",
			"Data data/a.json .veris/twin.yaml",
			"Callback — default",
			"Proxy require_service stripe · strict .veris/twin.yaml",
			"Command pytest -q .veris/twin.yaml",
			"Server record",
			"Name checkout-svc",
			"Services stripe, postgres",
			"Baseline wrld-c9d2f4h6 (repo@sha256:abc)",
			"Created 2026-09-01",
			"→ https://studio.example/environments/"+devID)
	})

	t.Run("a NAME argument, and the defaults of a bare config", func(t *testing.T) {
		_, stderr, code := b.run("env", "get", "ci")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		out := squash(stderr)
		wantLines(t, out, "Environment ci ("+ciID+") argument", "TTL 120 min default", "Boot bundle default", "Baseline none (boots the bundle)")
	})

	t.Run("--json", func(t *testing.T) {
		stdout, stderr, code := b.run("env", "get", "ci", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		var got envGot
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
		}
		if got.Name != "ci" || got.ID != ciID || got.Source != "flag" || got.Config == nil || got.Server == nil || got.Server.Name != "checkout-ci" {
			t.Errorf("json = %+v", got)
		}
		var raw struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw.Config["ttl_minutes"]; !ok || raw.Config["id"] != ciID {
			t.Errorf("config keys are not snake_case: %v", raw.Config)
		}
	})

	t.Run("a name neither the project nor the server knows", func(t *testing.T) {
		_, stderr, code := b.run("env", "get", "stagin")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ No environment 'stagin' in .veris/twin.yaml or on "+b.srv.URL, "→ Next: veris env list")
	})

	t.Run("a server id the project does not know is a bare id", func(t *testing.T) {
		b.plane.addEnv(probeID, "probe", []string{"stripe"}, "")
		_, stderr, code := b.run("env", "get", probeID)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, squash(stderr), "Environment probe ("+probeID+") argument", "Settings none: a bare id with no entry in .veris/twin.yaml", "Name probe")
	})

	t.Run("a server environment by exact name, as env use resolves it", func(t *testing.T) {
		_, stderr, code := b.run("env", "get", "probe")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, squash(stderr), "Environment probe ("+probeID+") argument", "Settings none: a bare id with no entry in .veris/twin.yaml", "Services stripe")
	})

	t.Run("without a project file the server list still answers", func(t *testing.T) {
		if err := os.RemoveAll(filepath.Join(b.project, ".veris")); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := b.run("env", "get", "probe")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, squash(stderr), "Environment probe ("+probeID+") argument", "Name probe")
	})
}

func TestEnvDelete(t *testing.T) {
	b := newEnvBench(t)

	t.Run("config only: the entry, the default and the local use go", func(t *testing.T) {
		b.twoEnvs()
		b.local(cfg.Local{Use: "dev", Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: devID}})
		_, stderr, code := b.run("env", "delete", "dev")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "✓ Removed 'dev' from .veris/twin.yaml (it was the default)")
		p := b.loadProject()
		if _, ok := p.Environments["dev"]; ok || p.Default != "" || len(p.Environments) != 1 {
			t.Errorf("project after delete: default %q envs %v", p.Default, p.Environments)
		}
		if l := b.loadLocal(); l.Use != "" || l.Sandbox == nil {
			t.Errorf("local after delete: use %q sandbox %v (the pointer is not this command's)", l.Use, l.Sandbox)
		}
		if len(b.plane.requests) != 0 {
			t.Errorf("the server was called: %v", b.plane.requests)
		}
		_, stderr, code = b.run("env", "delete", "dev")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr, "✗ No environment 'dev' in .veris/twin.yaml (have: ci)")
	})

	t.Run("--server refuses while sandboxes are live", func(t *testing.T) {
		b.twoEnvs()
		b.plane.addEnv(devID, "checkout-svc", []string{"stripe", "postgres"}, "")
		b.plane.addSandbox(devID, sbID, api.StatusReady)
		b.plane.addSandbox(devID, "2b8x1k5m9p3r7t0v4y6z8a1c3", api.StatusProvisioning)
		b.plane.addSandbox(devID, "f1a2i3l4e5d6f7a8i9l0e1d2f", api.StatusFailed)
		_, stderr, code := b.run("env", "delete", "dev", "--server", "--yes")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr,
			"✗ Environment has 2 live sandboxes ("+sbID+", 2b8x1k5m9p3r7t0v4y6z8a1c3).\n"+
				"  Once the record is deleted you cannot see or delete them until their TTL. Pass --cascade to delete them first.\n")
		if len(b.plane.deletedEnvs) != 0 || len(b.plane.deletedSandbox) != 0 {
			t.Errorf("deleted envs %v sandboxes %v", b.plane.deletedEnvs, b.plane.deletedSandbox)
		}
		if _, ok := b.loadProject().Environments["dev"]; !ok {
			t.Errorf("config removed although the server delete was refused")
		}
	})

	t.Run("--cascade stops at the first failure and keeps the record and the config", func(t *testing.T) {
		b.plane.failDeleteSB["2b8x1k5m9p3r7t0v4y6z8a1c3"] = true
		b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: devID}})
		_, stderr, code := b.run("env", "delete", "dev", "--server", "--cascade", "--yes")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		wantLines(t, stderr,
			"Delete 2 sandboxes and environment \"checkout-svc\"? y",
			"✓ Sandbox deleted: "+sbID,
			"✗ Failed to delete sandbox 2b8x1k5m9p3r7t0v4y6z8a1c3: [500] node unreachable")
		wantNot(t, stderr, "Environment deleted")
		if !reflect.DeepEqual(b.plane.deletedSandbox, []string{sbID}) || len(b.plane.deletedEnvs) != 0 {
			t.Errorf("deleted sandboxes %v envs %v", b.plane.deletedSandbox, b.plane.deletedEnvs)
		}
		if _, ok := b.loadProject().Environments["dev"]; !ok {
			t.Errorf("config removed over a survivor")
		}
		if l := b.loadLocal(); l.Sandbox != nil {
			t.Errorf("the deleted sandbox is still this folder's pointer: %+v", l.Sandbox)
		}
	})

	t.Run("--cascade deletes the rest, then the record, then the config", func(t *testing.T) {
		delete(b.plane.failDeleteSB, "2b8x1k5m9p3r7t0v4y6z8a1c3")
		_, stderr, code := b.run("env", "delete", "dev", "--server", "--cascade", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr,
			"Delete 1 sandbox and environment \"checkout-svc\"? y",
			"✓ Sandbox deleted: 2b8x1k5m9p3r7t0v4y6z8a1c3",
			"✓ Environment deleted: "+devID,
			"✓ Removed 'dev' from .veris/twin.yaml (it was the default)")
		if !reflect.DeepEqual(b.plane.deletedEnvs, []string{devID}) {
			t.Errorf("deleted envs %v", b.plane.deletedEnvs)
		}
		if _, ok := b.loadProject().Environments["dev"]; ok {
			t.Errorf("config kept after the server delete")
		}
	})

	t.Run("--server off a TTY needs --yes; a server-only id needs no project entry", func(t *testing.T) {
		b.plane.addEnv(probeID, "probe", []string{"stripe"}, "")
		_, stderr, code := b.run("env", "delete", probeID, "--server")
		if code != 1 || !strings.Contains(stderr, "Pass --yes instead.") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		_, stderr, code = b.runTTY("n\n", "env", "delete", probeID, "--server")
		if code != 1 || !strings.Contains(stderr, "Delete environment \"probe\" ("+probeID+")? [y/N] ") || !strings.Contains(stderr, "veris: declined") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		_, stderr, code = b.run("env", "delete", "probe", "--server", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "✓ Environment deleted: "+probeID)
		wantNot(t, stderr, "Removed")
	})

	t.Run("a record already gone is a warning; the config still goes", func(t *testing.T) {
		_, stderr, code := b.run("env", "delete", "ci", "--server", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		wantLines(t, stderr, "! environment "+ciID+" is already gone on the server", "✓ Removed 'ci' from .veris/twin.yaml")
	})

	t.Run("usage", func(t *testing.T) {
		if _, stderr, code := b.run("env", "delete"); code != 1 || !strings.Contains(stderr, "env delete needs exactly one NAME") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		if _, stderr, code := b.run("env", "delete", "x", "--cascade"); code != 1 || !strings.Contains(stderr, "--cascade goes with --server") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestEnvSplitWords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"pytest -q", []string{"pytest", "-q"}},
		{`pytest -q "tests/a b" 'it''s' x\ y`, []string{"pytest", "-q", "tests/a b", "its", "x y"}},
		{`echo "a \"quoted\" \$word" ''`, []string{"echo", `a "quoted" $word`, ""}},
		{"  spaced\tout  ", []string{"spaced", "out"}},
		{"", nil},
	}
	for _, c := range cases {
		got, err := splitWords(c.in)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitWords(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := splitWords(`pytest "unterminated`); err == nil || err.Error() != `unterminated " quote` {
		t.Errorf("unterminated: err = %v", err)
	}
}
