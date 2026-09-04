package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// runWatchCLI is runSandboxCLI with the stderr terminal forced on or off,
// and the panel's redraw turned from seconds into milliseconds. stdin stays
// what it is in every CLI test, a reader: the panel is stderr's business.
func runWatchCLI(t *testing.T, tty bool, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	in, hook, interval := stdin, newSessionHook, watchInterval
	stdin = strings.NewReader("")
	newSessionHook = func(s *session) { s.ui.OutTTY = tty }
	watchInterval = 5 * time.Millisecond
	t.Cleanup(func() { stdin, newSessionHook, watchInterval = in, hook, interval })
	var out, errOut bytes.Buffer
	err := cli.Execute(root(), &cli.Globals{}, args, &out, &errOut)
	return exitStatusTo(&errOut, err), out.String(), errOut.String()
}

// watchFor runs a --watch that would otherwise run until Ctrl-C, and ends
// it the way Ctrl-C would once the plane has answered polls reads of the
// sandbox.
func watchFor(t *testing.T, plane *sandboxPlane, polls int, tty bool, args ...string) (int, string, string) {
	t.Helper()
	mk := watchContext
	ctx, cancel := context.WithCancel(context.Background())
	watchContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { watchContext = mk; cancel() })
	plane.script(func(p *sandboxPlane) {
		prev := p.answer
		p.answer = func(poll int) *api.Sandbox {
			if poll >= polls {
				cancel()
			}
			return prev(poll)
		}
	})
	return runWatchCLI(t, tty, args...)
}

func TestWatchPanelRedrawsInPlace(t *testing.T) {
	t.Run("on a terminal every frame rewrites the last", func(t *testing.T) {
		var out bytes.Buffer
		p := newPanel(&ui.UI{Out: &out, OutTTY: true})
		p.render([]string{"a", "b", "c"})
		if got, want := out.String(), "\r\033[Ka\n\r\033[Kb\n\r\033[Kc\n"; got != want {
			t.Errorf("first frame %q, want %q", got, want)
		}
		out.Reset()
		p.render([]string{"x", "y"})
		// Up three, two lines rewritten, the third cleared, back up one so
		// the next frame starts where this one ends.
		if got, want := out.String(), "\033[3A\r\033[Kx\n\r\033[Ky\n\r\033[K\n\033[1A"; got != want {
			t.Errorf("second frame %q, want %q", got, want)
		}
		out.Reset()
		p.render([]string{"x", "y", "z"})
		if got, want := out.String(), "\033[2A\r\033[Kx\n\r\033[Ky\n\r\033[Kz\n"; got != want {
			t.Errorf("third frame %q, want %q", got, want)
		}
		out.Reset()
		p.end()
		p.end()
		if got := out.String(); got != "\n" {
			t.Errorf("end wrote %q, want one blank line under the frame however often it is called", got)
		}
	})

	t.Run("off a terminal, and when quiet, it writes nothing", func(t *testing.T) {
		// A terminal on stdin does not make one of stderr: `2>&1 | tee`
		// has exactly that shape, and the log must not fill with frames.
		for _, u := range []*ui.UI{{OutTTY: false}, {TTY: true, OutTTY: false}, {OutTTY: true, Quiet: true}} {
			var out bytes.Buffer
			u.Out = &out
			p := newPanel(u)
			p.render([]string{"a"})
			p.render([]string{"b"})
			p.end()
			if out.Len() != 0 {
				t.Errorf("TTY=%v OutTTY=%v Quiet=%v wrote %q", u.TTY, u.OutTTY, u.Quiet, out.String())
			}
		}
	})
}

func TestWatchFrameHasTheDocsShape(t *testing.T) {
	checked := time.Date(2026, 9, 2, 14, 4, 29, 0, time.Local)
	f := watchFrame{
		id:      sbID,
		env:     "checkout-svc (" + devID + ")",
		status:  "ready",
		expires: "2026-09-02 16:04:11",
		checked: checked,
		twins: []twinState{
			{name: "stripe", status: "routable", detail: "218 ms", ok: true},
			{name: "postgres", status: "ready", detail: "502 from gateway — retrying"},
		},
	}
	want := []string{
		"Sandbox " + sbID,
		"Environment: checkout-svc (" + devID + ")",
		"Status:      ready",
		"Services:    ████████░░░░░░░░  1/2 routable",
		"  ✓ stripe     routable   218 ms",
		"  · postgres   ready      502 from gateway — retrying",
		"Expires:     2026-09-02 16:04:11",
		"Last checked: 14:04:29",
	}
	got := f.lines(&ui.UI{})
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("frame:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := f.stateLines(&ui.UI{}); strings.Join(got, "\n") != strings.Join(want[2:6], "\n") {
		t.Errorf("state lines:\n%s", strings.Join(got, "\n"))
	}
	// The keys behind them name the state, not the text: a routable twin's
	// latency is left out, an unrouted twin's verdict is kept.
	wantKeys := []string{"status\x00ready", "services\x001/2", "stripe\x00routable", "postgres\x00ready\x00502 from gateway — retrying"}
	if got := f.stateKeys(); strings.Join(got, "|") != strings.Join(wantKeys, "|") {
		t.Errorf("state keys %q, want %q", got, wantKeys)
	}

	painted := f.lines(&ui.UI{Color: true})
	if !strings.Contains(painted[4], "\033[32m✓\033[0m") || !strings.Contains(painted[5], "\033[2m·\033[0m") {
		t.Errorf("with colour the marks are painted:\n%s", strings.Join(painted, "\n"))
	}
	if strings.Contains(strings.Join(got, "\n"), "\033[") {
		t.Errorf("without colour (NO_COLOR) no escape codes:\n%s", strings.Join(got, "\n"))
	}

	// The bar fills by the routable share; a sandbox with no services yet
	// has an empty bar rather than a division by zero.
	for _, c := range []struct {
		ok, total int
		want      string
	}{
		{0, 0, "░░░░░░░░░░░░░░░░"},
		{0, 2, "░░░░░░░░░░░░░░░░"},
		{1, 2, "████████░░░░░░░░"},
		{2, 3, "██████████░░░░░░"},
		{2, 2, "████████████████"},
	} {
		if got := watchBar(c.ok, c.total); got != c.want {
			t.Errorf("watchBar(%d, %d) = %s, want %s", c.ok, c.total, got, c.want)
		}
	}
}

func TestWatchUpEndsWhenRoutable(t *testing.T) {
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

	code, stdout, stderr := runWatchCLI(t, true, "up", "--watch")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout should be empty without --json, got %q", stdout)
	}
	// Three frames: provisioning with no twins yet, ready with stripe
	// behind a 502, ready with both through; then the tail as up prints it.
	sbInOrder(t, stderr,
		"✓ Sandbox created: "+sbID+"\n",
		"\n\r\033[KSandbox "+sbID+"\n",
		"Environment: checkout-ci ("+ciID+")\n",
		"Status:      provisioning\n",
		"Services:    ░░░░░░░░░░░░░░░░  0/0 routable\n",
		"Last checked: ",
		"\033[6A",
		"Status:      ready\n",
		"Services:    ████████░░░░░░░░  1/2 routable\n",
		"  · stripe     ready      502 from gateway — retrying\n",
		"  ✓ postgres   ready      (data plane; handed to the app, not proxied)\n",
		"Expires:     "+expires.Local().Format("2006-01-02 15:04:05")+"\n",
		"\033[8A",
		"Services:    ████████████████  2/2 routable\n",
		"  ✓ stripe     routable   ",
		" ms\n",
		"Last checked: ",
		"\n✓ Sandbox ready and routable (2/2 twins, ",
		" s)\n",
		"  stripe     STRIPE_API_BASE="+twins.srv.URL+"/s/"+sbID+"/stripe\n",
		"✓ Added data/customers.json: stripe customers 1, payment_methods 1\n",
		"✓ Up: "+sbID+" is this folder's sandbox (expires "+expires.Local().Format("15:04 MST")+")\n",
		"→ Next: veris run\n",
	)
	if strings.Contains(stderr, "Waiting for") {
		t.Errorf("the panel replaces the spinner:\n%s", stderr)
	}
	if n := twins.probes(); n != 2 {
		t.Errorf("health probes = %d, want 2 (one 502, one ok)", n)
	}
}

func TestWatchUpOutcomes(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()

	t.Run("a failed sandbox is exit 1 with its reason", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox {
				return &api.Sandbox{ID: sbID, EnvironmentID: devID, Status: api.StatusFailed, FailureReason: "image pull backoff"}
			}
		})
		code, _, stderr := runWatchCLI(t, true, "up", "--watch")
		if code != 1 {
			t.Errorf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "Status:      failed — image pull backoff\n", "\n✗ Sandbox "+sbID+" failed: image pull backoff\n")
		if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
			t.Errorf("the pointer stays so `veris down` finds it, got %+v", ptr)
		}
	})

	t.Run("the deadline is exit 4 with the sandbox kept", func(t *testing.T) {
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox {
				return &api.Sandbox{ID: sbID, EnvironmentID: devID, Status: api.StatusProvisioning}
			}
		})
		code, _, stderr := runWatchCLI(t, true, "up", "--watch", "--timeout", "100ms")
		if code != 4 {
			t.Errorf("exit %d, want 4:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "Status:      provisioning\n",
			"! Sandbox "+sbID+" is still provisioning after 100ms; it is kept and may still come up\n",
			"→ Next: veris status\n")
	})

	t.Run("ready but never routable is exit 4 naming the twin", func(t *testing.T) {
		expires := time.Now().Add(30 * time.Minute)
		twins.script(func(f *sandboxTwins) { f.healthFailures = 1 << 20 })
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(false), expires) }
		})
		code, _, stderr := runWatchCLI(t, true, "up", "--watch", "--timeout", "100ms")
		if code != 4 {
			t.Errorf("exit %d, want 4:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "  · stripe     ready      502 from gateway — retrying\n",
			"! Sandbox "+sbID+" is ready but not routable after 100ms (stripe: 502 from gateway); it is kept and may still come up\n")
	})

	t.Run("off a terminal --watch is the spinner path", func(t *testing.T) {
		expires := time.Now().Add(30 * time.Minute)
		twins.script(func(f *sandboxTwins) { f.healthFailures = 0 })
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(false), expires) }
		})
		code, _, stderr := runWatchCLI(t, false, "up", "--watch")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "Waiting for "+sbID+" · ready\n", "  ✓ stripe    routable  ", "✓ Up: "+sbID)
		if strings.Contains(stderr, "\033[") || strings.Contains(stderr, "Last checked") {
			t.Errorf("no panel off a terminal:\n%s", stderr)
		}
	})
}

func TestWatchStatusOffATerminalPrintsChanges(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	expires := time.Now().Add(3 * time.Hour)
	twins.script(func(f *sandboxTwins) { f.healthFailures = 1 })
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(false), expires) }
	})

	// The one-shot read is polls 1 and 2 (the sandbox, then its services);
	// the panel's ticks are 3 (stripe behind a 502) and 4 (through); 5 is
	// Ctrl-C.
	code, stdout, stderr := watchFor(t, plane, 5, false, "status", "--watch")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	stamp := `\d\d:\d\d:\d\d  `
	for _, want := range []string{
		"Boot:        bundle\n",
		"  Twin      Status  Env hint",
		stamp + "Status:      ready\n",
		stamp + "· stripe     ready      502 from gateway — retrying\n",
		stamp + `✓ postgres   ready      \(data plane; handed to the app, not proxied\)\n`,
		stamp + "✓ stripe     routable   \\d+ ms\n",
	} {
		if !regexp.MustCompile(want).MatchString(stderr) {
			t.Errorf("missing %q in:\n%s", want, stderr)
		}
	}
	if n := strings.Count(stderr, "✓ postgres   ready"); n != 1 {
		t.Errorf("an unchanged twin prints once, got %d:\n%s", n, stderr)
	}
	if strings.Contains(stderr, "\033[") || strings.Contains(stderr, "Last checked") {
		t.Errorf("no panel off a terminal:\n%s", stderr)
	}
}

// A routable twin's line carries the probe's latency, which is different on
// every tick; off a terminal it must still print once, on the tick the twin
// first got through, not once per tick.
func TestWatchStatusOffATerminalPrintsARoutableTwinOnce(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	expires := time.Now().Add(3 * time.Hour)
	// Every probe answers ok, each one slower than the last.
	twins.script(func(f *sandboxTwins) { f.healthFailures, f.healthDelay = 0, 3*time.Millisecond })
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(false), expires) }
	})

	// Polls 1 and 2 are the one-shot read; 3 to 8 are six panel ticks, each
	// probing stripe; 9 is Ctrl-C.
	code, stdout, stderr := watchFor(t, plane, 9, false, "status", "--watch")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	if n := twins.probes(); n < 3 {
		t.Fatalf("health probes = %d, want several ticks' worth", n)
	}
	// Each stamped state line once; the one-shot output above them is the
	// unstamped table.
	for _, want := range []string{
		`✓ stripe     routable   \d+ ms\n`,
		"Status:      ready\n",
		"Services:    ████████████████  2/2 routable\n",
		`✓ postgres   ready      \(data plane; handed to the app, not proxied\)\n`,
	} {
		stamped := regexp.MustCompile(`\d\d:\d\d:\d\d  ` + want)
		if n := len(stamped.FindAllString(stderr, -1)); n != 1 {
			t.Errorf("%q prints once, got %d:\n%s", want, n, stderr)
		}
	}
}

func TestWatchStatusOnATerminalRedrawsThePanel(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newSandboxTwins(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	expires := time.Now().Add(3 * time.Hour)
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(true), expires) }
	})

	code, stdout, stderr := watchFor(t, plane, 5, true, "sandbox", "get", "--watch")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "Boot:") || strings.Contains(stderr, "Twin") {
		t.Errorf("on a terminal the panel replaces the one-shot output:\n%s", stderr)
	}
	sbInOrder(t, stderr,
		"\r\033[KSandbox "+sbID+"\n",
		"Environment: checkout-ci ("+ciID+") → ci\n",
		"Status:      ready\n",
		"Services:    ████████████████  2/2 routable\n",
		"  ✓ stripe     routable   ",
		"  ✓ postgres   routable   ",
		"Expires:     "+expires.Local().Format("2006-01-02 15:04:05")+"\n",
		"Last checked: ",
		"\033[8A",
		"Last checked: ",
	)
	if !strings.HasSuffix(stderr, "\n\n") {
		t.Errorf("Ctrl-C leaves the frame and a blank line, got tail %q", stderr[max(0, len(stderr)-40):])
	}
}

func TestWatchRefusesJSON(t *testing.T) {
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	// --quiet is refused with --watch the same way: the panel and its state
	// lines are the watch's whole output, and a silent watch would sit until
	// Ctrl-C with nothing to show for it.
	for _, c := range []struct{ args, want string }{
		{"status --watch --json", "veris: status takes --watch or --json, not both\n"},
		{"sandbox get --watch --json", "veris: sandbox get takes --watch or --json, not both\n"},
		{"status --watch -q", "veris: status takes --watch or --quiet, not both\n"},
		{"sandbox get --watch --quiet", "veris: sandbox get takes --watch or --quiet, not both\n"},
	} {
		code, stdout, stderr := runWatchCLI(t, false, strings.Fields(c.args)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d, stdout %q, stderr:\n%s", c.args, code, stdout, stderr)
		}
	}
}
