package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// watchInterval is the wait between the panel's redraws. A variable so a
// test can turn seconds into milliseconds; the binary never changes it.
var watchInterval = 2 * time.Second

// watchContext is the context `status --watch` runs under: one that ends
// on Ctrl-C, so the loop returns cleanly (exit 0) with the last frame left
// on screen. Tests replace it with a context they cancel themselves.
var watchContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// --- the panel --------------------------------------------------------------

// panel is a block of lines on stderr redrawn in place: every render moves
// the cursor back to the top of the previous frame and rewrites each line,
// clearing it first, so a frame that got shorter leaves no tail behind. It
// is live only when stderr itself is a terminal (OutTTY, not the stdin
// answer that gates prompts: `2>&1 | tee` would keep every frame), and never
// when quiet; anywhere else render is a no-op and the caller prints lines of
// its own, because a log full of escape codes is worse than no panel at all.
// Colour is the UI's decision (NO_COLOR and TERM=dumb already turned it off
// there); the cursor moves are not colour and are always emitted while live.
type panel struct {
	u     *ui.UI
	live  bool
	drawn int // lines of the previous frame still on screen
}

func newPanel(u *ui.UI) *panel {
	return &panel{u: u, live: u.OutTTY && !u.Quiet}
}

// render replaces the previous frame with lines.
func (p *panel) render(lines []string) {
	if !p.live {
		return
	}
	var b strings.Builder
	if p.drawn > 0 {
		fmt.Fprintf(&b, "\033[%dA", p.drawn)
	}
	for _, l := range lines {
		b.WriteString("\r\033[K")
		b.WriteString(l)
		b.WriteString("\n")
	}
	// A frame shorter than the last: clear what it no longer covers, then
	// come back up so the next frame starts where this one ends.
	for i := len(lines); i < p.drawn; i++ {
		b.WriteString("\r\033[K\n")
	}
	if extra := p.drawn - len(lines); extra > 0 {
		fmt.Fprintf(&b, "\033[%dA", extra)
	}
	p.drawn = len(lines)
	fmt.Fprint(p.u.Out, b.String())
}

// end leaves the last frame on screen and moves on to a blank line, so
// whatever prints next does not land against the panel. Idempotent: the
// frame is handed over once, so a deferred end after an explicit one adds
// nothing.
func (p *panel) end() {
	if p.live && p.drawn > 0 {
		fmt.Fprintln(p.u.Out)
		p.drawn = 0
	}
}

// --- the frame --------------------------------------------------------------

// twinState is one twin as the panel shows it.
type twinState struct {
	name   string
	status string // "routable", else the control plane's word for the twin
	detail string // "218 ms", "502 from gateway — retrying", the data-plane note, or ""
	ok     bool   // routable, or data plane: counted in the bar, ✓ rather than ·
}

// watchFrame is one reading of a sandbox, rendered as the doc's panel:
//
//	Sandbox 7hqz4m2n9c1v5x8b3k6t0r2p4
//	Environment: checkout-svc (k3j2v0d8p1q7x9r2m5n8b4c6a)
//	Status:      ready
//	Services:    ████████░░░░░░░░  1/2 routable
//	  ✓ stripe     routable   218 ms
//	  · postgres   ready      502 from gateway — retrying
//	Expires:     2026-09-02 16:04:11
//	Last checked: 14:04:29
type watchFrame struct {
	id      string
	env     string
	status  string
	expires string
	twins   []twinState
	checked time.Time
}

// routable is how many twins are through, of how many.
func (f watchFrame) routable() (ok, total int) {
	for _, t := range f.twins {
		if t.ok {
			ok++
		}
	}
	return ok, len(f.twins)
}

// lines renders the frame. The marks are painted only when the UI paints.
func (f watchFrame) lines(u *ui.UI) []string {
	ok, total := f.routable()
	out := []string{
		"Sandbox " + f.id,
		"Environment: " + f.env,
		"Status:      " + f.status,
		fmt.Sprintf("Services:    %s  %d/%d routable", watchBar(ok, total), ok, total),
	}
	width := 0
	for _, t := range f.twins {
		width = max(width, len(t.name))
	}
	for _, t := range f.twins {
		line := fmt.Sprintf("  %s %-*s   %-8s   %s", watchMark(u, t.ok), width, t.name, t.status, t.detail)
		out = append(out, strings.TrimRight(line, " "))
	}
	return append(out,
		"Expires:     "+f.expires,
		"Last checked: "+f.checked.Format("15:04:05"))
}

// stateLines are the lines that change only when the sandbox does -- the
// status, the routable count and each twin -- for the off-terminal
// degradation, which prints each one as it first appears rather than a
// frame every two seconds.
func (f watchFrame) stateLines(u *ui.UI) []string {
	all := f.lines(u)
	return all[2 : 4+len(f.twins)]
}

// stateKeys name the state behind each of stateLines, one to one, so the
// degradation dedupes on what changed rather than on the rendered text: a
// routable twin's line carries the probe's latency, which differs on every
// tick and would otherwise make the same twin a new line every two seconds.
func (f watchFrame) stateKeys() []string {
	ok, total := f.routable()
	keys := []string{"status\x00" + f.status, fmt.Sprintf("services\x00%d/%d", ok, total)}
	for _, t := range f.twins {
		keys = append(keys, t.key())
	}
	return keys
}

// key is the twin's state without its latency.
func (t twinState) key() string {
	if t.ok && t.status == "routable" {
		return t.name + "\x00routable"
	}
	return t.name + "\x00" + t.status + "\x00" + t.detail
}

// watchBar is sixteen cells, filled left to right by the routable share.
func watchBar(ok, total int) string {
	const cells = 16
	filled := 0
	if total > 0 {
		filled = ok * cells / total
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", cells-filled)
}

func watchMark(u *ui.UI, ok bool) string {
	if ok {
		return watchPaint(u, "\033[32m", "✓")
	}
	return watchPaint(u, "\033[2m", "·")
}

func watchPaint(u *ui.UI, code, s string) string {
	if !u.Color {
		return s
	}
	return code + s + "\033[0m"
}

// watchEnvLine is the frame's environment as up knows it: the server's
// name and the id, or the id alone for an environment without a name.
func watchEnvLine(env *api.Environment) string {
	if env.Name == "" {
		return env.ID
	}
	return fmt.Sprintf("%s (%s)", env.Name, env.ID)
}

// --- one reading ------------------------------------------------------------

// watchProbe reads the sandbox once and, when the control plane calls it
// ready or degraded, probes every twin's /veris/health through the gateway
// the way up's routable wait does; a twin without a control URL is data
// plane, handed to the app rather than proxied, and counts as through. A
// sandbox still provisioning is not probed: the gateway has nothing to
// answer for yet, and each miss would cost a probe timeout. last keeps the
// verdict a probe cut short by the caller's deadline would otherwise lose.
func watchProbe(ctx context.Context, s *session, c *api.Client, id, envLine string, last map[string]string) (*api.Sandbox, watchFrame, error) {
	sb, err := c.GetSandbox(ctx, id)
	if err != nil {
		return nil, watchFrame{}, err
	}
	status := sb.Status
	if sb.FailureReason != "" {
		status += " — " + sb.FailureReason
	}
	f := watchFrame{id: sb.ID, env: envLine, status: status, expires: stampOf(sb.ExpiresAt), checked: time.Now()}
	probe := sb.Status == api.StatusReady || sb.Status == api.StatusDegraded
	for _, svc := range sb.Services {
		t := twinState{name: svc.Name, status: svc.Status}
		switch {
		case svc.ControlURL == "":
			t.ok, t.detail = true, "(data plane; handed to the app, not proxied)"
		case probe:
			t = probeTwin(ctx, s, svc, last)
		}
		f.twins = append(f.twins, t)
	}
	return sb, f, nil
}

// probeTwin is one health probe as a twin line.
func probeTwin(ctx context.Context, s *session, svc api.ServiceInfo, last map[string]string) twinState {
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, twinProbeTimeout)
	h, err := s.twin(svc.ControlURL).Health(pctx)
	cancel()
	if err == nil && h.Status == "ok" {
		return twinState{name: svc.Name, status: "routable", detail: fmt.Sprintf("%d ms", time.Since(start).Milliseconds()), ok: true}
	}
	// A probe the caller's deadline cut short says nothing about the twin;
	// the last answer the gateway gave is the verdict to show.
	if ctx.Err() == nil || last[svc.Name] == "" {
		last[svc.Name] = probeVerdict(err, h)
	}
	return twinState{name: svc.Name, status: svc.Status, detail: last[svc.Name] + " — retrying"}
}

// --- up --watch --------------------------------------------------------------

// watchUntilRoutable is up's wait under --watch on a terminal: the panel,
// redrawn every watchInterval, until the sandbox is ready and every twin
// answers through the gateway. The outcomes are the spinner path's: a
// failed sandbox is exit 1 with its reason; the deadline is exit 4 with
// the sandbox kept, since it may yet come up; Ctrl-C is exit 1 with the
// sandbox kept. The last frame stays on screen above the tail.
func watchUntilRoutable(ctx context.Context, s *session, c *api.Client, id, envLine string, deadline time.Time, timeout time.Duration) (*api.Sandbox, error) {
	wctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	p := newPanel(s.ui)
	defer p.end()
	start := time.Now()
	last := map[string]string{}
	// What the last frame showed, for the deadline's message when the read
	// that would have drawn the next one is what the deadline cut off.
	status := api.StatusProvisioning
	var twins []twinState
	for {
		sb, frame, err := watchProbe(wctx, s, c, id, envLine, last)
		switch {
		case err != nil && ctx.Err() != nil:
			return nil, watchStopped(s, id)
		case err != nil && (errors.Is(err, context.DeadlineExceeded) || wctx.Err() != nil):
			return nil, watchTimedOut(s, id, status, timeout, twins, last)
		case err != nil:
			return nil, s.fail("wait for", "sandbox "+id, err)
		}
		status, twins = sb.Status, frame.twins
		p.render(frame.lines(s.ui))
		if status == api.StatusFailed || status == api.StatusTerminating {
			reason := sb.FailureReason
			if reason == "" {
				reason = "status " + status
			}
			p.end()
			s.ui.Fail("Sandbox %s failed: %s", id, reason)
			return nil, printed(1)
		}
		ok, total := frame.routable()
		if status == api.StatusReady && ok == total {
			p.end()
			s.ui.Success("Sandbox ready and routable (%d/%d twins, %.1f s)", ok, total, time.Since(start).Seconds())
			return sb, nil
		}
		if !time.Now().Before(deadline) {
			return nil, watchTimedOut(s, id, status, timeout, frame.twins, last)
		}
		select {
		case <-ctx.Done():
			return nil, watchStopped(s, id)
		case <-time.After(min(watchInterval, time.Until(deadline))):
		}
	}
}

// watchStopped is Ctrl-C during the wait: exit 1, sandbox kept.
func watchStopped(s *session, id string) error {
	s.ui.Warn("Stopped waiting; sandbox %s is kept", id)
	s.ui.Next("veris status, or veris down")
	return printed(1)
}

// watchTimedOut is the deadline: exit 4, sandbox kept. A sandbox that got
// ready names the twins still short of routable and why.
func watchTimedOut(s *session, id, status string, timeout time.Duration, twins []twinState, last map[string]string) error {
	if status == api.StatusReady {
		var names []string
		for _, t := range twins {
			if !t.ok {
				names = append(names, fmt.Sprintf("%s: %s", t.name, last[t.name]))
			}
		}
		s.ui.Warn("Sandbox %s is ready but not routable after %s (%s); it is kept and may still come up",
			id, timeout, strings.Join(names, "; "))
	} else {
		s.ui.Warn("Sandbox %s is still %s after %s; it is kept and may still come up", id, status, timeout)
	}
	s.ui.Next("veris status")
	return printed(4)
}

// --- status --watch ----------------------------------------------------------

// watchStatus is status --watch and sandbox get --watch: the panel, redrawn
// every watchInterval, until Ctrl-C (exit 0). Off a terminal there is no
// panel to redraw; the normal output has already been printed, and each
// state line prints once, stamped, as its state first appears -- the
// sandbox's status and each twin's verdict, a routable twin with the
// latency of the probe that first got through -- so a log shows every
// change and nothing else.
func watchStatus(s *session, c *api.Client, id, envLine string) error {
	ctx, stop := watchContext()
	defer stop()
	p := newPanel(s.ui)
	defer p.end()
	last := map[string]string{}
	seen := map[string]bool{}
	for {
		_, frame, err := watchProbe(ctx, s, c, id, envLine, last)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			return s.fail("read", "sandbox "+id, err)
		}
		if p.live {
			p.render(frame.lines(s.ui))
		} else {
			stamp := frame.checked.Format("15:04:05")
			keys := frame.stateKeys()
			for i, line := range frame.stateLines(s.ui) {
				if !seen[keys[i]] {
					seen[keys[i]] = true
					s.ui.Info("%s  %s", stamp, strings.TrimSpace(line))
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(watchInterval):
		}
	}
}
