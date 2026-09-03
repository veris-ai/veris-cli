package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
)

const doctorKey = "vsk_testkey0000000"

// doctorPlane is a control plane with one organisation, the bench's dev
// environment and its sandbox, plus that sandbox's stripe twin under
// /s/<id>/stripe. Every knob is fixed at construction, so the handler and
// the test never write the same field.
type doctorPlane struct {
	gatewayAvailable bool
	gatewayStatus    int // 0 answers 200
	envMissing       bool
	sandboxMissing   bool
	sandboxStatus    string        // "" is ready
	sandboxExpiresIn time.Duration // 0 is two hours
	failureReason    string
	twinStatus       int // 0 answers ok
}

func (p *doctorPlane) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (p *doctorPlane) handle(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	path := r.URL.Path
	switch {
	case path == "/healthz":
		reply(http.StatusOK, map[string]any{"status": "ok", "checkout": "3f9a1b2c"})
		return
	case strings.HasPrefix(path, "/s/"):
		if strings.HasSuffix(path, "/veris/health") {
			if p.twinStatus != 0 {
				reply(p.twinStatus, map[string]any{"detail": "upstream unavailable"})
				return
			}
			reply(http.StatusOK, map[string]any{"status": "ok", "service": "stripe"})
			return
		}
		reply(http.StatusNotFound, map[string]any{"detail": "Not Found"})
		return
	}
	if r.Header.Get("X-API-Key") != doctorKey {
		reply(http.StatusUnauthorized, map[string]any{"detail": "invalid or missing API key"})
		return
	}
	switch path {
	case "/v1/me":
		reply(http.StatusOK, map[string]any{
			"kind": "api_key", "user": nil, "organization_id": "org1",
			"organizations": []map[string]any{{"id": "org1", "name": "Acme", "slug": "acme", "kind": "team"}},
		})
	case "/v1/gateway/health":
		if p.gatewayStatus != 0 {
			reply(p.gatewayStatus, map[string]any{"detail": "Not Found"})
			return
		}
		canary := ""
		if p.gatewayAvailable {
			canary = "gw.api.veris.ai"
		}
		reply(http.StatusOK, map[string]any{"available": p.gatewayAvailable, "canary_host": canary})
	case "/v1/environments/" + devID:
		if p.envMissing {
			reply(http.StatusNotFound, map[string]any{"detail": "environment " + devID + " not found"})
			return
		}
		reply(http.StatusOK, map[string]any{
			"id": devID, "name": "checkout-svc", "services": []string{"stripe", "postgres"},
			"created_at": "2026-03-01T09:00:00Z", "owner": "org1", "baseline": nil,
		})
	case "/v1/sandboxes/" + sbID:
		if p.sandboxMissing {
			reply(http.StatusNotFound, map[string]any{"detail": "sandbox " + sbID + " not found"})
			return
		}
		status := p.sandboxStatus
		if status == "" {
			status = "ready"
		}
		expires := p.sandboxExpiresIn
		if expires == 0 {
			expires = 2 * time.Hour
		}
		base := "http://" + r.Host
		reply(http.StatusOK, map[string]any{
			"id": sbID, "environment_id": devID, "status": status,
			"created_at":     time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
			"expires_at":     time.Now().UTC().Add(expires).Format(time.RFC3339),
			"failure_reason": p.failureReason,
			"services": []map[string]any{
				{"name": "stripe", "status": "ready", "url": base + "/s/" + sbID + "/stripe",
					"control_url": base + "/s/" + sbID + "/stripe", "env_hint": "STRIPE_API_BASE"},
				{"name": "postgres", "status": "ready", "url": "postgres://x@db/app", "env_hint": "DATABASE_URL"},
			},
		})
	default:
		reply(http.StatusNotFound, map[string]any{"detail": "Not Found"})
	}
}

// fakeDocker puts a docker of the given script on PATH, and nothing else
// there; an empty script leaves PATH with no docker at all.
func fakeDocker(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stands in for docker with a shell script")
	}
	dir := t.TempDir()
	if script != "" {
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// doctorBench lays out the happy machine: logged in against plane, a project
// with dev as its default, this folder's sandbox pointer, and a docker whose
// daemon answers. Tests break one thing each.
func doctorBench(t *testing.T, plane *doctorPlane) *bench {
	t.Helper()
	b := newBench(t)
	url := plane.serve(t)
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: doctorKey, ConsoleURL: "https://studio.example"},
	}})
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: devID}})
	fakeDocker(t, "#!/bin/sh\necho 27.1.1\n")
	return b
}

func runDoctor(t *testing.T, g cli.Globals) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	ctx := &cli.Context{Globals: &g, Stdout: &out, Stderr: &errOut, Path: []string{"veris", "doctor"}}
	err := cmdDoctor(ctx, nil)
	return out.String(), errOut.String(), exitStatusTo(&errOut, err)
}

func doctorWants(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output lacks %q:\n%s", w, got)
		}
	}
}

func doctorRejects(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(got, u) {
			t.Errorf("output should not carry %q:\n%s", u, got)
		}
	}
}

func TestDoctorHappyPath(t *testing.T) {
	plane := &doctorPlane{gatewayAvailable: true}
	b := doctorBench(t, plane)
	stdout, stderr, code := runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout is the machine's; got %q", stdout)
	}
	doctorWants(t, stderr,
		"✓ veris "+version+" ("+runtime.GOOS+"/"+runtime.GOARCH+")",
		"✓ Logged in: Acme via profile 'default' (vsk_testkey0…)",
		"✓ Control plane http://127.0.0.1:",
		" reachable (status ok, checkout 3f9a1b2c)",
		"✓ Gateway mode configured (canary gw.api.veris.ai)",
		"✓ docker on PATH, daemon answers (server 27.1.1)",
		"✓ Project file "+filepath.Join(b.project, ".veris", "twin.yaml")+" (2 environments, default 'dev')",
		"✓ Environment dev (k3j2v0d8…) reachable; services: stripe, postgres",
		"✓ Sandbox 7hqz4m2n… ready, expires in 2h 0m",
		"  ✓ stripe  ok",
		"  ✓ postgres  data plane (handed to the app, not proxied)",
	)
	doctorRejects(t, stderr, "✗", "!", doctorKey)
}

func TestDoctorJSONCarriesEveryCheck(t *testing.T) {
	doctorBench(t, &doctorPlane{gatewayAvailable: true})
	stdout, stderr, code := runDoctor(t, cli.Globals{JSON: true})
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("--json puts the checks on stdout alone; stderr got %q", stderr)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not the report: %v\n%s", err, stdout)
	}
	if !report.OK {
		t.Errorf("ok = false in %s", stdout)
	}
	var names []string
	for _, c := range report.Checks {
		names = append(names, c.Check+":"+c.Status)
	}
	want := "binary:ok login:ok plane:ok gateway:ok docker:ok project:ok environment:ok sandbox:ok twin:stripe:ok twin:postgres:ok"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("checks = %q\nwant     %q", got, want)
	}
	if strings.Contains(stdout, doctorKey) {
		t.Errorf("the key must be masked in --json too:\n%s", stdout)
	}
}

func TestDoctorNotLoggedIn(t *testing.T) {
	b := newBench(t)
	url := (&doctorPlane{}).serve(t)
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{"default": {APIBase: url}}})
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID}})
	fakeDocker(t, "#!/bin/sh\necho 27.1.1\n")

	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"✗ Not logged in for profile 'default' (no API key)",
		"→ Next: veris login --profile default",
		"✓ Control plane http://127.0.0.1:",
		"! Environment dev (k3j2v0d8…) not checked: not logged in",
		"! Sandbox 7hqz4m2n… not checked: not logged in",
	)
	doctorRejects(t, stderr, "Gateway")

	// A key the plane refuses is the same finding with the plane's reason.
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: "vsk_revokedkey00000"},
	}})
	_, stderr, code = runDoctor(t, cli.Globals{})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"✗ Not logged in for profile 'default': [401] invalid or missing API key",
		"→ Next: veris login --profile default",
	)
}

func TestDoctorPlaneDown(t *testing.T) {
	b := newBench(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: doctorKey},
	}})
	fakeDocker(t, "")

	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"✗ Login not verified: "+url+" answered",
		"✗ Control plane "+url+" unreachable:",
		"! docker not on PATH — host tier works; --image (container tier) will not",
		"! No .veris/twin.yaml found (searched up from "+b.project+")",
		"→ Next: veris env create",
		"! No sandbox for this folder",
		"→ Next: veris up",
	)
	doctorRejects(t, stderr, "Gateway", "Environment")
}

func TestDoctorDockerDaemonDown(t *testing.T) {
	doctorBench(t, &doctorPlane{gatewayAvailable: true})
	fakeDocker(t, "#!/bin/sh\necho 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2\nexit 1\n")
	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("a daemon that is down is a warning, exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"! docker on PATH but `docker info` failed: Cannot connect to the Docker daemon at unix:///var/run/docker.sock — --image (container tier) will not work until the daemon answers",
	)
}

func TestDoctorGatewayNotConfigured(t *testing.T) {
	doctorBench(t, &doctorPlane{})
	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "! Gateway mode not configured on http://127.0.0.1:", "; runs use the proxy tier")
	doctorRejects(t, stderr, "Gateway mode up", "Gateway up")

	doctorBench(t, &doctorPlane{gatewayStatus: http.StatusNotFound})
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorWants(t, stderr, "! Gateway health is not served by http://127.0.0.1:", " (an older control plane)")
}

func TestDoctorEnvironmentFindings(t *testing.T) {
	b := doctorBench(t, &doctorPlane{envMissing: true, gatewayAvailable: true})
	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 1 {
		t.Errorf("a missing environment fails every run; exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"✗ Environment dev (k3j2v0d8…) unreachable: [404] environment "+devID+" not found",
		"→ Next: veris env list",
	)

	// Drift the plane can see: a required service the environment does not
	// run, and a baseline boot with nothing pinned.
	b = doctorBench(t, &doctorPlane{gatewayAvailable: true})
	b.projectFile(cfg.Project{Project: "proj", Default: "dev", Environments: map[string]cfg.EnvConfig{
		"dev": {ID: devID, Boot: "baseline", Proxy: cfg.ProxyConfig{RequireService: []string{"stripe", "github:2"}}},
	}})
	_, stderr, code = runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("drift is a warning; exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"✓ Environment dev (k3j2v0d8…) reachable; services: stripe, postgres",
		"! proxy.require_service names github, which 'dev' does not run (has: stripe, postgres)",
		"! 'dev' boots its baseline, but the environment has none pinned; up boots the bundle",
	)
	doctorRejects(t, stderr, "names stripe")

	// A project with environments and nothing chosen says how to choose.
	b.projectFile(cfg.Project{Project: "proj", Environments: map[string]cfg.EnvConfig{"dev": {ID: devID}}})
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorWants(t, stderr, "! No environment selected", "→ Next: veris env use NAME, or pass --env")

	// A name the file does not know is the refusal every command prints,
	// not a 404 from the plane.
	b.twoEnvs()
	t.Setenv(cfg.EnvEnv, "stagin")
	_, stderr, code = runDoctor(t, cli.Globals{})
	if code != 1 {
		t.Errorf("a typo in VERIS_ENV fails; exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "✗ No environment 'stagin' in "+filepath.Join(b.project, ".veris", "twin.yaml")+" (have: ci, dev)", "→ Next: veris env list")
	doctorRejects(t, stderr, "unreachable")
	t.Setenv(cfg.EnvEnv, "")

	// --env, which the hint above recommends, picks the environment checked.
	var out, errOut bytes.Buffer
	err := cli.Execute(root(), &cli.Globals{}, []string{"doctor", "--env", "ci"}, &out, &errOut)
	if exitStatusTo(&errOut, err) != 1 {
		t.Errorf("ci is unknown to this plane; exit %d:\n%s", exitStatusTo(&errOut, err), errOut.String())
	}
	doctorWants(t, errOut.String(), "✗ Environment ci (c1a2b3d4…) unreachable: [404]")
}

func TestDoctorSandboxStates(t *testing.T) {
	cases := []struct {
		name  string
		plane doctorPlane
		code  int
		want  []string
	}{
		{"failed", doctorPlane{sandboxStatus: "failed", failureReason: "image pull failed"}, 1,
			[]string{"✗ Sandbox 7hqz4m2n… failed: image pull failed", "→ Next: veris down && veris up"}},
		{"provisioning", doctorPlane{sandboxStatus: "provisioning"}, 0,
			[]string{"! Sandbox 7hqz4m2n… still provisioning, expires in 2h 0m", "→ Next: veris status"}},
		{"gone", doctorPlane{sandboxMissing: true}, 1,
			[]string{"✗ Sandbox 7hqz4m2n… is gone: [404] sandbox " + sbID + " not found", "→ Next: veris up"}},
		{"expired", doctorPlane{sandboxExpiresIn: -5 * time.Minute}, 1,
			[]string{"✗ Sandbox 7hqz4m2n… expired 5m ago", "→ Next: veris up"}},
		{"twin down", doctorPlane{twinStatus: http.StatusBadGateway}, 1,
			[]string{"✓ Sandbox 7hqz4m2n… ready, expires in 2h 0m", "✗ stripe  health: [502] upstream unavailable", "→ Next: veris status", "  ✓ postgres  data plane"}},
		{"twin quiet while provisioning", doctorPlane{sandboxStatus: "provisioning", twinStatus: http.StatusBadGateway}, 0,
			[]string{"! Sandbox 7hqz4m2n… still provisioning", "! stripe  health: [502] upstream unavailable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plane := tc.plane
			plane.gatewayAvailable = true
			doctorBench(t, &plane)
			_, stderr, code := runDoctor(t, cli.Globals{})
			if code != tc.code {
				t.Errorf("exit %d, want %d:\n%s", code, tc.code, stderr)
			}
			doctorWants(t, stderr, tc.want...)
		})
	}
}

func TestDoctorQuietKeepsTheFindings(t *testing.T) {
	doctorBench(t, &doctorPlane{sandboxStatus: "failed", failureReason: "oom"})
	_, stderr, code := runDoctor(t, cli.Globals{Quiet: true})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "✗ Sandbox 7hqz4m2n… failed: oom", "! Gateway mode not configured")
	doctorRejects(t, stderr, "✓")
}

// --- version ----------------------------------------------------------------

func runVersion(t *testing.T, g cli.Globals) (stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	ctx := &cli.Context{Globals: &g, Stdout: &out, Stderr: &errOut, Path: []string{"veris", "version"}}
	if err := cmdVersion(ctx, nil); err != nil {
		t.Fatalf("version must never fail, got %v", err)
	}
	return out.String(), errOut.String()
}

func TestVersionOffline(t *testing.T) {
	// A fresh machine: nothing names a plane, so nothing is asked.
	b := newBench(t)
	stdout, stderr := runVersion(t, cli.Globals{})
	if stdout != version+"\n" || stderr != "" {
		t.Errorf("fresh machine: stdout %q stderr %q", stdout, stderr)
	}

	// A plane that does not answer leaves the line alone, and says nothing.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: doctorKey},
	}})
	stdout, stderr = runVersion(t, cli.Globals{})
	if stdout != version+"\n" || stderr != "" {
		t.Errorf("dead plane: stdout %q stderr %q", stdout, stderr)
	}

	// So does a profiles file that will not parse.
	if err := os.WriteFile(cfg.GlobalPath(), []byte("profiles: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr = runVersion(t, cli.Globals{})
	if stdout != version+"\n" || stderr != "" {
		t.Errorf("broken profiles file: stdout %q stderr %q", stdout, stderr)
	}
}

func TestVersionOnline(t *testing.T) {
	b := newBench(t)
	url := (&doctorPlane{}).serve(t)
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: doctorKey},
	}})
	stdout, stderr := runVersion(t, cli.Globals{})
	want := version + "\ncontrol plane " + url + " · ok · checkout 3f9a1b2c\n"
	if stdout != want || stderr != "" {
		t.Errorf("stdout %q stderr %q, want stdout %q", stdout, stderr, want)
	}

	stdout, _ = runVersion(t, cli.Globals{Quiet: true})
	if stdout != version+"\n" {
		t.Errorf("-q: stdout %q", stdout)
	}

	stdout, _ = runVersion(t, cli.Globals{JSON: true})
	var report versionReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("--json: %v\n%s", err, stdout)
	}
	if report.Version != version || report.ControlPlane == nil ||
		report.ControlPlane.APIBase != url || report.ControlPlane.Status != "ok" || report.ControlPlane.Checkout != "3f9a1b2c" {
		t.Errorf("--json: %+v (plane %+v)", report, report.ControlPlane)
	}

	// A plane named without a key is still asked: /healthz needs none.
	if err := os.Remove(cfg.GlobalPath()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(cfg.EnvAPIBase, url)
	stdout, _ = runVersion(t, cli.Globals{})
	if stdout != want {
		t.Errorf("VERIS_API_BASE alone: stdout %q, want %q", stdout, want)
	}
}
