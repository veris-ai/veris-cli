package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/twin"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// Boot sources an environment config (or --boot) may name. "bundle" and
// "baseline" send nothing: the server boots the environment's pinned
// baseline when it has one and the global bundle otherwise, so the two
// differ only in what up tells the user. "snapshot" sends snapshot_id.
const (
	bootBundle   = "bundle"
	bootBaseline = "baseline"
	bootSnapshot = "snapshot"
)

const (
	// No default TTL lives here. When neither the flag nor the config says,
	// the request omits ttl_minutes and the control plane applies its own
	// default -- the same reasoning as the vendor hostnames: a number the
	// server owns, kept in one place, so a change there needs no release
	// here. Its bounds are the server's too, and its refusal names them.

	// defaultUpTimeout is up's whole budget for ready plus routable.
	defaultUpTimeout = "300s"
)

// Timings of up's two waits and of the per-twin probes status runs. They
// are variables so a test can turn seconds into milliseconds; the binary
// never changes them.
var (
	sandboxPollInterval = 2 * time.Second
	routableInterval    = time.Second
	twinProbeTimeout    = 5 * time.Second
)

// sandboxCommands is up, status, down and the sandbox group: the everyday
// verbs act on this folder's sandbox, the group takes an explicit --id.
func sandboxBaseCommands() []*cli.Command {
	var getID, deleteID, resetID, listEnv string
	var downAll, listAll, statusWatch, getWatch bool
	return []*cli.Command{
		upCommand(),
		{
			Name:    "status",
			Summary: "This folder's sandbox and its twins",
			Usage:   "veris status [--watch] [--json]",
			Help: "status is sandbox get for this folder: the sandbox's state, boot source and expiry, then every twin's status, env hint, URL and table counts.\n" +
				"--watch keeps a live panel of the sandbox and its twins' routability on stderr, redrawn every 2 s until Ctrl-C;\n" +
				"off a terminal it prints the normal output and then one line per change.",
			Flags: func(fs *flag.FlagSet) {
				fs.BoolVar(&statusWatch, "watch", false, "keep a live panel until Ctrl-C")
			},
			Run: func(ctx *cli.Context, args []string) error {
				if err := noPositionals(ctx, args); err != nil {
					return err
				}
				if err := noWatchJSON(ctx, statusWatch); err != nil {
					return err
				}
				return sandboxGet(ctx, "", statusWatch)
			},
		},
		{
			Name:    "down",
			Summary: "Delete this folder's sandbox",
			Usage:   "veris down [--all] [--yes]",
			Help:    "down deletes this folder's sandbox and clears the pointer. --all deletes every sandbox of the in-use environment after one confirmation. The TTL is the backstop either way.",
			Flags: func(fs *flag.FlagSet) {
				fs.BoolVar(&downAll, "all", false, "delete every sandbox of the in-use environment")
			},
			Run: func(ctx *cli.Context, args []string) error {
				if err := noPositionals(ctx, args); err != nil {
					return err
				}
				return sandboxDelete(ctx, "", downAll)
			},
		},
		{
			Name:    "sandbox",
			Summary: "Sandboxes by id: get, list, delete, reset",
			Usage:   "veris sandbox <command> [--id ID] [flags]",
			Help:    "The same verbs as status and down, for a sandbox named by --id (default: this folder's).",
			Sub: []*cli.Command{
				{
					Name:    "get",
					Summary: "One sandbox: status, boot source, expiry and its twins",
					Usage:   "veris sandbox get [--id ID] [--watch] [--json]",
					Flags: func(fs *flag.FlagSet) {
						fs.StringVar(&getID, "id", "", "sandbox id (default: this folder's)")
						fs.BoolVar(&getWatch, "watch", false, "keep a live panel until Ctrl-C")
					},
					Run: func(ctx *cli.Context, args []string) error {
						if err := noPositionals(ctx, args); err != nil {
							return err
						}
						if err := noWatchJSON(ctx, getWatch); err != nil {
							return err
						}
						return sandboxGet(ctx, getID, getWatch)
					},
				},
				{
					Name:    "list",
					Summary: "Sandboxes of an environment, or of every environment",
					Usage:   "veris sandbox list [--env NAME | --all] [--json]",
					Help:    "Without --all the sandboxes of the in-use environment (or --env NAME). --all asks the control plane for every sandbox and, where it has no such route, fans out over every environment; an environment that cannot be listed is a ! line, never silence.",
					Flags: func(fs *flag.FlagSet) {
						fs.StringVar(&listEnv, "env", "", "environment name or id to list")
						fs.BoolVar(&listAll, "all", false, "every environment")
					},
					Run: func(ctx *cli.Context, args []string) error {
						if err := noPositionals(ctx, args); err != nil {
							return err
						}
						if listAll && listEnv != "" {
							return &cli.UsageError{Msg: "sandbox list takes --env or --all, not both", Cmd: nil}
						}
						return sandboxList(ctx, listEnv, listAll)
					},
				},
				{
					Name:    "delete",
					Summary: "Delete a sandbox",
					Usage:   "veris sandbox delete [--id ID] [--yes]",
					Flags: func(fs *flag.FlagSet) {
						fs.StringVar(&deleteID, "id", "", "sandbox id (default: this folder's)")
					},
					Run: func(ctx *cli.Context, args []string) error {
						if err := noPositionals(ctx, args); err != nil {
							return err
						}
						return sandboxDelete(ctx, deleteID, false)
					},
				},
				{
					Name:    "reset",
					Summary: "Restore every twin to its boot seed and set the clock live",
					Usage:   "veris sandbox reset [--id ID] [--yes]",
					Help:    "Refused (409) for a sandbox booted from a snapshot or a promoted baseline: that world is an image, and a fresh copy is `veris down && veris up`.",
					Flags: func(fs *flag.FlagSet) {
						fs.StringVar(&resetID, "id", "", "sandbox id (default: this folder's)")
					},
					Run: func(ctx *cli.Context, args []string) error {
						if err := noPositionals(ctx, args); err != nil {
							return err
						}
						return sandboxReset(ctx, resetID)
					},
				},
			},
		},
	}
}

// noPositionals refuses stray words on a command that takes none, so
// `veris down dev` does not silently delete whatever the folder points at.
func noPositionals(ctx *cli.Context, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("%s takes no arguments (got %q)", strings.Join(ctx.Path[1:], " "), strings.Join(args, " "))
}

// noWatchJSON refuses --watch beside --json on a command that would never
// end: stdout carries one body, and a panel that runs until Ctrl-C has no
// one body to carry. --quiet is refused with it for the mirror reason: the
// panel and its state lines are the watch's whole output, and a watch with
// nothing to say would sit silent until Ctrl-C.
func noWatchJSON(ctx *cli.Context, watch bool) error {
	if !watch || ctx.Globals == nil {
		return nil
	}
	if ctx.Globals.JSON {
		return fmt.Errorf("%s takes --watch or --json, not both", strings.Join(ctx.Path[1:], " "))
	}
	if ctx.Globals.Quiet {
		return fmt.Errorf("%s takes --watch or --quiet, not both", strings.Join(ctx.Path[1:], " "))
	}
	return nil
}

// --- up ---------------------------------------------------------------------

// upOptions are up's own flags; "" and 0 mean "not given", which lets the
// environment config answer.
type upOptions struct {
	env         string
	ttl         int
	boot        string
	snapshot    string
	callbackURL string
	timeout     string
	watch       bool
}

func upCommand() *cli.Command {
	var o upOptions
	return &cli.Command{
		Name:    "up",
		Summary: "Start a sandbox of the environment and wait for it",
		Usage:   "veris up [NAME | --env NAME] [--ttl N] [--boot bundle|baseline|snapshot] [--snapshot ID|NAME] [--callback-url URL] [--timeout 300s] [--watch] [--json]",
		Help: "up deploys a sandbox of the environment (NAME, --env, the folder's `use`, or the project default),\n" +
			"remembers its id for this folder at once, waits until the control plane reports it ready and\n" +
			"every twin answers through the gateway, adds the environment config's data files, and prints\n" +
			"the env-var hints the code under test needs. Settings come from the flag, then the environment\n" +
			"config, then the defaults: boot bundle, and no TTL of its own -- a sandbox nobody gave one\n" +
			"lives as long as the control plane's own default. --watch shows the wait as a live panel of\n" +
			"the sandbox and its twins on a terminal, redrawn every 2 s until every twin is routable.",
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&o.watch, "watch", false, "show the wait as a live panel (terminal only)")
			fs.StringVar(&o.env, "env", "", "environment name or id (same as NAME)")
			fs.IntVar(&o.ttl, "ttl", 0, "sandbox lifetime in minutes (config, then the control plane's default)")
			fs.StringVar(&o.boot, "boot", "", "what the sandbox boots: bundle, baseline or snapshot (config, then bundle)")
			fs.StringVar(&o.snapshot, "snapshot", "", "snapshot id or name, for --boot snapshot")
			fs.StringVar(&o.callbackURL, "callback-url", "", "where the twins deliver callbacks (config)")
			fs.StringVar(&o.timeout, "timeout", defaultUpTimeout, "budget for ready and routable, e.g. 300s or 5m")
		},
		Run: func(ctx *cli.Context, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("up takes one environment name, got %q", strings.Join(args, " "))
			}
			name := o.env
			if len(args) == 1 {
				if name != "" && name != args[0] {
					return fmt.Errorf("up was given both NAME %q and --env %q", args[0], name)
				}
				name = args[0]
			}
			return up(ctx, name, o)
		},
	}
}

// up is the whole start-up: resolve, create, remember, wait, probe, seed,
// print. Every failure after the create leaves the sandbox and its pointer
// in place, since a sandbox that exists is worth more than a clean folder.
func up(ctx *cli.Context, name string, o upOptions) error {
	s, sb, err := upSandbox(ctx, name, o, true)
	if err != nil {
		return err
	}
	if s.res.Local != nil {
		s.ui.Success("Up: %s is this folder's sandbox (expires %s)", sb.ID, clockOf(sb.ExpiresAt))
	} else {
		s.ui.Success("Up: %s is ready (expires %s)", sb.ID, clockOf(sb.ExpiresAt))
	}
	studioLink(s.ui, s.consoleURL(), "sandboxes", sb.ID)
	s.ui.Next("veris run")
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, sb)
	}
	return nil
}

// upSandbox is up without its closing lines: resolve, create, wait, probe
// and seed, returning the session and the ready sandbox. remember is
// whether the folder's pointer is written, as up does at once so `veris
// down` finds what was created; run --fresh passes false, since the sandbox
// is its own and deleted before it returns. The sandbox is returned beside
// the error of any step after the create, so a caller that made it for
// itself can still delete it.
func upSandbox(ctx *cli.Context, name string, o upOptions, remember bool) (*session, *api.Sandbox, error) {
	timeout, err := parseUpTimeout(o.timeout)
	if err != nil {
		return nil, nil, err
	}
	s, err := newSession(ctx, name, "")
	if err != nil {
		return nil, nil, err
	}
	if o.boot != "" && !upBootKnown(o.boot) {
		s.ui.Fail("--boot must be bundle, baseline or snapshot (got '%s')", o.boot)
		return s, nil, printed(1)
	}
	envName, envID, conf, err := s.requireEnv()
	if err != nil {
		return s, nil, err
	}
	c, err := s.client()
	if err != nil {
		return s, nil, err
	}
	// Ctrl-C ends the waits, not the sandbox: the pointer is written before
	// any wait starts, so `veris down` finds what was created.
	bg, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ttl, boot, snapshot, callback := upSettings(o, conf)
	if !upBootKnown(boot) {
		s.ui.Fail("Environment '%s' has boot '%s' in %s; it must be bundle, baseline or snapshot",
			envName, boot, s.res.Project.Path)
		return s, nil, printed(1)
	}
	// A --snapshot beside --boot baseline or bundle would be dropped without
	// a word; the user who named it gets a baseline world and no warning.
	if o.snapshot != "" && boot != bootSnapshot {
		s.ui.Fail("--snapshot only applies with --boot snapshot (got --boot %s)", boot)
		return s, nil, printed(1)
	}
	env, err := c.GetEnvironment(bg, envID)
	if err != nil {
		return s, nil, s.fail("read", "environment "+envID, err)
	}

	req := api.CreateSandboxRequest{Metadata: map[string]string{"project": upProjectName(s, envName)}}
	if ttl > 0 {
		req.TTLMinutes = &ttl
	}
	bootLabel := bootBundle
	switch boot {
	case bootBundle:
		// "bundle" sends nothing, and the server boots a pinned baseline
		// before the bundle whatever the config says; say what will boot.
		if env.Baseline != nil {
			bootLabel = "baseline " + shortID(env.Baseline.RevisionID) + " (pinned)"
		}
	case bootBaseline:
		if env.Baseline == nil {
			s.ui.Warn("Environment '%s' has no baseline; the sandbox boots the bundle", envName)
		} else {
			bootLabel = "baseline " + shortID(env.Baseline.RevisionID)
		}
	case bootSnapshot:
		if snapshot == "" {
			s.ui.Fail("--boot snapshot needs --snapshot ID|NAME (or `snapshot:` in the environment config)")
			return s, nil, printed(1)
		}
		id, err := resolveSnapshot(bg, s, c, envID, snapshot)
		if err != nil {
			return s, nil, err
		}
		req.SnapshotID = &id
		bootLabel = "snapshot " + shortID(id)
	}
	if callback != "" {
		req.ClientBaseURL = &callback
	}

	serverName := env.Name
	if serverName == "" {
		serverName = shortID(env.ID)
	}
	// Nothing asked for a TTL, so nothing here knows one: the request omits
	// it and the sandbox lives as long as the control plane says.
	life := "ttl from the control plane"
	if ttl > 0 {
		life = fmt.Sprintf("ttl %d min", ttl)
	}
	s.ui.Info("Starting '%s' (%s: %s) · boot %s · %s",
		envName, serverName, strings.Join(env.Services, ", "), bootLabel, life)
	// The pointer is about to be replaced: a sandbox it still names keeps
	// running until its TTL and is reachable afterwards only by id, so the
	// orphan is announced with the command that deletes it.
	if old := priorSandbox(s); remember && old != "" {
		s.ui.Warn("This folder already pointed at sandbox %s; it keeps running until its TTL (veris sandbox delete --id %s)", old, old)
	}
	sb, err := c.CreateSandbox(bg, envID, req)
	if err != nil {
		return s, nil, s.fail("create", "sandbox", err)
	}
	s.ui.Success("Sandbox created: %s", sb.ID)
	if remember {
		if err := s.rememberSandbox(sb); err != nil {
			return s, sb, err
		}
	}

	deadline := time.Now().Add(timeout)
	// --watch is the panel when stderr is a terminal; anywhere else the
	// spinner path already prints a line per state change, which is the
	// degradation.
	if o.watch && s.ui.OutTTY && !s.ui.Quiet {
		s.ui.Info("")
		ready, err := watchUntilRoutable(bg, s, c, sb.ID, watchEnvLine(env), deadline, timeout)
		if err != nil {
			return s, sb, err
		}
		sb = ready
	} else {
		ready, err := waitReady(bg, s, c, sb.ID, deadline, timeout)
		if err != nil {
			return s, sb, err
		}
		sb = ready
		if err := waitRoutable(bg, s, sb, deadline, timeout); err != nil {
			return s, sb, err
		}
	}
	printHints(s.ui, sb.Services)
	if conf != nil && len(conf.Data) > 0 {
		if err := addDataFiles(bg, s, sb, conf.Data); err != nil {
			return s, sb, err
		}
	}
	return s, sb, nil
}

// priorSandbox is the id the local file points at when that sandbox may
// still be running: one whose recorded expiry has passed is gone on its own
// and not worth a warning. "" when there is nothing to announce.
func priorSandbox(s *session) string {
	if s.res.Local == nil || s.res.Local.Sandbox == nil || s.res.Local.Sandbox.ID == "" {
		return ""
	}
	ptr := s.res.Local.Sandbox
	if exp, err := time.Parse(time.RFC3339, ptr.ExpiresAt); err == nil && exp.Before(time.Now()) {
		return ""
	}
	return ptr.ID
}

// upSettings applies flag → environment config → default for each of up's
// settings. A --snapshot on its own means --boot snapshot: naming the
// snapshot is the intent, and asking for the word too would be pedantry.
func upSettings(o upOptions, conf *cfg.EnvConfig) (ttl int, boot, snapshot, callback string) {
	ttl, boot, snapshot, callback = o.ttl, o.boot, o.snapshot, o.callbackURL
	if conf != nil {
		if ttl == 0 {
			ttl = conf.TTLMinutes
		}
		if snapshot == "" {
			snapshot = conf.Snapshot
		}
		if callback == "" {
			callback = conf.CallbackURL
		}
	}
	if boot == "" {
		switch {
		case o.snapshot != "":
			boot = bootSnapshot
		case conf != nil && conf.Boot != "":
			boot = conf.Boot
		default:
			boot = bootBundle
		}
	}
	return ttl, boot, snapshot, callback
}

func upBootKnown(b string) bool {
	return b == bootBundle || b == bootBaseline || b == bootSnapshot
}

// parseUpTimeout reads --timeout as a duration, or as bare seconds, since
// "300" is what a person types for 300 s.
func parseUpTimeout(v string) (time.Duration, error) {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--timeout: '%s' is not a duration (try 300s or 5m)", v)
	}
	return d, nil
}

// upProjectName is the sandbox's metadata.project: the project file's name,
// else its folder, else the environment name in a folder without a file.
func upProjectName(s *session, envName string) string {
	if p := s.res.Project; p != nil {
		if p.Project != "" {
			return p.Project
		}
		return filepath.Base(p.Dir())
	}
	return envName
}

// resolveSnapshot turns --snapshot into an id: an id-shaped value is used as
// given; a name is looked up in the environment's snapshots, and when
// several share it the newest wins, said aloud.
func resolveSnapshot(ctx context.Context, s *session, c *api.Client, envID, ref string) (string, error) {
	if looksLikeID(ref) {
		return ref, nil
	}
	snaps, err := c.ListSnapshots(ctx, envID)
	if err != nil {
		return "", s.fail("list", "snapshots of environment "+envID, err)
	}
	var matches []api.Snapshot
	for _, sn := range snaps {
		if sn.Name == ref {
			matches = append(matches, sn)
		}
	}
	if len(matches) == 0 {
		names := make([]string, 0, len(snaps))
		for _, sn := range snaps {
			if sn.Name != "" {
				names = append(names, sn.Name)
			}
		}
		have := "none"
		if len(names) > 0 {
			have = strings.Join(names, ", ")
		}
		s.ui.Fail("No snapshot named '%s' in environment %s (have: %s)", ref, shortID(envID), have)
		return "", printed(1)
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].CreatedAt.After(matches[j].CreatedAt.Time) })
	if len(matches) > 1 {
		s.ui.Warn("%d snapshots are named '%s'; using the newest, %s (%s)",
			len(matches), ref, matches[0].ID, matches[0].CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	return matches[0].ID, nil
}

// waitReady polls the control plane until the sandbox is ready, a spinner
// naming the status meanwhile. A sandbox that failed is exit 1 with its
// reason; one still on its way at the deadline is exit 4 and kept, since it
// may yet come up; the pointer stays in both cases so `veris down` works.
func waitReady(ctx context.Context, s *session, c *api.Client, id string, deadline time.Time, timeout time.Duration) (*api.Sandbox, error) {
	sp := s.ui.Spinner(fmt.Sprintf("Waiting for %s · %s", id, api.StatusProvisioning))
	sb, err := c.WaitSandbox(ctx, id, api.WaitOptions{
		Interval: sandboxPollInterval,
		// A budget already spent must not fall back to WaitSandbox's own
		// five-minute default, hence the floor of one nanosecond.
		Timeout: max(time.Until(deadline), time.Nanosecond),
		OnPoll: func(sb *api.Sandbox) {
			sp.Update(fmt.Sprintf("Waiting for %s · %s", id, sb.Status))
		},
	})
	sp.Stop()
	if err == nil {
		return sb, nil
	}
	var failed *api.SandboxFailedError
	var timedOut *api.WaitTimeoutError
	switch {
	case errors.As(err, &failed):
		reason := failed.Reason
		if reason == "" {
			reason = "status " + failed.Sandbox.Status
		}
		s.ui.Fail("Sandbox %s failed: %s", id, reason)
		return nil, printed(1)
	case errors.As(err, &timedOut):
		s.ui.Warn("Sandbox %s is still %s after %s; it is kept and may still come up", id, timedOut.Sandbox.Status, timeout)
		s.ui.Next("veris status")
		return nil, printed(4)
	case ctx.Err() != nil:
		s.ui.Warn("Stopped waiting; sandbox %s is kept", id)
		s.ui.Next("veris status, or veris down")
		return nil, printed(1)
	}
	return nil, s.fail("wait for", "sandbox "+id, err)
}

// upSpinner is the spinner across up's probes. On a TTY it is stopped before
// a ✓ line prints and started again after, so the line does not land beside
// a frame; off a TTY the one spinner object stays, because every new one
// would print its label again.
type upSpinner struct {
	u     *ui.UI
	sp    *ui.Spinner
	label string
}

func newUpSpinner(u *ui.UI, label string) *upSpinner {
	return &upSpinner{u: u, sp: u.Spinner(label), label: label}
}

func (p *upSpinner) set(label string) { p.label = label; p.sp.Update(label) }
func (p *upSpinner) pause()           { p.sp.Stop() }
func (p *upSpinner) resume() {
	if p.u.TTY {
		p.sp = p.u.Spinner(p.label)
	}
}

// waitRoutable probes every twin's /veris/health through the gateway until
// each answers ok, because the control plane measures ready from its own
// node and the public ingress can lag it by seconds. Each twin prints its
// line the first time it answers; a service with no control URL is data
// plane, handed to the app rather than proxied, and needs no probe.
func waitRoutable(ctx context.Context, s *session, sb *api.Sandbox, deadline time.Time, timeout time.Duration) error {
	width := nameWidth(sb.Services)
	var pending []api.ServiceInfo
	for _, svc := range sb.Services {
		if svc.ControlURL == "" {
			s.ui.Info("  ✓ %-*s  ready  (data plane; handed to the app, not proxied)", width, svc.Name)
			continue
		}
		pending = append(pending, svc)
	}
	if len(pending) == 0 {
		return nil
	}
	// The ready poll can land at the deadline; a probe made under an
	// expired context fails in the transport before any twin is asked, and
	// reporting that as the twin's verdict would blame a twin never probed.
	if !time.Now().Before(deadline) {
		s.ui.Warn("Sandbox %s is ready; the %s budget left no time to probe its twins. It is kept and may well be routable",
			sb.ID, timeout)
		s.ui.Next("veris status")
		return printed(4)
	}
	sp := newUpSpinner(s.ui, fmt.Sprintf("Waiting for %s · %s  %s  probing", sb.ID, pending[0].Name, pending[0].Status))
	defer sp.pause()
	lastErr := map[string]string{}
	for {
		var still []api.ServiceInfo
		var lines []string
		for _, svc := range pending {
			start := time.Now()
			probeDeadline := start.Add(twinProbeTimeout)
			pctx, cancel := context.WithDeadline(ctx, minTime(deadline, probeDeadline))
			h, err := s.twin(svc.ControlURL).Health(pctx)
			cancel()
			ms := time.Since(start).Milliseconds()
			if err == nil && h.Status == "ok" {
				lines = append(lines, fmt.Sprintf("  ✓ %-*s  routable  %d ms", width, svc.Name, ms))
				continue
			}
			// A probe the overall deadline cut short says nothing about the
			// twin; the last answer the gateway gave is the verdict to report.
			cutShort := deadline.Before(probeDeadline) && errors.Is(err, context.DeadlineExceeded)
			if !cutShort || lastErr[svc.Name] == "" {
				lastErr[svc.Name] = probeVerdict(err, h)
			}
			still = append(still, svc)
		}
		if len(lines) > 0 {
			sp.pause()
			for _, l := range lines {
				s.ui.Info("%s", l)
			}
			sp.resume()
		}
		pending = still
		if len(pending) == 0 {
			return nil
		}
		first := pending[0]
		sp.set(fmt.Sprintf("Waiting for %s · %s  %s  %s — retrying", sb.ID, first.Name, first.Status, lastErr[first.Name]))
		if time.Now().After(deadline) {
			sp.pause()
			names := make([]string, 0, len(pending))
			for _, svc := range pending {
				names = append(names, fmt.Sprintf("%s: %s", svc.Name, lastErr[svc.Name]))
			}
			s.ui.Warn("Sandbox %s is ready but not routable after %s (%s); it is kept and may still come up",
				sb.ID, timeout, strings.Join(names, "; "))
			s.ui.Next("veris status")
			return printed(4)
		}
		select {
		case <-ctx.Done():
			sp.pause()
			s.ui.Warn("Stopped waiting; sandbox %s is kept", sb.ID)
			s.ui.Next("veris status, or veris down")
			return printed(1)
		case <-time.After(routableInterval):
		}
	}
}

// probeVerdict is the short reason a health probe did not count, for the
// spinner: "502 from gateway" for a status the twin did not send itself,
// the twin's own status word when it answered but not ok, "unreachable"
// for a probe that timed out, else the error.
func probeVerdict(err error, h *twin.Health) string {
	if err == nil {
		return fmt.Sprintf("status '%s'", h.Status)
	}
	var te *twin.Error
	if errors.As(err, &te) {
		return fmt.Sprintf("%d from gateway", te.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("unreachable (no answer within %s)", twinProbeTimeout)
	}
	return err.Error()
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// nameWidth is the longest service name, so the per-twin lines align.
func nameWidth(services []api.ServiceInfo) int {
	w := 0
	for _, svc := range services {
		w = max(w, len(svc.Name))
	}
	return w
}

// addDataFiles seeds the environment config's data files. Each is JSON
// keyed by twin name; a twin's value is either a map of tables to rows
// (POST /veris/data) or {"sql": PATH}, DDL for a postgres twin's
// /veris/seed. Paths are relative to the project directory. The first
// failure stops the seeding and exits 1 with the sandbox kept: a half-seeded
// world is still there to look at, and a re-run adds only what is missing.
func addDataFiles(ctx context.Context, s *session, sb *api.Sandbox, files []string) error {
	dir := s.res.Project.Dir()
	for _, file := range files {
		raw, err := os.ReadFile(projectPath(dir, file))
		if err != nil {
			return s.fail("read", file, err)
		}
		names, byTwin, err := parseDataFile(raw)
		if err != nil {
			return s.fail("parse", file, err)
		}
		var parts []string
		for _, name := range names {
			svc := findService(sb.Services, name)
			if svc == nil {
				s.ui.Fail("%s names twin '%s', which sandbox %s does not run (have: %s)",
					file, name, sb.ID, strings.Join(serviceNames(sb.Services), ", "))
				return printed(1)
			}
			if svc.ControlURL == "" {
				s.ui.Fail("%s: twin '%s' has no control URL to add data through", file, name)
				return printed(1)
			}
			data := byTwin[name]
			tw := s.twin(svc.ControlURL)
			if sqlRef, ok := data["sql"].(string); ok {
				sql, err := os.ReadFile(projectPath(dir, sqlRef))
				if err != nil {
					return s.fail("read", sqlRef, err)
				}
				if _, err := tw.Seed(ctx, string(sql)); err != nil {
					return s.fail("seed", name+" from "+sqlRef, err)
				}
				parts = append(parts, fmt.Sprintf("%s %s (%d bytes)", name, sqlRef, len(sql)))
				continue
			}
			w, err := tw.Add(ctx, data)
			if err != nil {
				return s.fail("add", file+" to "+name, err)
			}
			for _, warn := range w.Warnings {
				s.ui.Warn("%s: %s", name, warn)
			}
			parts = append(parts, name+" "+countsLine(w.Added))
		}
		s.ui.Success("Added %s: %s", file, strings.Join(parts, " · "))
	}
	return nil
}

// parseDataFile reads a data file's bytes: a JSON object keyed by twin name
// whose values are objects of tables to rows, or {"sql": PATH} for a
// postgres twin. The names come back sorted so two runs seed in the same
// order. It is shared by up (the environment config's files) and by
// `sandbox data add` (files named on the command line).
func parseDataFile(raw []byte) (names []string, byTwin map[string]map[string]any, err error) {
	var blocks map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, fmt.Errorf("not a JSON object keyed by twin name: %w", err)
	}
	byTwin = make(map[string]map[string]any, len(blocks))
	for name, block := range blocks {
		var data map[string]any
		if err := json.Unmarshal(block, &data); err != nil {
			return nil, nil, fmt.Errorf("'%s' must be an object of tables to rows", name)
		}
		byTwin[name] = data
		names = append(names, name)
	}
	sort.Strings(names)
	return names, byTwin, nil
}

// projectPath resolves a config path against the project directory.
func projectPath(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func findService(services []api.ServiceInfo, name string) *api.ServiceInfo {
	for i := range services {
		if services[i].Name == name {
			return &services[i]
		}
	}
	return nil
}

func serviceNames(services []api.ServiceInfo) []string {
	names := make([]string, 0, len(services))
	for _, svc := range services {
		names = append(names, svc.Name)
	}
	return names
}

// countsLine renders per-table counts as "customers 1, payment_methods 1",
// sorted by table so two runs read the same.
func countsLine(counts map[string]int) string {
	if len(counts) == 0 {
		return "nothing"
	}
	tables := make([]string, 0, len(counts))
	for t := range counts {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		parts = append(parts, fmt.Sprintf("%s %d", t, counts[t]))
	}
	return strings.Join(parts, ", ")
}

// printHints prints what the code under test needs: one ENV_HINT=url line
// per service. A URL that is not http is a data-plane DSN the app dials
// itself, and says so beneath.
func printHints(u *ui.UI, services []api.ServiceInfo) {
	width := nameWidth(services)
	for _, svc := range services {
		v := svc.URL
		if svc.EnvHint != "" {
			v = svc.EnvHint + "=" + svc.URL
		}
		u.Info("  %-*s   %s", width, svc.Name, v)
		if !isHTTPURL(svc.URL) {
			u.Info("  %-*s   (data plane; handed to the app, not proxied)", width, "")
		}
	}
}

func isHTTPURL(u string) bool {
	return strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")
}

// --- status / sandbox get ---------------------------------------------------

// sandboxGet is status and sandbox get: the sandbox as the control plane
// sees it, then each twin as it answers itself. idFlag is --id, "" for this
// folder's sandbox. watch keeps going: on a terminal the panel replaces
// the one-shot output and is redrawn until Ctrl-C; off one the one-shot
// output prints and each change after it is a line.
func sandboxGet(ctx *cli.Context, idFlag string, watch bool) error {
	s, err := newSession(ctx, "", idFlag)
	if err != nil {
		return err
	}
	id, err := s.requireSandbox()
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bg := context.Background()
	sb, err := c.GetSandbox(bg, id)
	if err != nil {
		return s.fail("read", "sandbox "+id, err)
	}
	// The scoped services route is the same record; reading it keeps the
	// table honest about what the control plane serves under /services.
	services, err := c.GetSandboxServices(bg, id)
	if err != nil {
		return s.fail("read", "services of sandbox "+id, err)
	}
	sb.Services = services
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, sb)
	}
	env, envErr := c.GetEnvironment(bg, sb.EnvironmentID)

	envLine := sb.EnvironmentID
	if env != nil && env.Name != "" {
		envLine = fmt.Sprintf("%s (%s)", env.Name, sb.EnvironmentID)
	}
	if name := projectEnvName(s, sb.EnvironmentID); name != "" {
		envLine += " → " + name
	}
	if envErr != nil {
		s.ui.Warn("could not read environment %s: %v", shortID(sb.EnvironmentID), envErr)
	}
	if watch && s.ui.OutTTY && !s.ui.Quiet {
		return watchStatus(s, c, sb.ID, envLine)
	}
	s.ui.Info("Sandbox %s", sb.ID)
	s.ui.Info("Environment: %s", envLine)
	status := sb.Status
	if sb.FailureReason != "" {
		status += " — " + sb.FailureReason
	}
	s.ui.Info("Status:      %s", status)
	s.ui.Info("Boot:        %s", bootSource(sb, env))
	s.ui.Info("Expires:     %s", stampOf(sb.ExpiresAt))

	rows := make([][]string, 0, len(services))
	for _, svc := range services {
		hint := svc.EnvHint
		if hint == "" {
			hint = "—"
		}
		rows = append(rows, []string{"  " + svc.Name, svc.Status, hint, svc.URL, tableCounts(bg, s, svc)})
	}
	if len(rows) > 0 {
		s.ui.Table([]string{"  Twin", "Status", "Env hint", "URL", "Tables"}, rows)
	}
	if watch {
		return watchStatus(s, c, sb.ID, envLine)
	}
	return nil
}

// bootSource is what the sandbox booted, as far as the records say: the
// sandbox names its snapshot; a baseline or the bundle is the environment's
// pin, so an unreadable environment leaves it unknown.
func bootSource(sb *api.Sandbox, env *api.Environment) string {
	switch {
	case sb.SnapshotID != "":
		return "snapshot " + sb.SnapshotID
	case env == nil:
		return "—"
	case env.Baseline != nil:
		return "baseline " + env.Baseline.RevisionID
	}
	return bootBundle
}

// tableCounts is one twin's row counts from the bare GET /veris/data, as
// "customers 41 · faults 0"; "—" when the twin does not answer or has no
// such route (the postgres twin serves no data listing). The clock and
// client singletons are not tables the user seeded and are left out.
func tableCounts(ctx context.Context, s *session, svc api.ServiceInfo) string {
	if svc.ControlURL == "" {
		return "—"
	}
	pctx, cancel := context.WithTimeout(ctx, twinProbeTimeout)
	defer cancel()
	counts, err := s.twin(svc.ControlURL).Counts(pctx)
	if err != nil {
		return "—"
	}
	tables := make([]string, 0, len(counts.Counts))
	for t := range counts.Counts {
		if t != "clock" && t != "client" {
			tables = append(tables, t)
		}
	}
	if len(tables) == 0 {
		return "—"
	}
	sort.Strings(tables)
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		parts = append(parts, fmt.Sprintf("%s %d", t, counts.Counts[t]))
	}
	return strings.Join(parts, " · ")
}

// projectEnvName is the name the project file gives an environment id, or
// "" when no configured environment points at it.
func projectEnvName(s *session, envID string) string {
	if s.res.Project == nil {
		return ""
	}
	for _, n := range s.envNames() {
		if s.res.Project.Environments[n].ID == envID {
			return n
		}
	}
	return ""
}

// envLabel names an environment for a prompt or a table: the project's name
// for it, else the short id.
func envLabel(s *session, envID string) string {
	if name := projectEnvName(s, envID); name != "" {
		return name
	}
	return shortID(envID)
}

// clockOf is an instant as "HH:MM ZONE" local, the doc's "expires 12:34
// EDT"; "—" when the control plane sent none. The zone is printed because
// the local file beside it records the same instant in UTC, and a bare
// clock reading against an RFC 3339 stamp looks like two different times.
func clockOf(t api.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("15:04 MST")
}

// stampOf is the full local instant, for the status panel.
func stampOf(t api.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// --- sandbox list -----------------------------------------------------------

// sandboxList lists the in-use environment's sandboxes (or --env NAME's),
// or with --all every environment's. The control plane may grow a list-all
// route; until it does, the 404 is the cue to fan out over every
// environment, saying so for each one that cannot be listed.
func sandboxList(ctx *cli.Context, envFlag string, all bool) error {
	s, err := newSession(ctx, envFlag, "")
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bg := context.Background()
	serverNames := map[string]string{}
	var sbs []api.Sandbox
	if all {
		envs, err := c.ListEnvironments(bg)
		if err != nil {
			return s.fail("list", "environments", err)
		}
		for _, env := range envs {
			serverNames[env.ID] = env.Name
		}
		sbs, err = listAllSandboxes(bg, c)
		switch {
		case api.IsStatus(err, http.StatusNotFound):
			for _, env := range envs {
				list, err := c.ListSandboxes(bg, env.ID)
				if err != nil {
					s.ui.Warn("could not list sandboxes of %s (%s): %v", env.Name, shortID(env.ID), err)
					continue
				}
				sbs = append(sbs, list...)
			}
		case err != nil:
			return s.fail("list", "sandboxes", err)
		}
	} else {
		name, envID, _, err := s.requireEnv()
		if err != nil {
			return err
		}
		if sbs, err = c.ListSandboxes(bg, envID); err != nil {
			return s.fail("list", "sandboxes of '"+name+"'", err)
		}
	}
	sort.SliceStable(sbs, func(i, j int) bool { return sbs[i].CreatedAt.After(sbs[j].CreatedAt.Time) })
	if s.ctx.Globals.JSON {
		if sbs == nil {
			sbs = []api.Sandbox{}
		}
		return printJSON(s.ctx.Stdout, sbs)
	}
	if len(sbs) == 0 {
		s.ui.Info("No sandboxes")
		s.ui.Next("veris up")
		return nil
	}
	rows := make([][]string, 0, len(sbs))
	for _, sb := range sbs {
		mark := " "
		if sb.ID == s.res.SandboxID {
			mark = "●"
		}
		envName := projectEnvName(s, sb.EnvironmentID)
		if envName == "" {
			envName = serverNames[sb.EnvironmentID]
		}
		if envName == "" {
			envName = shortID(sb.EnvironmentID)
		}
		rows = append(rows, []string{mark + " " + shortID(sb.ID), envName, sb.Status,
			clockOf(sb.ExpiresAt), strings.Join(serviceNames(sb.Services), ", ")})
	}
	s.ui.Table([]string{"  Sandbox", "Environment", "Status", "Expires", "Twins"}, rows)
	return nil
}

// listAllSandboxes is GET /v1/sandboxes, a route the control plane does not
// serve today and the api client therefore lacks. It is sent with the
// client's own transport and headers so the day the route lands it answers,
// and until then its 404 is what sandboxList fans out on.
func listAllSandboxes(ctx context.Context, c *api.Client) ([]api.Sandbox, error) {
	endpoint := c.Base + "/v1/sandboxes"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.Key != "" {
		req.Header.Set("X-API-Key", c.Key)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the control plane at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read GET %s: %w", endpoint, err)
	}
	if resp.StatusCode/100 != 2 {
		e := &api.Error{Status: resp.StatusCode, Method: http.MethodGet, URL: endpoint, Body: body}
		var envelope struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(body, &envelope) == nil && envelope.Detail != "" {
			e.Detail = envelope.Detail
		} else if e.Detail = strings.TrimSpace(string(body)); e.Detail == "" {
			e.Detail = http.StatusText(resp.StatusCode)
		}
		return nil, e
	}
	var out []api.Sandbox
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("GET %s answered %d with a body that is not a sandbox list: %w", endpoint, resp.StatusCode, err)
	}
	return out, nil
}

// --- down / sandbox delete --------------------------------------------------

// sandboxDelete is down and sandbox delete. all deletes every sandbox of
// the in-use environment after one confirmation; otherwise idFlag (--id) or
// this folder's sandbox goes, after the doc's question.
func sandboxDelete(ctx *cli.Context, idFlag string, all bool) error {
	s, err := newSession(ctx, "", idFlag)
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bg := context.Background()
	if all {
		return deleteAllSandboxes(bg, s, c)
	}
	id, err := s.requireSandbox()
	if err != nil {
		return err
	}
	sb, err := c.GetSandbox(bg, id)
	if api.IsStatus(err, http.StatusNotFound) {
		return sandboxGone(s, id)
	}
	if err != nil {
		return s.fail("read", "sandbox "+id, err)
	}
	if err := confirm(s.ui, fmt.Sprintf("Delete sandbox %s (%s, expires %s)?",
		sb.ID, envLabel(s, sb.EnvironmentID), clockOf(sb.ExpiresAt))); err != nil {
		return err
	}
	return deleteSandbox(bg, s, c, sb)
}

func deleteAllSandboxes(ctx context.Context, s *session, c *api.Client) error {
	name, envID, _, err := s.requireEnv()
	if err != nil {
		return err
	}
	sbs, err := c.ListSandboxes(ctx, envID)
	if err != nil {
		return s.fail("list", "sandboxes of '"+name+"'", err)
	}
	if len(sbs) == 0 {
		s.ui.Info("No sandboxes of '%s' to delete", name)
		return nil
	}
	noun := "sandboxes"
	if len(sbs) == 1 {
		noun = "sandbox"
	}
	if err := confirm(s.ui, fmt.Sprintf("Delete %d %s of '%s' (%s)?", len(sbs), noun, name, shortID(envID))); err != nil {
		return err
	}
	// Stop at the first failure: a delete that did not happen is the thing
	// to look at, and the rest are still one `down --all` away.
	for i := range sbs {
		if err := deleteSandbox(ctx, s, c, &sbs[i]); err != nil {
			return err
		}
	}
	return nil
}

// deleteSandbox is the DELETE plus the bookkeeping: the pointer is
// forgotten when it named this sandbox, and a 404 is "already gone", which
// is the outcome the user wanted.
func deleteSandbox(ctx context.Context, s *session, c *api.Client, sb *api.Sandbox) error {
	err := c.DeleteSandbox(ctx, sb.EnvironmentID, sb.ID)
	if api.IsStatus(err, http.StatusNotFound) {
		return sandboxGone(s, sb.ID)
	}
	if err != nil {
		return s.fail("delete", "sandbox "+sb.ID, err)
	}
	if err := forgetIfOurs(s, sb.ID); err != nil {
		return err
	}
	s.ui.Success("Sandbox deleted: %s", sb.ID)
	return nil
}

func sandboxGone(s *session, id string) error {
	if err := forgetIfOurs(s, id); err != nil {
		return err
	}
	s.ui.Warn("Sandbox %s was already gone", id)
	return nil
}

// forgetIfOurs clears the folder's pointer only when it named id: deleting
// somebody else's sandbox by --id must not orphan this folder's.
func forgetIfOurs(s *session, id string) error {
	if s.res.Local == nil || s.res.Local.Sandbox == nil || s.res.Local.Sandbox.ID != id {
		return nil
	}
	return s.forgetSandbox()
}

// --- sandbox reset ----------------------------------------------------------

// sandboxReset rebuilds every twin's boot world and sets the clock live. A
// 409 is a sandbox booted from an image, which the control plane will not
// reseed in place; the allowed move is printed under the refusal.
func sandboxReset(ctx *cli.Context, idFlag string) error {
	s, err := newSession(ctx, "", idFlag)
	if err != nil {
		return err
	}
	id, err := s.requireSandbox()
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bg := context.Background()
	sb, err := c.GetSandbox(bg, id)
	if err != nil {
		return s.fail("read", "sandbox "+id, err)
	}
	if err := confirm(s.ui, fmt.Sprintf("Reset every service in %s to its boot profile and set the clock live?", id)); err != nil {
		return err
	}
	res, err := c.ResetSandbox(bg, sb.EnvironmentID, id)
	if err != nil {
		failed := s.fail("reset", "sandbox", err)
		if api.IsStatus(err, http.StatusConflict) {
			s.ui.Link("This world came from an image; a fresh copy is one command away:")
			s.ui.Detail("veris down && veris up")
		}
		return failed
	}
	var failures []string
	for _, svc := range res.Services {
		if !svc.OK {
			failures = append(failures, fmt.Sprintf("%s (%s)", svc.Name, resetDetail(svc.Detail)))
		}
	}
	if len(failures) > 0 {
		s.ui.Fail("Reset failed for: %s", strings.Join(failures, ", "))
		return printed(1)
	}
	for _, svc := range res.Services {
		if n, ok := seededTables(svc.Detail); ok {
			s.ui.Success("%s reset (%d tables)", svc.Name, n)
		} else {
			s.ui.Success("%s reset", svc.Name)
		}
	}
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, res)
	}
	return nil
}

// resetDetail is a failed service's detail as one line: the error string
// the control plane relays, or whatever JSON it sent instead.
func resetDetail(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil && text != "" {
		return text
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "no detail"
	}
	return string(raw)
}

// seededTables reads an HTTP twin's {reset, seeded} detail for how many
// tables the reset seeded; the postgres twin's {ok} has no such count.
func seededTables(raw json.RawMessage) (int, bool) {
	var detail struct {
		Seeded map[string]int `json:"seeded"`
	}
	if json.Unmarshal(raw, &detail) != nil || detail.Seeded == nil {
		return 0, false
	}
	return len(detail.Seeded), true
}
