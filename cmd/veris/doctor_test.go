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

	"github.com/veris-ai/veris-cli/internal/ca"
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

	// Milestone 3: the sandbox's clock and the callback registration the
	// stripe twin holds. clockMode "" is live; clockStatus 0 answers 200.
	// probeURL "" is no registration; probeState is its probe_state and
	// probeDead adds a dead-tunnel signature to the last probe result;
	// clientStatus != 0 fails the read.
	clockMode    string
	clockOffset  int64
	clockStatus  int
	probeURL     string
	probeState   string
	probeDead    bool
	clientStatus int
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
		if strings.HasSuffix(path, "/veris/data") && r.URL.Query().Get("entity_type") == "client" {
			if p.clientStatus != 0 {
				reply(p.clientStatus, map[string]any{"detail": "client table unavailable"})
				return
			}
			rows := []map[string]any{}
			if p.probeURL != "" {
				var result any
				if p.probeDead {
					result = map[string]any{"status": 530, "dead_tunnel_signature": "cloudflare-1033"}
				}
				rows = append(rows, map[string]any{
					"id": 1, "default_base_url": p.probeURL, "base_url_revision": 3,
					"probe_state": p.probeState, "probed_revision": 3, "last_probe_result": result,
				})
			}
			reply(http.StatusOK, map[string]any{"entity_type": "client", "rows": rows, "total": len(rows), "limit": 1, "offset": 0})
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
	case "/v1/environments/" + devID + "/sandboxes/" + sbID + "/clock":
		if p.clockStatus != 0 {
			reply(p.clockStatus, map[string]any{"detail": "clock unavailable"})
			return
		}
		clock := map[string]any{"id": 1, "mode": "live", "offset_seconds": p.clockOffset, "frozen_time": nil}
		if p.clockMode == "frozen" {
			clock["mode"] = "frozen"
			clock["frozen_time"] = int64(1772355600) // 2026-03-01T09:00:00Z
		}
		reply(http.StatusOK, clock)
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

// fakeCloudflared appends a directory holding a cloudflared that does
// nothing to PATH; doctor only looks for it. Call it after fakeDocker,
// which replaces PATH.
func fakeCloudflared(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stands in for cloudflared with a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cloudflared"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+dir)
}

// mintCA lays down the CA a first run would have, under the bench's HOME,
// and returns the certificate's path.
func mintCA(t *testing.T) string {
	t.Helper()
	dir := defaultCADir()
	c, err := ca.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c.CertPath()
}

// doctorBench lays out the happy machine: logged in against plane, a project
// with dev as its default, this folder's sandbox pointer, a docker whose
// daemon answers, cloudflared on PATH and a CA already minted. Tests break
// one thing each.
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
	fakeCloudflared(t)
	mintCA(t)
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
		"✓ cloudflared on PATH (",
		"✓ CA "+filepath.Join(b.home, ".veris", "ca", "veris-ca.pem")+" (key 0600)",
		"✓ Project file "+filepath.Join(b.project, ".veris", "twin.yaml")+" (2 environments, default 'dev')",
		"✓ Environment dev (k3j2v0d8…) reachable; services: stripe, postgres",
		"✓ Sandbox 7hqz4m2n… ready, expires in 2h 0m",
		"✓ Clock live",
		"  ✓ stripe  ok",
		"  ✓ postgres  data plane (handed to the app, not proxied)",
		"✓ No callback URL registered (run --expose PORT registers one)",
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
	want := "binary:ok login:ok plane:ok gateway:ok docker:ok tunnel:ok ca:ok project:ok environment:ok sandbox:ok clock:ok twin:stripe:ok twin:postgres:ok callback:ok"
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
		"! cloudflared not on PATH — --expose (callbacks) needs it in the host tier",
		"→ Next: brew install cloudflared, or see cloudflare.com/products/tunnel",
		"! No CA at "+filepath.Join(b.home, ".veris", "ca")+" yet; the first run mints one",
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

// --- Milestone 3 lines ------------------------------------------------------

// The tunnel prerequisite: cloudflared on PATH is ✓; missing, the line says
// whether --image still delivers callbacks (the runner image bundles it) or
// nothing does.
func TestDoctorM3Tunnel(t *testing.T) {
	doctorBench(t, &doctorPlane{gatewayAvailable: true})
	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "✓ cloudflared on PATH (", "/cloudflared) — --expose can open a callback tunnel")

	// docker up, cloudflared gone: the container tier still has one.
	fakeDocker(t, "#!/bin/sh\necho 27.1.1\n")
	_, stderr, code = runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("a missing cloudflared is a warning; exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "! cloudflared not on PATH — --expose works with --image (the runner image bundles it), not in the host tier")
	doctorRejects(t, stderr, "brew install cloudflared")

	// Neither: the install hint.
	fakeDocker(t, "")
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorWants(t, stderr,
		"! cloudflared not on PATH — --expose (callbacks) needs it in the host tier",
		"→ Next: brew install cloudflared, or see cloudflare.com/products/tunnel")

	// --json carries the same verdicts under "tunnel".
	stdout, _, _ := runDoctor(t, cli.Globals{JSON: true})
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Checks {
		if c.Check == "tunnel" && c.Status != "warn" {
			t.Errorf("tunnel = %+v, want warn", c)
		}
	}
}

// The CA: none yet is !, a key readable by others is ! with the chmod,
// present and 0600 is ✓.
func TestDoctorM3CA(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes")
	}
	b := doctorBench(t, &doctorPlane{gatewayAvailable: true})
	dir := filepath.Join(b.home, ".veris", "ca")
	cert, key := filepath.Join(dir, "veris-ca.pem"), filepath.Join(dir, "veris-ca-key.pem")

	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "✓ CA "+cert+" (key 0600)")

	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code = runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("a loose key is a warning; exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "! CA key "+key+" is 0644, not 0600; it can mint a certificate for any host", "→ Next: chmod 600 "+key)

	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorWants(t, stderr, "! CA "+cert+" has no key beside it; the next run mints a new CA")

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, _ := runDoctor(t, cli.Globals{JSON: true})
	if stderr != "" {
		t.Errorf("--json: stderr %q", stderr)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range report.Checks {
		if c.Check == "ca" {
			found = true
			if c.Status != "warn" || !strings.Contains(c.Message, "No CA at "+dir+" yet; the first run mints one") {
				t.Errorf("ca = %+v", c)
			}
		}
	}
	if !found {
		t.Errorf("no ca check in %s", stdout)
	}
	// doctor changes nothing: no CA was minted by looking for one.
	if _, err := os.Stat(cert); err == nil {
		t.Error("doctor minted a CA")
	}
}

// The sandbox's clock: live is ✓ (with its offset), frozen is ! with the
// verb that unfreezes it, and a clock that will not read is ! too.
func TestDoctorM3Clock(t *testing.T) {
	doctorBench(t, &doctorPlane{gatewayAvailable: true, clockOffset: 7 * 24 * 3600})
	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("exit %d, want 0:\n%s", code, stderr)
	}
	doctorWants(t, stderr, "✓ Clock live (+7d)")

	doctorBench(t, &doctorPlane{gatewayAvailable: true, clockMode: "frozen"})
	_, stderr, code = runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("a frozen clock is a warning; exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"! Clock frozen at 2026-03-01T09:00:00Z; outbound deliveries are paused while it is frozen",
		"→ Next: veris sandbox clock set --live")

	doctorBench(t, &doctorPlane{gatewayAvailable: true, clockStatus: http.StatusBadGateway})
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorWants(t, stderr, "! Clock of sandbox 7hqz4m2n… not read: [502] clock unavailable")

	// The clock is read once the sandbox is up, not while it is still on its
	// way or once it is gone: there the sandbox line is the finding, and a
	// clock route that cannot answer yet would only restate it.
	doctorBench(t, &doctorPlane{gatewayAvailable: true, sandboxStatus: "provisioning", clockStatus: http.StatusBadGateway})
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorWants(t, stderr, "! Sandbox 7hqz4m2n… still provisioning")
	doctorRejects(t, stderr, "Clock live", "Clock frozen", "Clock of sandbox")
	doctorBench(t, &doctorPlane{gatewayAvailable: true, sandboxMissing: true})
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorRejects(t, stderr, "Clock live", "Clock frozen", "Clock of sandbox")
}

// The callback registration, as the stripe twin holds it: none is ✓, one
// whose probe answered is ✓, one the sandbox could not reach is ! (a dead
// tunnel named as such), and a twin that will not answer is ! not read.
func TestDoctorM3Callback(t *testing.T) {
	cases := []struct {
		name  string
		plane doctorPlane
		want  []string
		not   []string
	}{
		{"none", doctorPlane{}, []string{"✓ No callback URL registered (run --expose PORT registers one)"}, nil},
		{"answered", doctorPlane{probeURL: "https://abc.trycloudflare.com", probeState: "answered"},
			[]string{"✓ Callbacks registered at https://abc.trycloudflare.com (probe answered)"}, []string{"→ Next: veris run --expose"}},
		{"unreachable", doctorPlane{probeURL: "https://abc.trycloudflare.com", probeState: "unreachable"},
			[]string{"! Callbacks registered at https://abc.trycloudflare.com, but the sandbox could not reach it (probe_state unreachable)",
				"→ Next: veris run --expose PORT … registers this run's own URL"}, nil},
		{"dead tunnel", doctorPlane{probeURL: "https://old.trycloudflare.com", probeState: "unreachable", probeDead: true},
			[]string{"! Callbacks registered at https://old.trycloudflare.com, but the tunnel behind it is gone (probe_state unreachable); an earlier run left it"}, nil},
		{"not read", doctorPlane{clientStatus: http.StatusBadGateway},
			[]string{"! Callback registration not read: [502] client table unavailable"}, nil},
		{"not while provisioning", doctorPlane{sandboxStatus: "provisioning", probeURL: "https://abc.trycloudflare.com", probeState: "unreachable"},
			nil, []string{"Callbacks registered", "Callback registration", "No callback URL"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plane := tc.plane
			plane.gatewayAvailable = true
			doctorBench(t, &plane)
			_, stderr, code := runDoctor(t, cli.Globals{})
			if code != 0 {
				t.Errorf("the callback line never fails a run; exit %d:\n%s", code, stderr)
			}
			doctorWants(t, stderr, tc.want...)
			doctorRejects(t, stderr, tc.not...)
		})
	}

	// --json carries the registration itself.
	doctorBench(t, &doctorPlane{gatewayAvailable: true, probeURL: "https://abc.trycloudflare.com", probeState: "answered"})
	stdout, _, _ := runDoctor(t, cli.Globals{JSON: true})
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	last := report.Checks[len(report.Checks)-1]
	detail, _ := last.Detail.(map[string]any)
	if last.Check != "callback" || last.Status != "ok" || detail["url"] != "https://abc.trycloudflare.com" || detail["probe_state"] != "answered" {
		t.Errorf("last check = %+v", last)
	}
}

// A shell key beside a profile with a login of its own: the shell's key is
// what is sent, to the profile's plane, and the line says so; a refused
// shell key is the shell's problem, in the words every command uses; and a
// shell that names the plane as well has said what it means.
func TestDoctorM3ShellKey(t *testing.T) {
	plane := &doctorPlane{gatewayAvailable: true}
	b := doctorBench(t, plane)
	url := b.planeURL(t)
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: "vsk_profilekey0000"},
	}})
	t.Setenv(cfg.EnvAPIKey, doctorKey)

	_, stderr, code := runDoctor(t, cli.Globals{})
	if code != 0 {
		t.Errorf("an accepted shell key is a warning; exit %d:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"✓ Logged in: Acme via env VERIS_API_KEY (vsk_testkey0…)",
		"! VERIS_API_KEY from your shell (vsk_testkey0…) is sent to "+url+" instead of profile 'default''s own key (vsk_profilek…)",
		"→ Next: unset VERIS_API_KEY to use the profile, or export VERIS_API_BASE for the plane the key belongs to",
	)
	doctorRejects(t, stderr, doctorKey, "vsk_profilekey0000")

	// The shell also names the plane: coherent, nothing to say.
	t.Setenv(cfg.EnvAPIBase, url)
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorRejects(t, stderr, "instead of profile")
	t.Setenv(cfg.EnvAPIBase, "")

	// The same key in both places is not an override.
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: doctorKey},
	}})
	_, stderr, _ = runDoctor(t, cli.Globals{})
	doctorRejects(t, stderr, "instead of profile")

	// A refused shell key: the shell's fault, not the login's.
	t.Setenv(cfg.EnvAPIKey, "vsk_wrongplane00000")
	_, stderr, code = runDoctor(t, cli.Globals{})
	if code != 1 {
		t.Errorf("exit %d, want 1:\n%s", code, stderr)
	}
	doctorWants(t, stderr,
		"✗ VERIS_API_KEY from your shell was rejected by "+url+": [401] invalid or missing API key",
		"→ Next: unset VERIS_API_KEY to use profile 'default', or export a key for "+url,
	)
	// One problem, one remedy: the shell-key warning does not restate it.
	doctorRejects(t, stderr, "Not logged in", "vsk_wrongplane00000", "instead of profile")
	if n := strings.Count(stderr, "VERIS_API_KEY from your shell"); n != 1 {
		t.Errorf("the refused shell key is blamed once, got %d:\n%s", n, stderr)
	}

	// --json names the check.
	t.Setenv(cfg.EnvAPIKey, doctorKey)
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: "vsk_profilekey0000"},
	}})
	stdout, _, _ := runDoctor(t, cli.Globals{JSON: true})
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Checks) < 3 || report.Checks[2].Check != "shell_key" || report.Checks[2].Status != "warn" {
		t.Errorf("checks = %+v", report.Checks)
	}
	if strings.Contains(stdout, doctorKey) || strings.Contains(stdout, "vsk_profilekey0000") {
		t.Errorf("keys must be masked:\n%s", stdout)
	}
}

// planeURL is the api base the bench's default profile points at.
func (b *bench) planeURL(t *testing.T) string {
	t.Helper()
	g, err := cfg.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	return g.Profiles["default"].APIBase
}

// --- version ----------------------------------------------------------------

// version --json: the binary's version, os and arch, and the plane as null
// when none answered -- valid JSON on stdout alone, in every case.
func TestVersionJSON(t *testing.T) {
	b := newBench(t)
	stdout, stderr := runVersion(t, cli.Globals{JSON: true})
	if stderr != "" {
		t.Errorf("--json: stderr %q", stderr)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("--json: %v\n%s", err, stdout)
	}
	if body["version"] != version || body["os"] != runtime.GOOS || body["arch"] != runtime.GOARCH {
		t.Errorf("body = %v", body)
	}
	if plane, ok := body["control_plane"]; !ok || plane != nil {
		t.Errorf("control_plane = %v, want null on a fresh machine", plane)
	}

	url := (&doctorPlane{}).serve(t)
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: url, APIKey: doctorKey},
	}})
	stdout, stderr = runVersion(t, cli.Globals{JSON: true, Quiet: true})
	if stderr != "" {
		t.Errorf("--json -q: stderr %q", stderr)
	}
	var report versionReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("--json: %v\n%s", err, stdout)
	}
	if report.OS != runtime.GOOS || report.ControlPlane == nil || report.ControlPlane.APIBase != url || report.ControlPlane.Checkout != "3f9a1b2c" {
		t.Errorf("report = %+v (plane %+v)", report, report.ControlPlane)
	}
	if strings.Contains(stdout, doctorKey) {
		t.Errorf("the key has no place in version:\n%s", stdout)
	}

	// Through the tree, so --json is read from the globals as a script
	// passes it.
	var out, errOut bytes.Buffer
	if err := cli.Execute(root(), &cli.Globals{}, []string{"version", "--json"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &body); err != nil || errOut.Len() != 0 {
		t.Errorf("version --json: %v, stderr %q\n%s", err, errOut.String(), out.String())
	}
}

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
