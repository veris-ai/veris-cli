package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/tunnel"
	"github.com/veris-ai/veris-cli/internal/twin"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// doctor is the one screen that answers "why is my first run failing". Each
// check is one line -- ✓ passed, ! worth knowing, ✗ will fail a run -- and
// they are ordered the way a run depends on them: the login (and a shell
// key overriding it), the plane it talks to, the gateway that plane offers,
// docker for the container tier, cloudflared for callbacks, the CA the
// proxy mints with, then this folder's project file, environment and
// sandbox with its clock and callback registration. Nothing here changes
// anything; the → lines say what would.

// doctorCallTimeout bounds every request doctor makes. A plane that takes
// longer than this to answer a health check is the finding.
const doctorCallTimeout = 5 * time.Second

// doctorCommand is the one screen that answers "why is my first run failing".
func doctorCommand() *cli.Command {
	var env string
	return &cli.Command{
		Name:    "doctor",
		Summary: "Check login, plane, docker, tunnel, CA, project, environment and sandbox",
		Usage:   "veris doctor [--env NAME] [--json]",
		Help: `Every check is one line: ✓ passed, ! worth knowing, ✗ will fail a run.
Nothing is changed; the → lines name the command that would. Exits 1
when any check failed, and --json puts the same checks on stdout. --env
checks a particular environment rather than the one this folder uses.
Besides the login, plane and project: docker for --image, cloudflared for
--expose, the CA under ~/.veris/ca, and for a sandbox that is up its
clock (frozen pauses deliveries) and the callback URL registered on it.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&env, "env", "", "environment `name` to check instead of the one in use")
		},
		Run: func(ctx *cli.Context, args []string) error {
			return doctorWith(ctx, args, env)
		},
	}
}

// doctorCheck is one line of the report, as --json carries it.
type doctorCheck struct {
	Check   string `json:"check"`
	Status  string `json:"status"` // ok, warn or fail
	Message string `json:"message"`
	Next    string `json:"next,omitempty"`
	Detail  any    `json:"detail,omitempty"`
}

// doctorReport is --json's body: whether a run may work, and every check.
type doctorReport struct {
	OK     bool          `json:"ok"`
	Checks []doctorCheck `json:"checks"`
}

// doctor runs the checks and keeps what each found, for the exit status and
// for --json. ui is the session's, or one that writes nowhere under --json:
// the checks are the answer there, and the answer is on stdout.
type doctor struct {
	s      *session
	ui     *ui.UI
	checks []doctorCheck
	// shellKeyBlamed is set once the login line has laid a refused key at
	// the shell's door, so shellKey says nothing more about the same key.
	shellKeyBlamed bool
}

func cmdDoctor(ctx *cli.Context, args []string) error {
	return doctorWith(ctx, args, "")
}

// doctorWith is doctor for one environment, "" for the one in use.
func doctorWith(ctx *cli.Context, args []string, env string) error {
	if len(args) > 0 {
		return &cli.UsageError{Msg: "doctor takes no arguments"}
	}
	s, err := newSession(ctx, env, "")
	if err != nil {
		return err
	}
	d := &doctor{s: s, ui: s.ui}
	asJSON := ctx.Globals != nil && ctx.Globals.JSON
	if asJSON {
		d.ui = ui.New(io.Discard, stdin)
		d.ui.Quiet, d.ui.AssumeYes = s.ui.Quiet, s.ui.AssumeYes
	}

	d.binary()
	loggedIn := d.login()
	d.shellKey()
	planeUp := d.plane()
	if loggedIn && planeUp {
		d.gateway()
	}
	dockerUp := d.docker()
	d.tunnel(dockerUp)
	d.ca()
	d.project()
	d.environment(loggedIn)
	d.sandbox(loggedIn)

	failed := false
	for _, c := range d.checks {
		failed = failed || c.Status == "fail"
	}
	if asJSON {
		if err := printJSON(ctx.Stdout, doctorReport{OK: !failed, Checks: d.checks}); err != nil {
			return err
		}
	}
	if failed {
		return printed(1)
	}
	return nil
}

// record keeps one check; the ok/warn/fail helpers print it as well.
func (d *doctor) record(status, check, msg string, detail any) *doctorCheck {
	d.checks = append(d.checks, doctorCheck{Check: check, Status: status, Message: msg, Detail: detail})
	return &d.checks[len(d.checks)-1]
}

func (d *doctor) ok(check string, detail any, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	d.record("ok", check, msg, detail)
	d.ui.Success("%s", msg)
}

func (d *doctor) warn(check string, detail any, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	d.record("warn", check, msg, detail)
	d.ui.Warn("%s", msg)
}

func (d *doctor) fail(check string, detail any, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	d.record("fail", check, msg, detail)
	d.ui.Fail("%s", msg)
}

// next names the command that answers the check just recorded.
func (d *doctor) next(cmd string) {
	d.checks[len(d.checks)-1].Next = cmd
	d.ui.Next(cmd)
}

// call runs one request under doctorCallTimeout.
func (d *doctor) call(f func(ctx context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), doctorCallTimeout)
	defer cancel()
	return f(ctx)
}

// binary names what is running, since "which version" is the first question
// of any report this screen ends up pasted into.
func (d *doctor) binary() {
	d.ok("binary", map[string]any{"version": d.s.ver, "os": runtime.GOOS, "arch": runtime.GOARCH},
		"veris %s (%s/%s)", d.s.ver, runtime.GOOS, runtime.GOARCH)
}

// login is GET /v1/me with the resolved key. No key, or a refused one, prints
// the same not-logged-in lines every other command would, so the fix reads
// the same wherever it is met.
func (d *doctor) login() bool {
	res := d.s.res
	profile := res.ProfileName
	if res.APIKey == "" {
		_ = notLoggedIn(d.ui, profile, "")
		c := d.record("fail", "login", fmt.Sprintf("not logged in for profile '%s' (no API key)", profile), nil)
		c.Next = "veris login --profile " + profile
		return false
	}
	source := fmt.Sprintf("profile '%s'", profile)
	if res.APIKeySource == cfg.SourceEnv {
		source = "env " + cfg.EnvAPIKey
	} else if res.APIKeySource == cfg.SourceFlag {
		source = "--api-key"
	}
	var me *api.Me
	err := d.call(func(ctx context.Context) error {
		var err error
		me, err = d.s.plane().Me(ctx)
		return err
	})
	if err != nil {
		if api.IsStatus(err, http.StatusUnauthorized) {
			// A refused shell key is not a login problem, and (*session).fail
			// says so in the same words: the profile may be fine, the shell is
			// pointing every command at another plane's key.
			if res.APIKeySource == cfg.SourceEnv {
				d.fail("login", map[string]any{"key_source": string(res.APIKeySource), "key": ui.MaskKey(res.APIKey)},
					"%s from your shell was rejected by %s: %v", cfg.EnvAPIKey, res.APIBase, err)
				d.next(fmt.Sprintf("unset %s to use profile '%s', or export a key for %s", cfg.EnvAPIKey, profile, res.APIBase))
				d.shellKeyBlamed = true
				return false
			}
			_ = notLoggedIn(d.ui, profile, err.Error())
			c := d.record("fail", "login", fmt.Sprintf("not logged in for profile '%s': %v", profile, err), nil)
			c.Next = "veris login --profile " + profile
			return false
		}
		d.fail("login", nil, "Login not verified: %s answered %v", res.APIBase, err)
		return false
	}
	org := me.OrganizationID
	for _, o := range me.Organizations {
		if o.ID == me.OrganizationID && o.Name != "" {
			org = o.Name
		}
	}
	d.ok("login", map[string]any{
		"profile": profile, "key_source": string(res.APIKeySource), "key": ui.MaskKey(res.APIKey),
		"organization_id": me.OrganizationID, "organization": org, "kind": me.Kind,
	}, "Logged in: %s via %s (%s)", org, source, ui.MaskKey(res.APIKey))
	return true
}

// shellKey is the warning for a VERIS_API_KEY that overrides a profile with
// a login of its own. The shell's key wins the precedence, and unless the
// shell also names the plane (VERIS_API_BASE or --api-base) it is sent to
// the profile's plane -- where a key minted for another one is refused, or
// worse, accepted by a plane the user did not mean. Nothing to say when the
// profile has no key, when the two are the same key, or when the shell
// chose the plane as well.
func (d *doctor) shellKey() {
	res := d.s.res
	if d.shellKeyBlamed || res.APIKeySource != cfg.SourceEnv || res.Global == nil {
		return
	}
	if res.APIBaseSource == cfg.SourceEnv || res.APIBaseSource == cfg.SourceFlag {
		return
	}
	p, ok := res.Global.Profiles[res.ProfileName]
	if !ok || p.APIKey == "" || p.APIKey == res.APIKey {
		return
	}
	d.warn("shell_key", map[string]any{
		"profile": res.ProfileName, "api_base": res.APIBase,
		"shell_key": ui.MaskKey(res.APIKey), "profile_key": ui.MaskKey(p.APIKey),
	}, "%s from your shell (%s) is sent to %s instead of profile '%s''s own key (%s)",
		cfg.EnvAPIKey, ui.MaskKey(res.APIKey), res.APIBase, res.ProfileName, ui.MaskKey(p.APIKey))
	d.next(fmt.Sprintf("unset %s to use the profile, or export %s for the plane the key belongs to", cfg.EnvAPIKey, cfg.EnvAPIBase))
}

// plane is GET /healthz, which needs no key: it tells "the plane is down"
// from "the key is wrong" when both lines above and below are red.
func (d *doctor) plane() bool {
	base := d.s.res.APIBase
	var h *api.Healthz
	err := d.call(func(ctx context.Context) error {
		var err error
		h, err = d.s.plane().Healthz(ctx)
		return err
	})
	if err != nil {
		d.fail("plane", map[string]any{"api_base": base}, "Control plane %s unreachable: %v", base, err)
		return false
	}
	fields := "status " + h.Status
	if h.Checkout != "" {
		fields += ", checkout " + h.Checkout
	}
	d.ok("plane", map[string]any{"api_base": base, "status": h.Status, "checkout": h.Checkout},
		"Control plane %s reachable (%s)", base, fields)
	return true
}

// gateway is GET /v1/gateway/health. It says whether the deployment is
// configured for gateway mode, never whether the gateway is up: nothing here
// connects to it. Not configured is a fact about the plane, not a failure,
// since every run through the proxy tier works without it.
func (d *doctor) gateway() {
	base := d.s.res.APIBase
	var g *api.GatewayHealth
	err := d.call(func(ctx context.Context) error {
		var err error
		g, err = d.s.plane().GatewayHealth(ctx)
		return err
	})
	switch {
	case api.IsStatus(err, http.StatusNotFound):
		d.warn("gateway", nil, "Gateway health is not served by %s (an older control plane); gateway-mode runs are unavailable", base)
	case err != nil:
		d.warn("gateway", nil, "Gateway health check failed: %v", err)
	case g.Available:
		d.ok("gateway", map[string]any{"available": true, "canary_host": g.CanaryHost},
			"Gateway mode configured (canary %s)", g.CanaryHost)
	default:
		d.warn("gateway", map[string]any{"available": false},
			"Gateway mode not configured on %s; runs use the proxy tier", base)
	}
}

// docker is `docker info` with a deadline. Missing docker is ! rather than
// ✗: the host tier runs a local child with no docker at all, and only
// --image needs it. It reports whether the container tier is usable, which
// the tunnel line reads.
func (d *doctor) docker() bool {
	path, err := exec.LookPath("docker")
	if err != nil {
		d.warn("docker", nil, "docker not on PATH — host tier works; --image (container tier) will not")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorCallTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		d.warn("docker", map[string]any{"path": path},
			"docker on PATH but `docker info` did not answer within %s — --image (container tier) will wait on it", doctorCallTimeout)
		return false
	case err != nil:
		d.warn("docker", map[string]any{"path": path},
			"docker on PATH but `docker info` failed: %s — --image (container tier) will not work until the daemon answers", firstLine(out))
		return false
	default:
		server := strings.TrimSpace(string(out))
		detail := map[string]any{"path": path, "server_version": server}
		if server == "" {
			d.ok("docker", detail, "docker on PATH, daemon answers")
			return true
		}
		d.ok("docker", detail, "docker on PATH, daemon answers (server %s)", server)
		return true
	}
}

// tunnel is whether cloudflared, which --expose opens the callback path
// with, can be found. Missing is !: every run without --expose works, and
// the runner image bundles its own, so with docker up the container tier
// still delivers callbacks.
func (d *doctor) tunnel(dockerUp bool) {
	path, err := exec.LookPath(tunnel.DefaultBinary)
	if err == nil {
		d.ok("tunnel", map[string]any{"path": path}, "%s on PATH (%s) — --expose can open a callback tunnel", tunnel.DefaultBinary, path)
		return
	}
	if dockerUp {
		d.warn("tunnel", map[string]any{"bundled": true},
			"%s not on PATH — --expose works with --image (the runner image bundles it), not in the host tier", tunnel.DefaultBinary)
		return
	}
	d.warn("tunnel", nil, "%s not on PATH — --expose (callbacks) needs it in the host tier", tunnel.DefaultBinary)
	d.next("brew install cloudflared, or see cloudflare.com/products/tunnel")
}

// ca is the CA the proxy mints leaf certificates with, under ~/.veris/ca.
// None yet is !: the first run mints one. Present, the key beside it is
// held to 0600 -- it can mint a certificate for any host -- while the
// certificate itself is public material the workload and docker read, and
// is written wider on purpose.
func (d *doctor) ca() {
	dir := defaultCADir()
	cert := filepath.Join(dir, "veris-ca.pem")
	key := filepath.Join(dir, "veris-ca-key.pem")
	if _, err := os.Stat(cert); err != nil {
		d.warn("ca", map[string]any{"path": cert}, "No CA at %s yet; the first run mints one", dir)
		return
	}
	detail := map[string]any{"path": cert}
	info, err := os.Stat(key)
	if err != nil {
		d.warn("ca", detail, "CA %s has no key beside it; the next run mints a new CA, and whatever trusted this one must trust it again", cert)
		return
	}
	perm := info.Mode().Perm()
	detail["key_mode"] = fmt.Sprintf("%04o", perm)
	if runtime.GOOS != "windows" && perm&0o077 != 0 {
		d.warn("ca", detail, "CA key %s is %04o, not 0600; it can mint a certificate for any host", key, perm)
		d.next("chmod 600 " + key)
		return
	}
	d.ok("ca", detail, "CA %s (key 0600)", cert)
}

// project is whether a .veris/twin.yaml was found up from here. None is !
// rather than ✗: a run can still name its sandbox itself.
func (d *doctor) project() {
	p := d.s.res.Project
	if p == nil {
		d.warn("project", nil, "No .veris/twin.yaml found (searched up from %s)", d.s.cwd)
		d.next("veris env create")
		return
	}
	n := len(p.Environments)
	what := fmt.Sprintf("%d environments", n)
	if n == 1 {
		what = "1 environment"
	}
	if p.Default != "" {
		what += fmt.Sprintf(", default '%s'", p.Default)
	}
	d.ok("project", map[string]any{"path": p.Path, "environments": n, "default": p.Default},
		"Project file %s (%s)", p.Path, what)
}

// environment is GET /v1/environments/{id} for the resolved environment,
// then what the project file says about it held against what the plane has:
// a require_service the environment does not run would fail every run with
// exit 3, and a baseline boot on an environment with no baseline boots the
// bundle instead.
func (d *doctor) environment(loggedIn bool) {
	res := d.s.res
	if res.EnvName == "" {
		switch {
		case res.Project == nil:
			// The project line above already said where to start.
		case len(res.Project.Environments) == 0:
			d.warn("environment", nil, "No environments in %s", res.Project.Path)
			d.next("veris env create")
		default:
			d.warn("environment", nil, "No environment selected")
			d.next("veris env use NAME, or pass --env")
		}
		return
	}
	name, id, conf := res.EnvName, res.EnvName, res.Env
	label := name
	// A name the project file does not know, and that is not shaped like
	// an id, is a typo: the refusal every other command prints, not a 404
	// that blames the plane.
	if conf == nil && res.Project != nil && len(res.Project.Environments) > 0 && !looksLikeID(name) {
		d.fail("environment", map[string]any{"name": name}, "No environment '%s' in %s (have: %s)",
			name, res.Project.Path, strings.Join(d.s.envNames(), ", "))
		d.next("veris env list")
		return
	}
	if conf != nil {
		if conf.ID == "" {
			d.fail("environment", nil, "Environment '%s' in %s has no id", name, res.Project.Path)
			d.next("veris env create " + name + " --from ID --force")
			return
		}
		id = conf.ID
		label = fmt.Sprintf("%s (%s)", name, shortID(id))
	}
	if !loggedIn {
		d.warn("environment", map[string]any{"name": name, "id": id},
			"Environment %s not checked: not logged in", label)
		return
	}
	var e *api.Environment
	err := d.call(func(ctx context.Context) error {
		var err error
		e, err = d.s.plane().GetEnvironment(ctx, id)
		return err
	})
	if err != nil {
		d.fail("environment", map[string]any{"name": name, "id": id},
			"Environment %s unreachable: %v", label, err)
		if api.IsStatus(err, http.StatusNotFound) {
			d.next("veris env list")
		}
		return
	}
	services := strings.Join(e.Services, ", ")
	if services == "" {
		services = "none"
	}
	d.ok("environment", map[string]any{"name": name, "id": id, "server_name": e.Name, "services": e.Services},
		"Environment %s reachable; services: %s", label, services)
	if conf == nil {
		return
	}
	for _, raw := range conf.Proxy.RequireService {
		want, _, _ := strings.Cut(strings.TrimSpace(raw), ":")
		if want == "" || contains(e.Services, want) {
			continue
		}
		d.warn("environment", map[string]any{"require_service": want, "services": e.Services},
			"proxy.require_service names %s, which '%s' does not run (has: %s)", want, name, services)
	}
	if conf.Boot == "baseline" && e.Baseline == nil {
		d.warn("environment", map[string]any{"boot": conf.Boot},
			"'%s' boots its baseline, but the environment has none pinned; up boots the bundle", name)
	}
}

// sandbox is GET /v1/sandboxes/{id} for this folder's sandbox, then each
// twin's own /veris/health: a sandbox the plane calls ready whose twin does
// not answer is exactly the run failure this screen exists to explain.
func (d *doctor) sandbox(loggedIn bool) {
	id := d.s.res.SandboxID
	if id == "" {
		d.warn("sandbox", nil, "No sandbox for this folder")
		d.next("veris up")
		return
	}
	if !loggedIn {
		d.warn("sandbox", map[string]any{"id": id}, "Sandbox %s not checked: not logged in", shortID(id))
		return
	}
	var sb *api.Sandbox
	err := d.call(func(ctx context.Context) error {
		var err error
		sb, err = d.s.plane().GetSandbox(ctx, id)
		return err
	})
	if err != nil {
		if api.IsStatus(err, http.StatusNotFound) {
			d.fail("sandbox", map[string]any{"id": id}, "Sandbox %s is gone: %v", shortID(id), err)
			d.next("veris up")
			return
		}
		d.fail("sandbox", map[string]any{"id": id}, "Sandbox %s unreachable: %v", shortID(id), err)
		return
	}
	detail := map[string]any{"id": id, "status": sb.Status, "expires_at": sb.ExpiresAt}
	expiry := ""
	if !sb.ExpiresAt.IsZero() {
		left := time.Until(sb.ExpiresAt.Time)
		if left <= 0 {
			d.fail("sandbox", detail, "Sandbox %s expired %s ago", shortID(id), durationText(-left))
			d.next("veris up")
			return
		}
		expiry = ", expires in " + durationText(left)
	}
	switch sb.Status {
	case api.StatusReady:
		d.ok("sandbox", detail, "Sandbox %s ready%s", shortID(id), expiry)
	case api.StatusFailed:
		d.fail("sandbox", detail, "Sandbox %s failed: %s", shortID(id), sb.FailureReason)
		d.next("veris down && veris up")
		return
	case api.StatusTerminating:
		d.fail("sandbox", detail, "Sandbox %s is terminating", shortID(id))
		d.next("veris up")
		return
	default:
		d.warn("sandbox", detail, "Sandbox %s still %s%s", shortID(id), sb.Status, expiry)
		d.next("veris status")
	}
	// The clock, like the callback probe below, is read once the sandbox is
	// up: while it is still on its way the sandbox line is the finding, and
	// a clock route that cannot answer yet would only restate it.
	if sb.Status == api.StatusReady {
		d.clock(sb)
	}
	for _, svc := range sb.Services {
		check := "twin:" + svc.Name
		if svc.ControlURL == "" {
			d.record("ok", check, svc.Name+" data plane (handed to the app, not proxied)", nil)
			d.ui.Detail("✓ %s  data plane (handed to the app, not proxied)", svc.Name)
			continue
		}
		var status string
		err := d.call(func(ctx context.Context) error {
			h, err := d.s.twin(svc.ControlURL).Health(ctx)
			if err == nil {
				status = h.Status
			}
			return err
		})
		// A twin that does not answer under a ready sandbox fails every
		// intercepted request, so it is ✗; while the sandbox is still on
		// its way the twins are expected to be quiet, and it is !.
		report := d.warn
		if sb.Status == api.StatusReady {
			report = d.fail
		}
		switch {
		case err != nil:
			report(check, map[string]any{"control_url": svc.ControlURL}, "%s  health: %v", svc.Name, err)
		case status != "ok":
			report(check, map[string]any{"control_url": svc.ControlURL, "status": status},
				"%s  health: status %q", svc.Name, status)
		default:
			d.record("ok", check, svc.Name+" ok", map[string]any{"control_url": svc.ControlURL})
			d.ui.Detail("✓ %s  ok", svc.Name)
			continue
		}
		if sb.Status == api.StatusReady {
			d.next("veris status")
		}
	}
	if sb.Status == api.StatusReady {
		d.callback(sb)
	}
}

// clock is GET …/sandboxes/{id}/clock: a frozen clock pauses outbound
// deliveries, so a webhook suite against it waits forever with nothing
// said, and is !; live, with or without an offset, is ✓.
func (d *doctor) clock(sb *api.Sandbox) {
	var clock *api.SandboxClock
	err := d.call(func(ctx context.Context) error {
		var err error
		clock, err = d.s.plane().GetSandboxClock(ctx, sb.EnvironmentID, sb.ID)
		return err
	})
	if err != nil {
		d.warn("clock", nil, "Clock of sandbox %s not read: %v", shortID(sb.ID), err)
		return
	}
	detail := map[string]any{"mode": clock.Mode, "offset_seconds": clock.OffsetSeconds}
	if clock.FrozenTime != nil {
		detail["frozen_time"] = *clock.FrozenTime
	}
	if clock.Mode == api.ClockModeFrozen {
		d.warn("clock", detail, "Clock %s; outbound deliveries are paused while it is frozen", clockLabel(clock))
		d.next("veris sandbox clock set --live")
		return
	}
	d.ok("clock", detail, "Clock %s", clockLabel(clock))
}

// callback is the sandbox's callback registration, read from the first
// twin that serves /veris/data (the row is a sandbox-wide singleton, so one
// twin answers for all). None registered is ✓ -- a run without --expose
// needs none. One registered whose probe answered is ✓; one the sandbox
// could not reach is !: a run started against it receives nothing, and the
// stale URL of an earlier run is the usual reason.
func (d *doctor) callback(sb *api.Sandbox) {
	var control string
	for _, svc := range sb.Services {
		if svc.ControlURL != "" && isHTTPURL(svc.ControlURL) {
			control = svc.ControlURL
			break
		}
	}
	if control == "" {
		return
	}
	var rows *twin.Rows
	err := d.call(func(ctx context.Context) error {
		var err error
		rows, err = d.s.twin(control).Rows(ctx, "client", 1, 0)
		return err
	})
	if err != nil {
		d.warn("callback", map[string]any{"control_url": control}, "Callback registration not read: %v", err)
		return
	}
	if len(rows.Rows) == 0 {
		d.ok("callback", map[string]any{"registered": false}, "No callback URL registered (run --expose PORT registers one)")
		return
	}
	row := rows.Rows[0]
	base, _ := row["default_base_url"].(string)
	state, _ := row["probe_state"].(string)
	if base == "" {
		d.ok("callback", map[string]any{"registered": false}, "No callback URL registered (run --expose PORT registers one)")
		return
	}
	detail := map[string]any{"registered": true, "url": base, "probe_state": state}
	if state == "answered" {
		d.ok("callback", detail, "Callbacks registered at %s (probe answered)", base)
		return
	}
	dead := ""
	if result, ok := row["last_probe_result"].(map[string]any); ok {
		dead, _ = result["dead_tunnel_signature"].(string)
	}
	if dead != "" {
		detail["dead_tunnel"] = dead
		d.warn("callback", detail, "Callbacks registered at %s, but the tunnel behind it is gone (probe_state %s); an earlier run left it", base, state)
	} else {
		d.warn("callback", detail, "Callbacks registered at %s, but the sandbox could not reach it (probe_state %s)", base, state)
	}
	d.next("veris run --expose PORT … registers this run's own URL")
}

// durationText renders a remaining time the way the doc's transcript does:
// "1h 49m", "49m", or "under a minute".
func durationText(d time.Duration) string {
	d = d.Round(time.Minute)
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm", m)
	}
	return "under a minute"
}

// firstLine is the first non-empty line of a command's output, trimmed: the
// line a daemon that is not running prints before its usage.
func firstLine(out []byte) string {
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return "no output"
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
