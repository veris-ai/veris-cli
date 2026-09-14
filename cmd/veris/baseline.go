package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
)

// baselineCommand is `veris baseline …`: the one image pinned as what every
// new sandbox of an environment boots. promote captures a sandbox and pins
// it; set repoints the pin at a snapshot's image; clear returns to the
// packaged bundle; list is this machine's record of those moves, since the
// control plane keeps no promote history a client can read.
func baselineCommand() *cli.Command {
	var promote captureOptions
	var keepSource bool
	return &cli.Command{
		Name:    "baseline",
		Summary: "What every new sandbox boots: get, promote, set, clear, list",
		Usage:   "veris baseline <command> [flags]",
		Help: "An environment boots the packaged bundle until a baseline is pinned; then every `veris up`\n" +
			"boots the pinned image and `veris sandbox reset` is refused for those sandboxes (a fresh\n" +
			"copy is `veris down && veris up`). Running sandboxes never change when the pin does.",
		Sub: []*cli.Command{
			{
				Name:    "get",
				Summary: "The environment's pinned baseline",
				Usage:   "veris baseline get [--json]",
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return baselineGet(ctx)
				},
			},
			{
				Name:    "promote",
				Summary: "Capture a sandbox and pin it as the baseline",
				Usage:   "veris baseline promote [--sandbox ID] [--clock-restore today|frozen|rebase] [--keep-external] [--keep-source] [--timeout 1800s] [--request-id ID] [--yes] [--json]",
				Help: "Tracks a durable capture operation when supported by the API. Reuse --request-id after\n" +
					"an interrupted wait. Terminal failures retain the source. Older APIs use legacy capture\n" +
					"and baseline polling. Use --keep-source until a fresh boot has verified the saved data.",
				Flags: func(fs *flag.FlagSet) {
					promote.bind(fs)
					fs.BoolVar(&keepSource, "keep-source", false, "keep the captured sandbox (it is left frozen and scrubbed)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return baselinePromote(ctx, promote, keepSource)
				},
			},
			{
				Name:    "set",
				Summary: "Repoint the baseline at a snapshot's image, or a digest",
				Usage:   "veris baseline set SNAPSHOT|DIGEST [--yes] [--json]",
				Help:    "SNAPSHOT is a snapshot id or name of this environment; DIGEST is a full image reference (repo@sha256:…) of one of its kept images.",
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) != 1 || args[0] == "" {
						return fmt.Errorf("baseline set takes one snapshot id, name or image digest (got %q)", strings.Join(args, " "))
					}
					return baselineSet(ctx, args[0])
				},
			},
			{
				Name:    "clear",
				Summary: "Unpin the baseline; sandboxes boot the packaged bundle",
				Usage:   "veris baseline clear [--yes] [--json]",
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return baselineClear(ctx)
				},
			},
			{
				Name:    "list",
				Summary: "This machine's record of promotions and repoints",
				Usage:   "veris baseline list [--json]",
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return baselineList(ctx)
				},
			},
		},
	}
}

// baselineName is how prompts and ✓ lines name an environment: the project
// file's name for it, else the server's, else the short id.
func baselineName(s *session, env *api.Environment) string {
	if name := projectEnvName(s, env.ID); name != "" {
		return name
	}
	if env.Name != "" {
		return env.Name
	}
	return shortID(env.ID)
}

// revisionOf is a pin's revision for a "(current: …)" or a "(was …)":
// "none" or "bundle" when nothing is pinned.
func revisionOf(b *api.EnvironmentBaseline, none string) string {
	if b == nil {
		return none
	}
	if b.RevisionID != "" {
		return b.RevisionID
	}
	return b.Image
}

// recordBaseline appends one move of the pin to the local file's ledger,
// the history the control plane does not serve. Without a project file
// there is no local file, which is said rather than failing a promote
// that has already happened.
func recordBaseline(s *session, b api.EnvironmentBaseline, envID string) error {
	if s.res.Local == nil {
		s.ui.Warn("no .veris/twin.yaml here, so this baseline is not recorded in a local ledger")
		return nil
	}
	s.res.Local.Baselines = append(s.res.Local.Baselines, cfg.BaselineRef{
		EnvironmentID: envID,
		Revision:      b.RevisionID,
		Image:         b.Image,
		PromotedAt:    stamp(b.PromotedAt),
		SourceSandbox: b.SourceSandbox,
	})
	return s.saveLocal()
}

// baselineSession is the session, client and environment the pin verbs
// act on: the in-use environment, read from the control plane.
func baselineSession(ctx *cli.Context) (*session, *api.Client, *api.Environment, error) {
	s, err := newSession(ctx, "", "")
	if err != nil {
		return nil, nil, nil, err
	}
	_, envID, _, err := s.requireEnv()
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := s.client()
	if err != nil {
		return nil, nil, nil, err
	}
	env, err := c.GetEnvironment(context.Background(), envID)
	if err != nil {
		return nil, nil, nil, s.fail("read", "environment "+envID, err)
	}
	return s, c, env, nil
}

// --- baseline get -----------------------------------------------------------

func baselineGet(ctx *cli.Context) error {
	s, _, env, err := baselineSession(ctx)
	if err != nil {
		return err
	}
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, env.Baseline)
	}
	name := baselineName(s, env)
	if env.Baseline == nil {
		s.ui.Info("Baseline of %s (%s): bundle (no baseline pinned)", name, shortID(env.ID))
		s.ui.Next("veris baseline promote")
		return nil
	}
	printBaseline(s, env)
	s.ui.Next("veris up")
	return nil
}

// printBaseline is the pin as the control plane holds it. The platform's
// baseline record carries no clock_restore: that is a property of the
// capture (a snapshot row has it; a promote's answer has it once), not of
// the pin.
func printBaseline(s *session, env *api.Environment) {
	b := env.Baseline
	s.ui.Info("Baseline of %s (%s)", baselineName(s, env), shortID(env.ID))
	s.ui.Info("Revision:  %s", dashIfBlank(b.RevisionID))
	s.ui.Info("Image:     %s", b.Image)
	s.ui.Info("Promoted:  %s", stampOf(b.PromotedAt))
	s.ui.Info("Source:    sandbox %s", dashIfBlank(b.SourceSandbox))
}

// --- baseline promote -------------------------------------------------------

// baselinePromote captures the sandbox and pins the image as its
// environment's baseline, reading the pin back when the answer is lost,
// then deletes the frozen source unless asked to keep it.
func baselinePromote(ctx *cli.Context, o captureOptions, keepSource bool) error {
	timeout, err := parseUpTimeout(o.timeout)
	if err != nil {
		return err
	}
	s, err := newSession(ctx, "", o.sandbox)
	if err != nil {
		return err
	}
	if !clockRestoreKnown(o.clockRestore) {
		s.ui.Fail("--clock-restore must be today, frozen or rebase (got '%s')", o.clockRestore)
		return printed(1)
	}
	id, err := s.requireSandbox()
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bg, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	sb, err := c.GetSandbox(bg, id)
	if err != nil {
		return s.fail("read", "sandbox "+id, err)
	}
	envID := sb.EnvironmentID
	env, err := c.GetEnvironment(bg, envID)
	if err != nil {
		return s.fail("read", "environment "+envID, err)
	}
	name := baselineName(s, env)
	previous := revisionOf(env.Baseline, "none")
	if err := confirm(s.ui, fmt.Sprintf("Pin sandbox %s as the baseline of '%s' (current: %s)? Every future up of this environment boots from it.",
		id, name, previous)); err != nil {
		return err
	}

	start := time.Now()
	req := api.PromoteRequest{ClockRestore: o.clockRestore, KeepExternalDestinations: o.keepExternal}
	var resp *api.PromoteResponse
	var pinned *api.EnvironmentBaseline
	call := captureCall{
		label:   fmt.Sprintf("Promoting (freeze → capture → push; polling GET /v1/environments/%s if the load balancer times out)", shortID(envID)),
		pollFor: fmt.Sprintf("GET /v1/environments/%s for baseline.source_sandbox=%s", envID, shortID(id)),
		post: func(ctx context.Context) error {
			var result api.PromoteResponse
			if supported, err := durableCapture(ctx, s, envID, api.CaptureRequest{SandboxID: id, Kind: "promote", RequestID: o.requestID, ClockRestore: o.clockRestore, KeepExternalDestinations: o.keepExternal}, &result); supported {
				resp = &result
				return err
			}
			r, err := captureClient(s).PromoteSandbox(ctx, envID, id, req)
			resp = r
			return err
		},
		poll: func(ctx context.Context) (bool, error) {
			now, err := c.GetEnvironment(ctx, envID)
			if err != nil {
				return false, err
			}
			b := now.Baseline
			if b == nil || b.SourceSandbox != id || revisionOf(b, "") == previous {
				return false, nil
			}
			pinned = b
			return true, nil
		},
		check: "check veris baseline get before re-running",
	}
	dropped, err := runCapture(bg, s, "promote", "sandbox "+id, timeout, call)
	if err != nil {
		if api.IsStatus(err, http.StatusConflict) {
			s.ui.Detail("another capture holds this sandbox; wait for it, then veris baseline get")
		}
		return err
	}
	clock := o.clockRestore
	if clock == "" {
		clock = api.ClockToday
	}
	if dropped {
		s.ui.Success("Baseline pinned: %s (clock_restore %s, promoted %s, %s)",
			revisionOf(pinned, ""), clock, hhmmss(pinned.PromotedAt), elapsedText(time.Since(start)))
		s.ui.Detail("%s", pinned.Image)
		s.ui.Detail("scrub details unavailable: the load balancer dropped the response")
	} else {
		pinned = &resp.Baseline
		restored := ", curator clock restored"
		if !resp.CuratorClockRestored {
			restored = ""
		}
		s.ui.Success("Baseline pinned: %s (%s, clock_restore %s, promoted %s, %s%s)",
			revisionOf(pinned, ""), sizeText(resp.SizeBytes), resp.ClockRestore,
			hhmmss(pinned.PromotedAt), elapsedText(time.Since(start)), restored)
		s.ui.Detail("%s", pinned.Image)
		printScrubbed(s.ui, resp.Scrubbed, o.keepExternal)
		if !resp.CuratorClockRestored {
			s.ui.Warn("the source sandbox %s could not be handed its clock back; it stays frozen with delivery paused", id)
		}
	}
	// The pin is real from here: it is recorded before the source is
	// touched, and the link, the hint and the --json body print whatever
	// the delete does, so a failed delete cannot hide a promote that
	// happened. The delete's own failure still owns the exit code.
	if err := recordBaseline(s, *pinned, envID); err != nil {
		return err
	}
	var deleteErr error
	if keepSource {
		s.ui.Warn("the source sandbox %s is frozen and scrubbed; delete it: %s", id, deleteHint(s, id))
	} else if deleteErr = deleteSandbox(bg, s, c, sb); deleteErr != nil {
		s.ui.Warn("the source sandbox %s was not deleted; it is frozen and scrubbed: %s", id, deleteHint(s, id))
	}
	studioLink(s.ui, s.consoleURL(), "environments", envID)
	s.ui.Next(nextAfterPromote(s, id))
	if s.ctx.Globals.JSON {
		var body any = resp
		if dropped {
			body = map[string]any{"environment_id": envID, "sandbox_id": id, "baseline": pinned, "response_dropped": true}
		}
		if err := printJSON(s.ctx.Stdout, body); err != nil {
			return err
		}
	}
	return deleteErr
}

// nextAfterPromote is the → Next after a promote: a sandbox booted from the
// new pin. `veris down` comes first only while the folder's pointer still
// names the frozen source (--keep-source, or a delete that failed); once
// the source is gone, or when it was never this folder's, `down` would fail
// or delete an unrelated sandbox, and `up` alone is the step.
func nextAfterPromote(s *session, id string) string {
	if deleteHint(s, id) == "veris down" {
		return "veris down && veris up"
	}
	return "veris up"
}

// --- baseline set / clear ---------------------------------------------------

// baselineSet repoints the pin at a snapshot's image (by id or name) or at
// a digest reference given in full. The control plane verifies the digest
// is one of the environment's own images before pinning it.
func baselineSet(ctx *cli.Context, ref string) error {
	s, c, env, err := baselineSession(ctx)
	if err != nil {
		return err
	}
	bg := context.Background()
	name := baselineName(s, env)
	image, target := ref, ref
	if !strings.Contains(ref, "@sha256:") {
		snaps, err := c.ListSnapshots(bg, env.ID)
		if err != nil {
			return s.fail("list", "snapshots of '"+name+"'", err)
		}
		sn, err := snapshotByRef(s, snaps, ref, name)
		if err != nil {
			return err
		}
		image = sn.Image
		label := sn.ID
		if sn.Name != "" {
			label = sn.Name
		}
		target = fmt.Sprintf("snapshot %s (%s, %s)", label, sn.RevisionID, shortDigest(sn.Image))
	}
	if err := confirm(s.ui, fmt.Sprintf("Repoint %s's baseline to %s? Running sandboxes are unaffected.", name, target)); err != nil {
		return err
	}
	was := revisionOf(env.Baseline, "bundle")
	after, err := c.ResetEnvironment(bg, env.ID, &image)
	if err != nil {
		return s.fail("set", "baseline of '"+name+"'", err)
	}
	if after.Baseline == nil {
		s.ui.Fail("The control plane answered without a baseline after the reset; nothing is pinned")
		s.ui.Next("veris baseline get")
		return printed(1)
	}
	s.ui.Success("Baseline now %s (was %s)", revisionOf(after.Baseline, ""), was)
	s.ui.Detail("%s", after.Baseline.Image)
	if err := recordBaseline(s, *after.Baseline, env.ID); err != nil {
		return err
	}
	s.ui.Next("veris down && veris up")
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, after)
	}
	return nil
}

// hhmmss is the doc's "promoted 10:41:12": the instant as local wall time,
// "—" when the control plane sent none.
func hhmmss(t api.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("15:04:05")
}

// shortDigest renders an image reference as "…@sha256:41aa…", enough to
// tell two images apart in a prompt.
func shortDigest(image string) string {
	i := strings.Index(image, "@sha256:")
	if i < 0 {
		return image
	}
	hex := image[i+len("@sha256:"):]
	if len(hex) > 4 {
		hex = hex[:4] + "…"
	}
	return "…@sha256:" + hex
}

func baselineClear(ctx *cli.Context) error {
	s, c, env, err := baselineSession(ctx)
	if err != nil {
		return err
	}
	name := baselineName(s, env)
	if env.Baseline == nil {
		s.ui.Warn("'%s' has no baseline pinned; sandboxes already boot the packaged bundle", name)
		if s.ctx.Globals.JSON {
			return printJSON(s.ctx.Stdout, env)
		}
		return nil
	}
	if err := confirm(s.ui, fmt.Sprintf("Clear %s's baseline (current: %s)? Sandboxes will boot the packaged bundle; running ones are unaffected.",
		name, revisionOf(env.Baseline, ""))); err != nil {
		return err
	}
	after, err := c.ResetEnvironment(context.Background(), env.ID, nil)
	if err != nil {
		return s.fail("clear", "baseline of '"+name+"'", err)
	}
	if after.Baseline != nil {
		s.ui.Fail("The control plane still reports baseline %s after the reset", revisionOf(after.Baseline, ""))
		s.ui.Next("veris baseline get")
		return printed(1)
	}
	s.ui.Success("Baseline cleared; sandboxes boot the packaged bundle")
	s.ui.Next("veris down && veris up")
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, after)
	}
	return nil
}

// --- baseline list ----------------------------------------------------------

// baselineList is the local ledger, newest first. It needs a project file,
// since the ledger lives in the local file beside it.
func baselineList(ctx *cli.Context) error {
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	if _, err := s.requireProject(); err != nil {
		return err
	}
	var refs []cfg.BaselineRef
	if s.res.Local != nil {
		refs = s.res.Local.Baselines
	}
	if s.ctx.Globals.JSON {
		if refs == nil {
			refs = []cfg.BaselineRef{}
		}
		return printJSON(s.ctx.Stdout, refs)
	}
	s.ui.Info("Baseline ledger (this machine's record; the platform keeps no promote history)")
	if len(refs) == 0 {
		s.ui.Info("No baselines recorded on this machine")
		s.ui.Next("veris baseline promote")
		return nil
	}
	rows := make([][]string, 0, len(refs))
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		rows = append(rows, []string{"  " + envLabel(s, r.EnvironmentID), dashIfBlank(r.Revision),
			shortID(dashIfBlank(r.SourceSandbox)), ledgerStamp(r.PromotedAt), r.Image})
	}
	s.ui.Table([]string{"  Environment", "Revision", "Source", "Promoted", "Image"}, rows)
	return nil
}

// ledgerStamp renders a ledger instant, written by stamp, in local time.
func ledgerStamp(v string) string {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return dashIfBlank(v)
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
