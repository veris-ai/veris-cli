package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// defaultCaptureTimeout bounds the client wait. The server has its own
// capture budget; durable operations survive a disconnected client.
const defaultCaptureTimeout = "1800s"

// capturePollInterval is how often the poll after a dropped answer reads
// the control plane; a variable so a test turns seconds into milliseconds.
var capturePollInterval = 5 * time.Second

// captureOptions are the flags snapshot create and baseline promote share:
// which sandbox to capture and how the captured world treats its clock.
type captureOptions struct {
	sandbox      string
	clockRestore string
	keepExternal bool
	timeout      string
	requestID    string
}

func (o *captureOptions) bind(fs *flag.FlagSet) {
	fs.StringVar(&o.requestID, "request-id", "", "reuse a durable capture request after an interrupted wait")
	fs.StringVar(&o.sandbox, "sandbox", "", "sandbox id to capture (default: this folder's)")
	fs.StringVar(&o.clockRestore, "clock-restore", "", "what a sandbox booted from the capture does with its clock: today (default), frozen or rebase")
	fs.BoolVar(&o.keepExternal, "keep-external", false, "keep third-party webhook destinations in the image (default: scrub them)")
	fs.StringVar(&o.timeout, "timeout", defaultCaptureTimeout, "client deadline for the capture, and for the poll after a dropped answer")
}

// clockRestoreKnown reports whether v is one of the modes the control plane
// accepts; "" leaves the choice to the server (today).
func clockRestoreKnown(v string) bool {
	return v == "" || v == api.ClockToday || v == api.ClockFrozen || v == api.ClockRebase
}

// snapshotCommand is `veris snapshot …`: record a sandbox's world as an
// image that later sandboxes can boot from (`veris up --snapshot`). Many
// snapshots coexist; none changes what the environment boots by default,
// which is the baseline's job.
func snapshotCommand() *cli.Command {
	var create captureOptions
	var name string
	var deleteSource bool
	return &cli.Command{
		Name:    "snapshot",
		Summary: "Recorded worlds: create, list, get, delete",
		Usage:   "veris snapshot <command> [flags]",
		Help: "A snapshot is one sandbox's world captured as an image beside its environment. Any number\n" +
			"coexist and any can be booted with `veris up --snapshot ID|NAME`; none changes what the\n" +
			"environment boots by default (see veris baseline). Capturing freezes and scrubs the source\n" +
			"sandbox, which is left for you to delete.",
		Sub: []*cli.Command{
			{
				Name:    "create",
				Summary: "Capture a sandbox's world as a new snapshot",
				Usage:   "veris snapshot create [--name N] [--sandbox ID] [--clock-restore today|frozen|rebase] [--keep-external] [--delete-source] [--timeout 1800s] [--request-id ID] [--json]",
				Help: "Tracks a durable capture operation when supported by the API. Reuse --request-id after\n" +
					"an interrupted wait. Terminal failures stop polling and retain the source. Older APIs\n" +
					"use the legacy capture and snapshot-list recovery path.",
				Flags: func(fs *flag.FlagSet) {
					create.bind(fs)
					fs.StringVar(&name, "name", "", "a label for the snapshot (not unique; the newest wins a name lookup)")
					fs.BoolVar(&deleteSource, "delete-source", false, "delete the captured sandbox afterwards (it is left frozen and scrubbed)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return snapshotCreate(ctx, create, name, deleteSource)
				},
			},
			{
				Name:    "list",
				Summary: "The environment's snapshots, newest first",
				Usage:   "veris snapshot list [--json]",
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return snapshotList(ctx)
				},
			},
			{
				Name:    "get",
				Summary: "One snapshot by id or name",
				Usage:   "veris snapshot get ID|NAME [--json]",
				Run: func(ctx *cli.Context, args []string) error {
					ref, err := oneSnapshotRef(ctx, args)
					if err != nil {
						return err
					}
					return snapshotGet(ctx, ref)
				},
			},
			{
				Name:    "delete",
				Summary: "Delete a snapshot and its image",
				Usage:   "veris snapshot delete ID|NAME [--yes]",
				Help:    "Refused (409) while a running sandbox booted from the snapshot: its pod pulls the image again on a restart.",
				Run: func(ctx *cli.Context, args []string) error {
					ref, err := oneSnapshotRef(ctx, args)
					if err != nil {
						return err
					}
					return snapshotDelete(ctx, ref)
				},
			},
		},
	}
}

// oneSnapshotRef is the single ID|NAME a verb takes, or the usage error.
func oneSnapshotRef(ctx *cli.Context, args []string) (string, error) {
	if len(args) != 1 || args[0] == "" {
		return "", fmt.Errorf("%s takes one snapshot id or name (got %q)", strings.Join(ctx.Path[1:], " "), strings.Join(args, " "))
	}
	return args[0], nil
}

// --- call-then-poll ---------------------------------------------------------

// captureCall is one blocking capture as runCapture drives it: the POST,
// sent exactly once, and the read that says afterwards whether it happened.
type captureCall struct {
	// label is the spinner while the POST blocks.
	label string
	// pollFor names what the poll reads, for the "polling …" line.
	pollFor string
	// post sends the capture and keeps its answer; it is never called twice.
	post func(context.Context) error
	// poll reads the control plane and reports whether the capture's result
	// is visible yet.
	poll func(context.Context) (bool, error)
	// check is what to run when the outcome is unknown.
	check string
}

// runCapture sends one capture and, when its answer is lost, polls for its
// outcome. Both captures block on the control plane until the image is
// pushed, and the load balancer drops the connection long before that; the
// POST is never repeated, because a repeat 409s while the first still runs
// and mints a second image once it is done. dropped reports that the answer
// was lost and the result was read back instead. The errors are printed:
// a refusal is exit 1, an outcome still unknown at the poll deadline exit 4.
func runCapture(ctx context.Context, s *session, verb, noun string, timeout time.Duration, call captureCall) (dropped bool, err error) {
	start := time.Now()
	sp := s.ui.Spinner(call.label)
	pctx, cancel := context.WithTimeout(ctx, timeout)
	postErr := call.post(pctx)
	cancel()
	sp.Stop()
	if postErr == nil {
		return false, nil
	}
	if ctx.Err() != nil {
		s.ui.Warn("Stopped waiting; the capture may still be running")
		s.ui.Next(call.check)
		return false, printed(1)
	}
	var operationError *api.CaptureError
	if errors.As(postErr, &operationError) && operationError.State == "unconfirmed" {
		s.ui.Warn("%s", operationError.Error())
		return false, printed(4)
	}
	if !lbDropped(postErr) {
		return false, s.fail(verb, noun, postErr)
	}
	waited := time.Since(start).Round(time.Second)
	if status, _ := describe(postErr); status != 0 {
		s.ui.Warn("The load balancer answered %d after %s; the control plane is still capturing —", status, waited)
	} else {
		s.ui.Warn("No answer after %s; the control plane is still capturing —", waited)
	}
	s.ui.Detail("polling %s", call.pollFor)

	// The poll has the whole budget again: the answer may have been lost at
	// the deadline itself, and a poll with nothing left would report the
	// capture unknown while its row was one read away.
	pollStart := time.Now()
	deadline := pollStart.Add(timeout)
	label := func() string { return fmt.Sprintf("Polling %s  %s", call.pollFor, mmss(time.Since(pollStart))) }
	sp = s.ui.Spinner(label())
	defer sp.Stop()
	for {
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ok, err := call.poll(gctx)
		cancel()
		if err != nil {
			// A refusal (the environment is gone, the key was revoked) ends
			// the poll; a transport blip or a 5xx is the next poll's to retry.
			if status, _ := describe(err); status >= 400 && status < 500 {
				sp.Stop()
				return true, s.fail("confirm", noun, err)
			}
		}
		if ok {
			return true, nil
		}
		if time.Now().After(deadline) {
			sp.Stop()
			s.ui.Warn("Capture unconfirmed after %s; %s", timeout, call.check)
			return true, printed(4)
		}
		select {
		case <-ctx.Done():
			sp.Stop()
			s.ui.Warn("Stopped polling; the capture may still be running")
			s.ui.Next(call.check)
			return true, printed(1)
		case <-time.After(min(capturePollInterval, max(time.Until(deadline), time.Nanosecond))):
		}
		// Off a terminal every Update prints a line; the elapsed time is
		// worth a redraw, not a log entry every few seconds.
		if s.ui.TTY {
			sp.Update(label())
		}
	}
}

// lbDropped reports whether err is the answer being lost rather than the
// control plane refusing. Lost: the read timed out or the connection was
// cut (a transport error after the request went out), or a 5xx with no
// JSON detail, which is the load balancer speaking for a backend that is
// still busy. Refused: any 4xx, and a 5xx that carries a detail -- a 502
// "capture failed", a 503 "snapshots are not available" -- because the
// control plane wrote it and the capture did not happen; polling for it
// would wait out the deadline for nothing. A dial failure is the plane
// unreachable, and a POST that never went out has nothing to poll for.
func lbDropped(err error) bool {
	var ce *api.CaptureError
	if errors.As(err, &ce) {
		return false
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.Status >= 500 && !hasJSONDetail(ae.Body)
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return false
	}
	return true
}

// hasJSONDetail reports whether body is FastAPI's {"detail": …} envelope,
// the shape every answer the control plane writes itself has.
func hasJSONDetail(body []byte) bool {
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	return json.Unmarshal(body, &envelope) == nil && len(envelope.Detail) > 0
}

// captureClient is the session's client without the transport's 30 s
// timeout: a capture blocks for minutes, and the context's deadline is
// the one that bounds it.
func captureClient(s *session) *api.Client {
	c := s.plane()
	c.HTTP = &http.Client{}
	return c
}

// printScrubbed prints what a capture truncated per service, in the doc's
// shape, and the warning about webhook destinations when the capture
// rewrote any: an entry of the form table.column is a callback target the
// scrub late-bound (veris://client/…) or unbound (veris://unbound), which
// a sandbox booted from the image resolves against its own --callback-url.
func printScrubbed(u *ui.UI, scrubbed map[string][]string, keepExternal bool) {
	names := make([]string, 0, len(scrubbed))
	for n := range scrubbed {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		u.Detail("scrubbed: nothing")
		return
	}
	rebound := false
	for _, n := range names {
		u.Detail("scrubbed: %s [%s]", n, strings.Join(scrubbed[n], ", "))
		for _, entry := range scrubbed[n] {
			if strings.Contains(entry, ".") {
				rebound = true
			}
		}
	}
	if !rebound {
		return
	}
	others := "others became veris://unbound"
	if keepExternal {
		others = "external destinations were kept as they were"
	}
	u.Warn("webhook destinations under the curator's callback URL became veris://client/… (late-bound to the next sandbox's --callback-url); %s. Pass --callback-url on every up or those deliveries are blocked.", others)
}

// deleteHint is the command that deletes a sandbox the capture left frozen:
// down when it is this folder's, the explicit form otherwise.
func deleteHint(s *session, id string) string {
	if s.res.Local != nil && s.res.Local.Sandbox != nil && s.res.Local.Sandbox.ID == id {
		return "veris down"
	}
	return "veris sandbox delete --id " + id
}

// sizeText renders a compressed layer size as the doc's "4.2 MB".
func sizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// elapsedText renders how long a capture took as the doc's "3 m 41 s".
func elapsedText(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	switch {
	case secs >= 3600:
		return fmt.Sprintf("%d h %d m", secs/3600, secs%3600/60)
	case secs >= 60:
		return fmt.Sprintf("%d m %d s", secs/60, secs%60)
	}
	return fmt.Sprintf("%d s", secs)
}

// --- snapshot create --------------------------------------------------------

// snapshotCreate captures the sandbox's world as a snapshot of its
// environment, reading the outcome back when the answer is lost, and says
// what to do with the source sandbox the capture left frozen.
func snapshotCreate(ctx *cli.Context, o captureOptions, name string, deleteSource bool) error {
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
	s.ui.Warn("Capturing freezes and scrubs sandbox %s; deploy a fresh one afterwards.", id)

	// A row created before the capture began is an earlier snapshot of the
	// same sandbox, not this one; a minute of slack covers the two clocks
	// disagreeing, and no capture finishes inside it.
	start := time.Now()
	since := start.Add(-time.Minute)
	req := api.CreateSnapshotRequest{SandboxID: id, Name: name, ClockRestore: o.clockRestore, KeepExternalDestinations: o.keepExternal}
	var resp *api.SnapshotResponse
	var found *api.Snapshot
	call := captureCall{
		label:   "Capturing world",
		pollFor: fmt.Sprintf("GET /v1/environments/%s/snapshots for source_sandbox=%s", envID, shortID(id)),
		post: func(ctx context.Context) error {
			var result api.SnapshotResponse
			if supported, err := durableCapture(ctx, s, envID, api.CaptureRequest{SandboxID: id, Kind: "snapshot", RequestID: o.requestID, Name: name, ClockRestore: o.clockRestore, KeepExternalDestinations: o.keepExternal}, &result); supported {
				resp = &result
				return err
			}
			r, err := captureClient(s).CreateSnapshot(ctx, envID, req)
			resp = r
			return err
		},
		poll: func(ctx context.Context) (bool, error) {
			snaps, err := c.ListSnapshots(ctx, envID)
			if err != nil {
				return false, err
			}
			found = newestSnapshotOf(snaps, id, name, since)
			return found != nil, nil
		},
		check: "check veris snapshot list before re-running",
	}
	dropped, err := runCapture(bg, s, "create", "snapshot of sandbox "+id, timeout, call)
	if err != nil {
		if api.IsStatus(err, http.StatusConflict) {
			s.ui.Detail("another capture holds this sandbox; wait for it, then veris snapshot list")
		}
		return err
	}
	sn := found
	if !dropped {
		sn = &resp.Snapshot
	}
	label := ""
	if sn.Name != "" {
		label = fmt.Sprintf(" %q", sn.Name)
	}
	s.ui.Success("Snapshot recorded: %s%s (%s, %s, clock %s, %s)",
		sn.ID, label, sn.RevisionID, sizeText(sn.SizeBytes), sn.ClockRestore, elapsedText(time.Since(start)))
	if dropped {
		s.ui.Detail("scrub details unavailable: the load balancer dropped the response")
	} else {
		printScrubbed(s.ui, resp.Scrubbed, o.keepExternal)
		if !resp.CuratorClockRestored {
			s.ui.Warn("the source sandbox %s could not be handed its clock back; it stays frozen with delivery paused", id)
		}
	}
	// The snapshot exists from here: the link, the hint and the --json body
	// print whatever the source delete does, so a script keeps the id of a
	// capture that happened. The delete's own failure still owns the exit
	// code.
	var deleteErr error
	if deleteSource {
		if deleteErr = deleteSandbox(bg, s, c, sb); deleteErr != nil {
			s.ui.Warn("the source sandbox %s was not deleted; it is frozen and scrubbed: %s", id, deleteHint(s, id))
		}
	} else {
		s.ui.Warn("the source sandbox %s is frozen and scrubbed; delete it: %s", id, deleteHint(s, id))
	}
	studioLink(s.ui, s.consoleURL(), "environments", envID)
	s.ui.Next("veris up --snapshot " + sn.ID)
	if s.ctx.Globals.JSON {
		var body any = resp
		if dropped {
			body = map[string]any{"snapshot": sn, "response_dropped": true}
		}
		if err := printJSON(s.ctx.Stdout, body); err != nil {
			return err
		}
	}
	return deleteErr
}

// newestSnapshotOf is the newest snapshot captured from sandbox id at or
// after since, carrying name when one was asked for, nil when there is
// none yet.
func newestSnapshotOf(snaps []api.Snapshot, id, name string, since time.Time) *api.Snapshot {
	var best *api.Snapshot
	for i := range snaps {
		sn := &snaps[i]
		if sn.SourceSandbox != id || sn.CreatedAt.IsZero() || sn.CreatedAt.Before(since) {
			continue
		}
		if name != "" && sn.Name != name {
			continue
		}
		if best == nil || sn.CreatedAt.After(best.CreatedAt.Time) {
			best = sn
		}
	}
	return best
}

// --- snapshot list / get / delete -------------------------------------------

// listSnapshots is the environment's snapshots newest first, with the
// session and client the verbs go on to use.
func listSnapshots(ctx *cli.Context) (*session, *api.Client, string, string, []api.Snapshot, error) {
	s, err := newSession(ctx, "", "")
	if err != nil {
		return nil, nil, "", "", nil, err
	}
	name, envID, _, err := s.requireEnv()
	if err != nil {
		return nil, nil, "", "", nil, err
	}
	c, err := s.client()
	if err != nil {
		return nil, nil, "", "", nil, err
	}
	snaps, err := c.ListSnapshots(context.Background(), envID)
	if err != nil {
		return nil, nil, "", "", nil, s.fail("list", "snapshots of '"+name+"'", err)
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt.Time) })
	return s, c, name, envID, snaps, nil
}

func snapshotList(ctx *cli.Context) error {
	s, _, name, _, snaps, err := listSnapshots(ctx)
	if err != nil {
		return err
	}
	if s.ctx.Globals.JSON {
		if snaps == nil {
			snaps = []api.Snapshot{}
		}
		return printJSON(s.ctx.Stdout, snaps)
	}
	if len(snaps) == 0 {
		s.ui.Info("No snapshots of '%s'", name)
		s.ui.Next("veris snapshot create --name NAME")
		return nil
	}
	rows := make([][]string, 0, len(snaps))
	for _, sn := range snaps {
		rows = append(rows, []string{"  " + sn.ID, dashIfBlank(sn.Name), sn.RevisionID, sn.ClockRestore,
			sizeText(sn.SizeBytes), shortID(sn.SourceSandbox), sn.CreatedAt.Local().Format("2006-01-02 15:04")})
	}
	s.ui.Table([]string{"  ID", "Name", "Revision", "Clock", "Size", "Source", "Created"}, rows)
	return nil
}

func dashIfBlank(v string) string {
	if v == "" {
		return "—"
	}
	return v
}

func snapshotGet(ctx *cli.Context, ref string) error {
	s, _, name, _, snaps, err := listSnapshots(ctx)
	if err != nil {
		return err
	}
	sn, err := snapshotByRef(s, snaps, ref, name)
	if err != nil {
		return err
	}
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, sn)
	}
	s.ui.Info("Snapshot %s", sn.ID)
	s.ui.Info("Name:        %s", dashIfBlank(sn.Name))
	s.ui.Info("Environment: %s", envLabel(s, sn.EnvironmentID))
	s.ui.Info("Revision:    %s", sn.RevisionID)
	s.ui.Info("Image:       %s", sn.Image)
	s.ui.Info("Clock:       %s", sn.ClockRestore)
	s.ui.Info("Size:        %s", sizeText(sn.SizeBytes))
	s.ui.Info("Source:      sandbox %s", sn.SourceSandbox)
	s.ui.Info("Created:     %s", stampOf(sn.CreatedAt))
	s.ui.Next("veris up --snapshot " + sn.ID)
	return nil
}

func snapshotDelete(ctx *cli.Context, ref string) error {
	s, c, name, envID, snaps, err := listSnapshots(ctx)
	if err != nil {
		return err
	}
	sn, err := snapshotByRef(s, snaps, ref, name)
	if err != nil {
		return err
	}
	label := ""
	if sn.Name != "" {
		label = fmt.Sprintf(" %q", sn.Name)
	}
	if err := confirm(s.ui, fmt.Sprintf("Delete snapshot %s%s (%s, %s)?", sn.ID, label, sn.RevisionID, sizeText(sn.SizeBytes))); err != nil {
		return err
	}
	err = c.DeleteSnapshot(context.Background(), envID, sn.ID)
	if api.IsStatus(err, http.StatusNotFound) {
		s.ui.Warn("Snapshot %s was already gone", sn.ID)
		return nil
	}
	if err != nil {
		return s.fail("delete", "snapshot "+sn.ID, err)
	}
	s.ui.Success("Snapshot deleted: %s", sn.ID)
	return nil
}

// snapshotByRef is the snapshot ref names: by id when it is shaped like
// one, else by name, where several sharing it yield the newest, said
// aloud. Not found is the printed error (exit 1) naming what there is.
func snapshotByRef(s *session, snaps []api.Snapshot, ref, envName string) (*api.Snapshot, error) {
	if looksLikeID(ref) {
		for i := range snaps {
			if snaps[i].ID == ref {
				return &snaps[i], nil
			}
		}
		s.ui.Fail("No snapshot %s in environment '%s'", ref, envName)
		s.ui.Next("veris snapshot list")
		return nil, printed(1)
	}
	var matches []*api.Snapshot
	for i := range snaps {
		if snaps[i].Name == ref {
			matches = append(matches, &snaps[i])
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
		s.ui.Fail("No snapshot named '%s' in environment '%s' (have: %s)", ref, envName, have)
		s.ui.Next("veris snapshot list")
		return nil, printed(1)
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].CreatedAt.After(matches[j].CreatedAt.Time) })
	if len(matches) > 1 {
		s.ui.Warn("%d snapshots are named '%s'; using the newest, %s (%s)",
			len(matches), ref, matches[0].ID, matches[0].CreatedAt.Local().Format("2006-01-02 15:04"))
	}
	return matches[0], nil
}
