package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
)

const (
	otherSbID = "2c8v4b6n1m3k5j7h9g0f2d4s6"
	sbTestKey = "vsk_test00000000000000000"
)

// sandboxBench is a bench with a logged-in profile pointing at a fake
// control plane, and up's waits turned from seconds into milliseconds.
func sandboxBench(t *testing.T, planeURL string) *bench {
	t.Helper()
	b := newBench(t)
	b.global(cfg.Global{
		ActiveProfile: "default",
		Profiles: map[string]cfg.Profile{
			"default": {APIBase: planeURL, APIKey: sbTestKey, ConsoleURL: "https://studio.example"},
		},
	})
	poll, routable, probe := sandboxPollInterval, routableInterval, twinProbeTimeout
	sandboxPollInterval, routableInterval, twinProbeTimeout = 10*time.Millisecond, 10*time.Millisecond, 2*time.Second
	t.Cleanup(func() { sandboxPollInterval, routableInterval, twinProbeTimeout = poll, routable, probe })
	return b
}

// runSandboxCLI drives the binary's tree end to end, off a TTY, and returns
// the exit status main would have used with both streams.
func runSandboxCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	in, hook := stdin, newSessionHook
	stdin = strings.NewReader("")
	newSessionHook = func(s *session) { s.ui.TTY = false }
	t.Cleanup(func() { stdin, newSessionHook = in, hook })
	var out, errOut bytes.Buffer
	err := cli.Execute(root(), &cli.Globals{}, args, &out, &errOut)
	return exitStatusTo(&errOut, err), out.String(), errOut.String()
}

// sbInOrder asserts that every want appears in got, each after the previous.
func sbInOrder(t *testing.T, got string, want ...string) {
	t.Helper()
	rest := got
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Errorf("missing (or out of order) %q in:\n%s", w, got)
			return
		}
		rest = rest[i+len(w):]
	}
}

func sbJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func at(t time.Time) api.Time { return api.Time{Time: t} }

// sbPointer reads the local file's sandbox pointer, nil when none.
func sbPointer(t *testing.T, b *bench) *cfg.SandboxRef {
	t.Helper()
	l, err := cfg.LoadLocal(filepath.Join(b.project, ".veris", "twin.local.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return l.Sandbox
}

// sandboxTwins serves /veris/* for a stripe and a postgres twin under
// /s/<sandbox>/<twin>. The test scripts each answer through the setters;
// the getters say what was asked. Everything is behind the mutex because
// the handlers run on the server's goroutines.
type sandboxTwins struct {
	srv *httptest.Server
	mu  sync.Mutex

	healthFailures int            // stripe /veris/health answers 502 this many times first
	healthCalls    int            // stripe health probes seen
	addStatus      int            // stripe POST /veris/data status (0 → 200)
	addBody        map[string]any // what the last POST /veris/data carried
	counts         map[string]int // stripe GET /veris/data counts
	seedStatus     int            // postgres POST /veris/seed status (0 → 200)
	seedSQL        string         // the schema_sql the last seed carried
}

func newSandboxTwins(t *testing.T) *sandboxTwins {
	t.Helper()
	f := &sandboxTwins{}
	mux := http.NewServeMux()
	mux.HandleFunc("/s/"+sbID+"/stripe/veris/health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.healthCalls++
		if f.healthCalls <= f.healthFailures {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
			return
		}
		sbJSON(w, 200, map[string]any{"status": "ok", "service": "stripe", "state_version": 3})
	})
	mux.HandleFunc("/s/"+sbID+"/postgres/veris/health", func(w http.ResponseWriter, r *http.Request) {
		sbJSON(w, 200, map[string]any{"status": "ok", "service": "postgres"})
	})
	mux.HandleFunc("/s/"+sbID+"/stripe/veris/data", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			sbJSON(w, 200, map[string]any{"counts": f.counts, "state_version": 3})
		case http.MethodPost:
			var body struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.addBody = body.Data
			if f.addStatus == 422 {
				sbJSON(w, 422, map[string]any{"detail": []string{"customers[0].email: must be a string"}})
				return
			}
			added := map[string]int{}
			for table, rows := range body.Data {
				if list, ok := rows.([]any); ok {
					added[table] = len(list)
				}
			}
			sbJSON(w, 200, map[string]any{"added": added})
		}
	})
	// The postgres twin serves no data listing: FastAPI's own 404.
	mux.HandleFunc("/s/"+sbID+"/postgres/veris/data", func(w http.ResponseWriter, r *http.Request) {
		sbJSON(w, 404, map[string]any{"detail": "Not Found"})
	})
	mux.HandleFunc("POST /s/"+sbID+"/postgres/veris/seed", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			SchemaSQL string `json:"schema_sql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.seedSQL = body.SchemaSQL
		if f.seedStatus != 0 {
			sbJSON(w, f.seedStatus, map[string]any{"detail": "Not Found"})
			return
		}
		sbJSON(w, 200, map[string]any{"status": "ok", "tables": 2})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *sandboxTwins) script(fn func(f *sandboxTwins)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *sandboxTwins) probes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthCalls
}

func (f *sandboxTwins) lastAdd() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addBody
}

func (f *sandboxTwins) lastSeed() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seedSQL
}

// services is the sandbox's service list as the control plane reports it:
// stripe proxied through the gateway (its control URL is the fake twin),
// postgres a data-plane DSN, with a control URL only when withPGControl.
func (f *sandboxTwins) services(withPGControl bool) []api.ServiceInfo {
	stripe := f.srv.URL + "/s/" + sbID + "/stripe"
	pg := api.ServiceInfo{Name: "postgres", Status: "ready", URL: "postgresql://app:app@10.0.0.5:5432/sb?sslmode=require", EnvHint: "DATABASE_URL"}
	if withPGControl {
		pg.ControlURL = f.srv.URL + "/s/" + sbID + "/postgres"
	}
	return []api.ServiceInfo{
		{Name: "stripe", Status: "ready", URL: stripe, ControlURL: stripe, EnvHint: "STRIPE_API_BASE"},
		pg,
	}
}

// sandboxPlane is a control plane for one test: the environment records, a
// scripted answer per poll of the sandbox, and a record of what was sent.
// Tests change the script through script() and read the record through the
// getters, so nothing races the handlers.
type sandboxPlane struct {
	srv *httptest.Server
	mu  sync.Mutex

	envs      map[string]api.Environment
	snapshots []api.Snapshot
	// answer is GET /v1/sandboxes/{id}'s reply for the nth poll (1-based);
	// nil, or an id other than the one asked for, is a 404.
	answer func(poll int) *api.Sandbox
	polls  int

	created     *api.CreateSandboxRequest
	deleted     []string
	deleteState int // DELETE's status (0 → 204)
	reset       func() (int, any)
	resets      int
	listAll     func() (int, any) // GET /v1/sandboxes; nil → 404
	lists       map[string]func() (int, any)
}

func newSandboxPlane(t *testing.T) *sandboxPlane {
	t.Helper()
	p := &sandboxPlane{
		envs: map[string]api.Environment{
			devID: {ID: devID, Name: "checkout-svc", Services: []string{"stripe", "postgres"}},
			ciID:  {ID: ciID, Name: "checkout-ci", Services: []string{"stripe", "postgres"}},
		},
		lists: map[string]func() (int, any){},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		sbJSON(w, 200, []api.Environment{p.envs[devID], p.envs[ciID]})
	})
	mux.HandleFunc("GET /v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		env, ok := p.envs[r.PathValue("id")]
		if !ok {
			sbJSON(w, 404, map[string]string{"detail": "environment " + r.PathValue("id") + " not found"})
			return
		}
		sbJSON(w, 200, env)
	})
	mux.HandleFunc("GET /v1/environments/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		sbJSON(w, 200, p.snapshots)
	})
	mux.HandleFunc("POST /v1/environments/{id}/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var req api.CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sbJSON(w, 422, map[string]string{"detail": err.Error()})
			return
		}
		p.created = &req
		sbJSON(w, 201, api.Sandbox{ID: sbID, EnvironmentID: r.PathValue("id"), Status: api.StatusProvisioning,
			CreatedAt: at(time.Now()), ExpiresAt: at(time.Now().Add(30 * time.Minute))})
	})
	mux.HandleFunc("GET /v1/environments/{id}/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		h := p.lists[r.PathValue("id")]
		if h == nil {
			sbJSON(w, 200, []api.Sandbox{})
			return
		}
		status, body := h()
		sbJSON(w, status, body)
	})
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.listAll == nil {
			sbJSON(w, 404, map[string]string{"detail": "Not Found"})
			return
		}
		status, body := p.listAll()
		sbJSON(w, status, body)
	})
	sandbox := func(w http.ResponseWriter, r *http.Request) *api.Sandbox {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.polls++
		var sb *api.Sandbox
		if p.answer != nil {
			sb = p.answer(p.polls)
		}
		if sb == nil || sb.ID != r.PathValue("id") {
			sbJSON(w, 404, map[string]string{"detail": "sandbox " + r.PathValue("id") + " not found"})
			return nil
		}
		return sb
	}
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if sb := sandbox(w, r); sb != nil {
			sbJSON(w, 200, sb)
		}
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/services", func(w http.ResponseWriter, r *http.Request) {
		if sb := sandbox(w, r); sb != nil {
			sbJSON(w, 200, sb.Services)
		}
	})
	mux.HandleFunc("DELETE /v1/environments/{env}/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.deleted = append(p.deleted, r.PathValue("env")+"/"+r.PathValue("id"))
		if p.deleteState == 404 {
			sbJSON(w, 404, map[string]string{"detail": "sandbox " + r.PathValue("id") + " not found"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/environments/{env}/sandboxes/{id}/reset", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.resets++
		status, body := p.reset()
		sbJSON(w, status, body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		sbJSON(w, 404, map[string]string{"detail": "Not Found"})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *sandboxPlane) script(fn func(p *sandboxPlane)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

func (p *sandboxPlane) createdReq() *api.CreateSandboxRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.created
}

func (p *sandboxPlane) deletedIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deleted...)
}

func (p *sandboxPlane) resetCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resets
}

// readySandbox is sbID ready in ci with the given services.
func readySandbox(services []api.ServiceInfo, expires time.Time) *api.Sandbox {
	return &api.Sandbox{ID: sbID, EnvironmentID: ciID, Status: api.StatusReady,
		CreatedAt: at(expires.Add(-30 * time.Minute)), ExpiresAt: at(expires), Services: services,
		Metadata: map[string]string{"project": "proj"}}
}

// ciProject writes a project file whose ci environment has a ttl and one
// data file, and the data file itself.
func ciProject(t *testing.T, b *bench, data string) {
	t.Helper()
	b.projectFile(cfg.Project{
		Project: "proj",
		Default: "ci",
		Environments: map[string]cfg.EnvConfig{
			"dev": {ID: devID, TTLMinutes: 240, Boot: "baseline"},
			"ci":  {ID: ciID, TTLMinutes: 20, Data: []string{"data/customers.json"}},
		},
	})
	if err := os.MkdirAll(filepath.Join(b.project, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.project, "data", "customers.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

const customersJSON = `{"stripe": {"customers": [{"id": "cus_1", "email": "ada@example.com"}], "payment_methods": [{"id": "pm_1"}]}}`

func TestUpProvisionsWaitsProbesAndSeeds(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	ciProject(t, b, customersJSON)
	expires := time.Now().Add(30 * time.Minute)
	twins.script(func(f *sandboxTwins) { f.healthFailures = 1 })
	plane.script(func(p *sandboxPlane) {
		p.answer = func(poll int) *api.Sandbox {
			if poll == 1 {
				return &api.Sandbox{ID: sbID, EnvironmentID: ciID, Status: api.StatusProvisioning}
			}
			return readySandbox(twins.services(false), expires)
		}
	})

	code, stdout, stderr := runSandboxCLI(t, "up")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty without --json, got %q", stdout)
	}
	stripeURL := twins.srv.URL + "/s/" + sbID + "/stripe"
	sbInOrder(t, stderr,
		"Starting 'ci' (checkout-ci: stripe, postgres) · boot bundle · ttl 20 min\n",
		"✓ Sandbox created: "+sbID+"\n",
		"Waiting for "+sbID+" · provisioning\n",
		"Waiting for "+sbID+" · ready\n",
		"  ✓ postgres  ready  (data plane; handed to the app, not proxied)\n",
		"502 from gateway — retrying",
		"  ✓ stripe    routable  ",
		" ms\n",
		"  stripe     STRIPE_API_BASE="+stripeURL+"\n",
		"  postgres   DATABASE_URL=postgresql://app:app@10.0.0.5:5432/sb?sslmode=require\n",
		"             (data plane; handed to the app, not proxied)\n",
		"✓ Added data/customers.json: stripe customers 1, payment_methods 1\n",
		"✓ Up: "+sbID+" is this folder's sandbox (expires "+expires.Local().Format("15:04")+")\n",
		"→ https://studio.example/sandboxes/"+sbID+"\n",
		"→ Next: veris run\n",
	)
	if strings.Contains(stderr, "not ignored by git") {
		t.Errorf("a temp dir is no repository; no gitignore warning expected:\n%s", stderr)
	}
	created := plane.createdReq()
	if created == nil || created.TTLMinutes == nil || *created.TTLMinutes != 20 ||
		created.SnapshotID != nil || created.ClientBaseURL != nil || created.Metadata["project"] != "proj" {
		t.Errorf("create request = %+v, want ttl 20 from the config, no snapshot, metadata project proj", created)
	}
	if n := twins.probes(); n != 2 {
		t.Errorf("health probes = %d, want 2 (one 502, one ok)", n)
	}
	if got := twins.lastAdd(); got == nil || len(got["customers"].([]any)) != 1 {
		t.Errorf("twin received data %v, want the file's stripe block", got)
	}
	if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID || ptr.EnvironmentID != ciID {
		t.Errorf("local pointer = %+v, want %s in %s", ptr, sbID, ciID)
	}
}

func TestUpFlagsBeatTheConfigAndJSONGoesToStdout(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	expires := time.Now().Add(5 * time.Minute)
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(nil, expires) }
	})

	code, stdout, stderr := runSandboxCLI(t, "up", "ci", "--ttl", "5", "--callback-url", "https://cb.example/hooks", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	var body api.Sandbox
	if err := json.Unmarshal([]byte(stdout), &body); err != nil || body.ID != sbID || body.Status != api.StatusReady {
		t.Errorf("stdout is not the sandbox body: %v\n%s", err, stdout)
	}
	if strings.Contains(stderr, "{") {
		t.Errorf("JSON leaked onto stderr:\n%s", stderr)
	}
	created := plane.createdReq()
	if created == nil || *created.TTLMinutes != 5 || created.ClientBaseURL == nil || *created.ClientBaseURL != "https://cb.example/hooks" {
		t.Errorf("create request = %+v, want ttl 5 and the callback URL", created)
	}
	sbInOrder(t, stderr, "Starting 'ci' (checkout-ci: stripe, postgres) · boot bundle · ttl 5 min", "✓ Up: "+sbID)

	// dev's ttl comes from its config; nothing in ci's config means 120.
	if code, _, stderr = runSandboxCLI(t, "up", "dev"); code != 0 || !strings.Contains(stderr, "· ttl 240 min") {
		t.Errorf("exit %d, want dev's ttl 240:\n%s", code, stderr)
	}
	if code, _, stderr = runSandboxCLI(t, "up", "ci"); code != 0 || !strings.Contains(stderr, "· ttl 120 min") {
		t.Errorf("exit %d, want the default ttl 120:\n%s", code, stderr)
	}
	// --env is the positional's alias; naming two environments is a mistake.
	if code, _, stderr = runSandboxCLI(t, "up", "--env", "ci"); code != 0 || !strings.Contains(stderr, "Starting 'ci' ") {
		t.Errorf("exit %d, want --env ci:\n%s", code, stderr)
	}
	if code, _, stderr = runSandboxCLI(t, "up", "ci", "--env", "dev"); code != 1 || !strings.Contains(stderr, `veris: up was given both NAME "ci" and --env "dev"`) {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}

func TestUpReportsAFailedSandboxAndKeepsThePointer(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox {
			return &api.Sandbox{ID: sbID, EnvironmentID: devID, Status: api.StatusFailed, FailureReason: "image pull backoff"}
		}
	})
	code, _, stderr := runSandboxCLI(t, "up")
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "✗ Sandbox "+sbID+" failed: image pull backoff\n") {
		t.Errorf("failure line missing:\n%s", stderr)
	}
	if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
		t.Errorf("pointer = %+v, want kept so `veris down` finds it", ptr)
	}
}

func TestUpTimesOutAndKeepsTheSandbox(t *testing.T) {
	t.Run("still provisioning at the deadline is exit 4", func(t *testing.T) {
		plane := newSandboxPlane(t)
		b := sandboxBench(t, plane.srv.URL)
		b.twoEnvs()
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox {
				return &api.Sandbox{ID: sbID, EnvironmentID: devID, Status: api.StatusProvisioning}
			}
		})
		code, _, stderr := runSandboxCLI(t, "up", "--timeout", "30ms")
		if code != 4 {
			t.Errorf("exit %d, want 4:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"! Sandbox "+sbID+" is still provisioning after 30ms; it is kept and may still come up\n",
			"→ Next: veris status\n")
		if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
			t.Errorf("pointer = %+v, want kept", ptr)
		}
	})

	t.Run("ready but never routable is exit 4 too", func(t *testing.T) {
		plane := newSandboxPlane(t)
		twins := newSandboxTwins(t)
		b := sandboxBench(t, plane.srv.URL)
		b.twoEnvs()
		twins.script(func(f *sandboxTwins) { f.healthFailures = 1 << 30 })
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(false), time.Now().Add(time.Hour)) }
		})
		code, _, stderr := runSandboxCLI(t, "up", "ci", "--timeout", "200ms")
		if code != 4 {
			t.Errorf("exit %d, want 4:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"! Sandbox "+sbID+" is ready but not routable after 200ms (stripe: 502 from gateway); it is kept and may still come up\n",
			"→ Next: veris status\n")
		if strings.Contains(stderr, "routable  ") {
			t.Errorf("stripe must not be reported routable:\n%s", stderr)
		}
	})

	t.Run("a timeout that is not a duration is refused", func(t *testing.T) {
		plane := newSandboxPlane(t)
		b := sandboxBench(t, plane.srv.URL)
		b.twoEnvs()
		code, _, stderr := runSandboxCLI(t, "up", "--timeout", "soon")
		if code != 1 || !strings.Contains(stderr, "veris: --timeout: 'soon' is not a duration") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestUpRefusesA422FromADataFile(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	ciProject(t, b, customersJSON)
	twins.script(func(f *sandboxTwins) { f.addStatus = 422 })
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(false), time.Now().Add(time.Hour)) }
	})

	code, _, stderr := runSandboxCLI(t, "up")
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"  ✓ stripe    routable  ",
		"✗ Failed to add data/customers.json to stripe: [422]\n",
		"  customers[0].email: must be a string\n")
	if strings.Contains(stderr, "✓ Up:") || strings.Contains(stderr, "→ Next: veris run") {
		t.Errorf("a failed seed must not end in ✓ Up:\n%s", stderr)
	}
	if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
		t.Errorf("pointer = %+v, want kept", ptr)
	}

	t.Run("a twin the sandbox does not run", func(t *testing.T) {
		ciProject(t, b, `{"shopify": {"products": []}}`)
		code, _, stderr := runSandboxCLI(t, "up")
		if code != 1 || !strings.Contains(stderr, "✗ data/customers.json names twin 'shopify', which sandbox "+sbID+" does not run (have: stripe, postgres)") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("a data file that is not there", func(t *testing.T) {
		ciProject(t, b, customersJSON)
		if err := os.Remove(filepath.Join(b.project, "data", "customers.json")); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runSandboxCLI(t, "up")
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read data/customers.json: ") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestUpSeedsPostgresFromSQL(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	ciProject(t, b, `{"postgres": {"sql": "data/schema.sql"}}`)
	const schema = "create table customers (id text primary key);\ncreate table orders (id text primary key);\n"
	if err := os.WriteFile(filepath.Join(b.project, "data", "schema.sql"), []byte(schema), 0o644); err != nil {
		t.Fatal(err)
	}
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(true), time.Now().Add(time.Hour)) }
	})

	code, _, stderr := runSandboxCLI(t, "up")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"  ✓ postgres  routable  ",
		fmt.Sprintf("✓ Added data/customers.json: postgres data/schema.sql (%d bytes)\n", len(schema)),
		"✓ Up: "+sbID+" is this folder's sandbox")
	if got := twins.lastSeed(); got != schema {
		t.Errorf("the seed carried %q, want the file's contents", got)
	}

	t.Run("a twin without the route", func(t *testing.T) {
		twins.script(func(f *sandboxTwins) { f.seedStatus = 404 })
		code, _, stderr := runSandboxCLI(t, "up")
		if code != 1 {
			t.Errorf("exit %d, want 1:\n%s", code, stderr)
		}
		if !strings.Contains(stderr, "✗ Failed to seed postgres from data/schema.sql: ") {
			t.Errorf("stderr:\n%s", stderr)
		}
		if strings.Contains(stderr, "✓ Up:") {
			t.Errorf("a failed seed must not end in ✓ Up:\n%s", stderr)
		}
		if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
			t.Errorf("pointer = %+v, want kept", ptr)
		}
	})

	t.Run("a schema file that is not there", func(t *testing.T) {
		if err := os.Remove(filepath.Join(b.project, "data", "schema.sql")); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runSandboxCLI(t, "up")
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read data/schema.sql: ") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestUpAnnouncesThePointerItReplaces(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox {
			sb := readySandbox(nil, time.Now().Add(time.Hour))
			sb.EnvironmentID = devID
			return sb
		}
	})
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: otherSbID, EnvironmentID: devID, ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}})
	code, _, stderr := runSandboxCLI(t, "up")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"! This folder already pointed at sandbox "+otherSbID+"; it keeps running until its TTL (veris sandbox delete --id "+otherSbID+")\n",
		"✓ Sandbox created: "+sbID+"\n")

	// One whose recorded expiry has passed is gone on its own.
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: otherSbID, EnvironmentID: devID, ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}})
	code, _, stderr = runSandboxCLI(t, "up")
	if code != 0 || strings.Contains(stderr, "already pointed") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}

func TestUpBootSources(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.projectFile(cfg.Project{
		Project: "proj",
		Default: "ci",
		Environments: map[string]cfg.EnvConfig{
			"dev":   {ID: devID, Boot: "baseline"},
			"ci":    {ID: ciID},
			"night": {ID: ciID, Boot: "snapshot", Snapshot: "nightly"},
		},
	})
	older := "snapaaaaaaaaaaaaaaaaaaaaa"
	newer := "snapbbbbbbbbbbbbbbbbbbbbb"
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(nil, time.Now().Add(time.Hour)) }
		p.snapshots = []api.Snapshot{
			{ID: older, Name: "nightly", CreatedAt: at(time.Now().Add(-48 * time.Hour))},
			{ID: newer, Name: "nightly", CreatedAt: at(time.Now().Add(-24 * time.Hour))},
			{ID: "snapccccccccccccccccccccc", Name: "golden", CreatedAt: at(time.Now().Add(-1 * time.Hour))},
		}
	})
	snapshotSent := func(t *testing.T) string {
		t.Helper()
		created := plane.createdReq()
		if created == nil || created.SnapshotID == nil {
			t.Fatalf("create request %+v carries no snapshot_id", created)
		}
		return *created.SnapshotID
	}

	t.Run("a snapshot name resolves to the newest of that name", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "up", "ci", "--snapshot", "nightly")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"! 2 snapshots are named 'nightly'; using the newest, "+newer+" (",
			"· boot snapshot "+shortID(newer)+" · ttl 120 min")
		if got := snapshotSent(t); got != newer {
			t.Errorf("snapshot_id = %s, want %s", got, newer)
		}
	})

	t.Run("the config's snapshot, and an id-shaped --snapshot as given", func(t *testing.T) {
		if code, _, stderr := runSandboxCLI(t, "up", "night"); code != 0 || snapshotSent(t) != newer {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		if code, _, stderr := runSandboxCLI(t, "up", "ci", "--boot", "snapshot", "--snapshot", older); code != 0 || snapshotSent(t) != older {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("an unknown snapshot name, and none at all", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "up", "ci", "--snapshot", "weekly")
		if code != 1 || !strings.Contains(stderr, "✗ No snapshot named 'weekly' in environment "+shortID(ciID)+" (have: nightly, nightly, golden)") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		code, _, stderr = runSandboxCLI(t, "up", "ci", "--boot", "snapshot")
		if code != 1 || !strings.Contains(stderr, "✗ --boot snapshot needs --snapshot ID|NAME") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("baseline warns when the environment has none", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "up", "ci", "--boot", "baseline")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"! Environment 'ci' has no baseline; the sandbox boots the bundle\n",
			"Starting 'ci' (checkout-ci: stripe, postgres) · boot bundle ·")
		if created := plane.createdReq(); created.SnapshotID != nil {
			t.Errorf("baseline sends no snapshot_id, got %v", *created.SnapshotID)
		}
	})

	t.Run("baseline names the pinned revision", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) {
			env := p.envs[devID]
			env.Baseline = &api.EnvironmentBaseline{Image: "reg/ci@sha256:abc", RevisionID: "wrld-c9d2f4h6"}
			p.envs[devID] = env
			p.answer = func(int) *api.Sandbox {
				sb := readySandbox(nil, time.Now().Add(time.Hour))
				sb.EnvironmentID = devID
				return sb
			}
		})
		code, _, stderr := runSandboxCLI(t, "up", "dev")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		if !strings.Contains(stderr, "Starting 'dev' (checkout-svc: stripe, postgres) · boot baseline wrld-c9d… ·") {
			t.Errorf("baseline revision expected in the Starting line:\n%s", stderr)
		}
	})

	t.Run("a boot word that is not one of the three", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "up", "--boot", "image")
		if code != 1 || !strings.Contains(stderr, "✗ --boot must be bundle, baseline or snapshot (got 'image')") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("--snapshot beside a boot that ignores it is refused", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "up", "ci", "--boot", "baseline", "--snapshot", "nightly")
		if code != 1 || !strings.Contains(stderr, "✗ --snapshot only applies with --boot snapshot (got --boot baseline)") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestUpNeedsAnEnvironmentAndALogin(t *testing.T) {
	b := newBench(t)
	code, _, stderr := runSandboxCLI(t, "up")
	if code != 1 || !strings.Contains(stderr, "✗ No environment selected\n→ Next: veris env use NAME, or pass --env\n") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	b.twoEnvs()
	code, _, stderr = runSandboxCLI(t, "up")
	if code != 1 || !strings.Contains(stderr, "✗ Not logged in for profile 'default' (no API key)\n→ Next: veris login --profile default\n") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
	if code, _, stderr = runSandboxCLI(t, "up", "ci", "dev"); code != 1 || !strings.Contains(stderr, "veris: up takes one environment name") {
		t.Errorf("exit %d:\n%s", code, stderr)
	}
}

func TestStatusPrintsThePanel(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	expires := time.Now().Add(3 * time.Hour)
	twins.script(func(f *sandboxTwins) {
		f.counts = map[string]int{"customers": 41, "payment_methods": 13, "faults": 0, "clock": 1, "client": 1}
	})
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(true), expires) }
	})

	t.Run("status is this folder's sandbox", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "status")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"Sandbox "+sbID+"\n",
			"Environment: checkout-ci ("+ciID+") → ci\n",
			"Status:      ready\n",
			"Boot:        bundle\n",
			"Expires:     "+expires.Local().Format("2006-01-02 15:04:05")+"\n",
			"  Twin      Status  Env hint         URL",
			"Tables\n",
			"  stripe    ready   STRIPE_API_BASE  "+twins.srv.URL+"/s/"+sbID+"/stripe",
			"customers 41 · faults 0 · payment_methods 13\n",
			"  postgres  ready   DATABASE_URL     postgresql://app:app@10.0.0.5:5432/sb?sslmode=require",
			"—\n")
	})

	t.Run("sandbox get --id, and --json", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox {
				return &api.Sandbox{ID: otherSbID, EnvironmentID: devID, Status: api.StatusDegraded,
					SnapshotID: "snapbbbbbbbbbbbbbbbbbbbbb", ExpiresAt: at(expires)}
			}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "get", "--id", otherSbID)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "Sandbox "+otherSbID+"\n", "Environment: checkout-svc ("+devID+") → dev\n",
			"Status:      degraded\n", "Boot:        snapshot snapbbbbbbbbbbbbbbbbbbbbb\n")
		if strings.Contains(stderr, "Twin") {
			t.Errorf("no services, no table:\n%s", stderr)
		}
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "get", "--id", otherSbID, "--json")
		var body api.Sandbox
		if code != 0 || json.Unmarshal([]byte(stdout), &body) != nil || body.ID != otherSbID {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		if strings.Contains(stderr, "Sandbox "+otherSbID) {
			t.Errorf("--json must not print the panel:\n%s", stderr)
		}
	})

	t.Run("a sandbox the plane no longer has", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) { p.answer = nil })
		code, _, stderr := runSandboxCLI(t, "status")
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read sandbox "+sbID+": [404] sandbox "+sbID+" not found\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("no pointer", func(t *testing.T) {
		b.local(cfg.Local{})
		code, _, stderr := runSandboxCLI(t, "status")
		if code != 1 || !strings.Contains(stderr, "✗ No sandbox for this folder\n→ Next: veris up\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestSandboxListFansOutWhenThereIsNoListAllRoute(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID}})
	now := time.Now()
	ours := api.Sandbox{ID: sbID, EnvironmentID: ciID, Status: api.StatusReady, CreatedAt: at(now),
		ExpiresAt: at(now.Add(time.Hour)), Services: []api.ServiceInfo{{Name: "stripe"}, {Name: "postgres"}}}
	older := api.Sandbox{ID: otherSbID, EnvironmentID: ciID, Status: api.StatusProvisioning, CreatedAt: at(now.Add(-time.Hour)),
		ExpiresAt: at(now.Add(2 * time.Hour)), Services: []api.ServiceInfo{{Name: "stripe"}}}
	plane.script(func(p *sandboxPlane) {
		p.lists[ciID] = func() (int, any) { return 200, []api.Sandbox{older, ours} }
		p.lists[devID] = func() (int, any) { return 403, map[string]string{"detail": "forbidden"} }
	})

	t.Run("--all fans out and reports the environment it could not list", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "list", "--all")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"! could not list sandboxes of checkout-svc ("+shortID(devID)+"): [403] forbidden\n",
			"  Sandbox    Environment  Status        Expires  Twins\n",
			"● "+shortID(sbID)+"  ci           ready         "+now.Add(time.Hour).Local().Format("15:04")+"    stripe, postgres\n",
			"  "+shortID(otherSbID)+"  ci           provisioning  "+now.Add(2*time.Hour).Local().Format("15:04")+"    stripe\n")
	})

	t.Run("the in-use environment, and --env", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "list")
		if code != 1 || !strings.Contains(stderr, "✗ Failed to list sandboxes of 'dev': [403] forbidden\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "list", "--env", "ci", "--json")
		var list []api.Sandbox
		if code != 0 || json.Unmarshal([]byte(stdout), &list) != nil || len(list) != 2 || list[0].ID != sbID {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		if code, _, stderr = runSandboxCLI(t, "sandbox", "list", "--env", "ci", "--all"); code != 1 || !strings.Contains(stderr, "not both") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("a plane that serves the list-all route is not fanned out", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) { p.listAll = func() (int, any) { return 200, []api.Sandbox{ours} } })
		code, _, stderr := runSandboxCLI(t, "sandbox", "list", "--all")
		if code != 0 || strings.Contains(stderr, "could not list") || !strings.Contains(stderr, "● "+shortID(sbID)) {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("nothing to list", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) { p.listAll = func() (int, any) { return 200, []api.Sandbox{} } })
		code, _, stderr := runSandboxCLI(t, "sandbox", "list", "--all")
		if code != 0 || !strings.Contains(stderr, "No sandboxes\n→ Next: veris up\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestDownDeletesAndForgetsThePointer(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	expires := time.Now().Add(time.Hour)
	arm := func() {
		b.local(cfg.Local{Use: "ci", Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
		plane.script(func(p *sandboxPlane) {
			p.deleted, p.deleteState = nil, 0
			p.answer = func(int) *api.Sandbox { return readySandbox(nil, expires) }
		})
	}

	t.Run("down --yes", func(t *testing.T) {
		arm()
		code, _, stderr := runSandboxCLI(t, "down", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"Delete sandbox "+sbID+" (ci, expires "+expires.Local().Format("15:04")+")? y\n",
			"✓ Sandbox deleted: "+sbID+"\n")
		if got := plane.deletedIDs(); len(got) != 1 || got[0] != ciID+"/"+sbID {
			t.Errorf("deleted %v, want the scoped DELETE", got)
		}
		if ptr := sbPointer(t, b); ptr != nil {
			t.Errorf("pointer %+v, want forgotten", ptr)
		}
	})

	t.Run("off a TTY without --yes nothing is deleted", func(t *testing.T) {
		arm()
		code, _, stderr := runSandboxCLI(t, "down")
		if code != 1 || !strings.Contains(stderr, "--yes") || len(plane.deletedIDs()) != 0 {
			t.Errorf("exit %d, deleted %v:\n%s", code, plane.deletedIDs(), stderr)
		}
		if ptr := sbPointer(t, b); ptr == nil {
			t.Errorf("pointer must survive a refused confirmation")
		}
	})

	t.Run("a 404 on delete is already gone", func(t *testing.T) {
		arm()
		plane.script(func(p *sandboxPlane) { p.deleteState = 404 })
		code, _, stderr := runSandboxCLI(t, "down", "--yes")
		if code != 0 || !strings.Contains(stderr, "! Sandbox "+sbID+" was already gone\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		if ptr := sbPointer(t, b); ptr != nil {
			t.Errorf("pointer %+v, want forgotten", ptr)
		}
	})

	t.Run("a 404 on the read is already gone too", func(t *testing.T) {
		arm()
		plane.script(func(p *sandboxPlane) { p.answer = nil })
		code, _, stderr := runSandboxCLI(t, "sandbox", "delete", "--yes")
		if code != 0 || !strings.Contains(stderr, "! Sandbox "+sbID+" was already gone\n") || len(plane.deletedIDs()) != 0 {
			t.Errorf("exit %d, deleted %v:\n%s", code, plane.deletedIDs(), stderr)
		}
		if ptr := sbPointer(t, b); ptr != nil {
			t.Errorf("pointer %+v, want forgotten", ptr)
		}
	})

	t.Run("deleting another sandbox by --id leaves the pointer", func(t *testing.T) {
		arm()
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox {
				return &api.Sandbox{ID: otherSbID, EnvironmentID: devID, Status: api.StatusReady}
			}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "delete", "--id", otherSbID, "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "Delete sandbox "+otherSbID+" (dev, expires —)? y\n", "✓ Sandbox deleted: "+otherSbID+"\n")
		if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
			t.Errorf("pointer %+v, want untouched", ptr)
		}
	})

	t.Run("down --all deletes every sandbox of the in-use environment", func(t *testing.T) {
		arm()
		plane.script(func(p *sandboxPlane) {
			p.lists[ciID] = func() (int, any) {
				return 200, []api.Sandbox{*readySandbox(nil, expires), {ID: otherSbID, EnvironmentID: ciID, Status: api.StatusReady}}
			}
		})
		code, _, stderr := runSandboxCLI(t, "down", "--all", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"Delete 2 sandboxes of 'ci' ("+shortID(ciID)+")? y\n",
			"✓ Sandbox deleted: "+sbID+"\n",
			"✓ Sandbox deleted: "+otherSbID+"\n")
		if got := plane.deletedIDs(); len(got) != 2 {
			t.Errorf("deleted %v, want both", got)
		}
		if ptr := sbPointer(t, b); ptr != nil {
			t.Errorf("pointer %+v, want forgotten", ptr)
		}
		plane.script(func(p *sandboxPlane) { p.lists[ciID] = func() (int, any) { return 200, []api.Sandbox{} } })
		if code, _, stderr = runSandboxCLI(t, "down", "--all", "--yes"); code != 0 || !strings.Contains(stderr, "No sandboxes of 'ci' to delete\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("no pointer", func(t *testing.T) {
		b.local(cfg.Local{})
		code, _, stderr := runSandboxCLI(t, "down", "--yes")
		if code != 1 || !strings.Contains(stderr, "✗ No sandbox for this folder\n→ Next: veris up\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestSandboxResetOutcomes(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(nil, time.Now().Add(time.Hour)) }
	})
	question := "Reset every service in " + sbID + " to its boot profile and set the clock live? y\n"

	t.Run("409: the world is an image", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) {
			p.reset = func() (int, any) {
				return 409, map[string]string{"detail": "sandbox " + sbID + " boots image reg/ci@sha256:abc; restore it by deleting and recreating the sandbox"}
			}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "reset", "--yes")
		if code != 1 {
			t.Errorf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, question,
			"✗ Failed to reset sandbox: [409] sandbox "+sbID+" boots image reg/ci@sha256:abc; restore it by deleting and recreating the sandbox\n",
			"→ This world came from an image; a fresh copy is one command away:\n",
			"  veris down && veris up\n")
	})

	t.Run("200 with a service that did not reset", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) {
			p.reset = func() (int, any) {
				return 200, map[string]any{"id": sbID, "ok": false, "services": []map[string]any{
					{"name": "stripe", "ok": false, "detail": "seed profile 'default' failed: boom"},
					{"name": "postgres", "ok": true, "detail": map[string]any{"ok": true}},
				}}
			}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "reset", "--yes")
		if code != 1 || !strings.Contains(stderr, "✗ Reset failed for: stripe (seed profile 'default' failed: boom)\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "✓ postgres reset") {
			t.Errorf("a partial reset is not a success:\n%s", stderr)
		}
	})

	t.Run("every service ok", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) {
			p.reset = func() (int, any) {
				return 200, map[string]any{"id": sbID, "ok": true, "services": []map[string]any{
					{"name": "stripe", "ok": true, "detail": map[string]any{"reset": true, "seeded": map[string]int{"customers": 3, "prices": 12}}},
					{"name": "postgres", "ok": true, "detail": map[string]any{"ok": true}},
				}}
			}
		})
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "reset", "--yes", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, question, "✓ stripe reset (2 tables)\n", "✓ postgres reset\n")
		var res api.ResetResponse
		if json.Unmarshal([]byte(stdout), &res) != nil || !res.OK || len(res.Services) != 2 {
			t.Errorf("stdout is not the reset body:\n%s", stdout)
		}
	})

	t.Run("off a TTY without --yes nothing is posted", func(t *testing.T) {
		before := plane.resetCount()
		code, _, stderr := runSandboxCLI(t, "sandbox", "reset")
		if code != 1 || !strings.Contains(stderr, "--yes") || plane.resetCount() != before {
			t.Errorf("exit %d, resets %d→%d:\n%s", code, before, plane.resetCount(), stderr)
		}
	})
}

func TestSandboxCommandsRefuseStrayWords(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	for _, argv := range [][]string{{"status", "x"}, {"down", "dev"}, {"sandbox", "get", "x"}, {"sandbox", "list", "x"}, {"sandbox", "delete", "x"}, {"sandbox", "reset", "x"}} {
		code, _, stderr := runSandboxCLI(t, argv...)
		if code != 1 || !strings.Contains(stderr, "takes no arguments") {
			t.Errorf("%v: exit %d:\n%s", argv, code, stderr)
		}
	}
}
