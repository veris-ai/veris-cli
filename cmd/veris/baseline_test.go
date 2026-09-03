package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
)

const (
	wrldOld = "wrld-" + snapOldID
	wrldNew = "wrld-" + snapNewID
)

// oldBaseline is a pin an earlier promote of another sandbox left on ci.
func oldBaseline() *api.EnvironmentBaseline {
	return &api.EnvironmentBaseline{Image: imageOld, RevisionID: wrldOld,
		PromotedAt: at(time.Date(2026, 8, 28, 16, 5, 0, 0, time.UTC)), SourceSandbox: otherSbID}
}

// newBaseline is what the promote under test pins.
func newBaseline() api.EnvironmentBaseline {
	return api.EnvironmentBaseline{Image: imageNew, RevisionID: wrldNew, PromotedAt: at(time.Now()), SourceSandbox: sbID}
}

func TestBaselineGet(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)

	code, stdout, stderr := runSandboxCLI(t, "baseline", "get")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d stdout %q\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "Baseline of ci ("+shortID(ciID)+"): bundle (no baseline pinned)", "→ Next: veris baseline promote")
	code, stdout, _ = runSandboxCLI(t, "baseline", "get", "--json")
	if code != 0 || strings.TrimSpace(stdout) != "null" {
		t.Errorf("--json with no pin: exit %d stdout %q", code, stdout)
	}

	p.script(func(p *capturePlane) { p.envs[ciID].Baseline = oldBaseline() })
	code, _, stderr = runSandboxCLI(t, "baseline", "get")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "Baseline of ci ("+shortID(ciID)+")", "Revision:  "+wrldOld, "Image:     "+imageOld,
		"Promoted:  2026-08-2", "Source:    sandbox "+otherSbID, "→ Next: veris up")
	code, stdout, _ = runSandboxCLI(t, "baseline", "get", "--json")
	var b api.EnvironmentBaseline
	if code != 0 || json.Unmarshal([]byte(stdout), &b) != nil || b.RevisionID != wrldOld {
		t.Errorf("--json: exit %d stdout %q", code, stdout)
	}
}

func TestBaselinePromotePinsDeletesAndRecords(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) {
			pin := newBaseline()
			p.envs[ciID].Baseline = &pin
			return 200, api.PromoteResponse{EnvironmentID: ciID, SandboxID: sbID, Baseline: pin,
				ClockRestore: body.ClockRestore, SizeBytes: 4508876, CuratorClockRestored: true,
				Scrubbed: map[string][]string{"stripe": {"deliveries", "_veris_requests", "webhook_endpoints.url"}, "postgres": {}}}
		}
	})
	code, stdout, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--clock-restore", "frozen", "--keep-external", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	bodies := p.promoteBodies()
	if len(bodies) != 1 || bodies[0] != (api.PromoteRequest{ClockRestore: api.ClockFrozen, KeepExternalDestinations: true}) {
		t.Errorf("promote bodies = %+v", bodies)
	}
	sbInOrder(t, stderr,
		"Pin sandbox "+sbID+" as the baseline of 'ci' (current: none)? Every future up of this environment boots from it. y",
		"Promoting (freeze → capture → push; polling GET /v1/environments/"+shortID(ciID)+" if the load balancer times out)",
		"✓ Baseline pinned: "+wrldNew+" (4.3 MB, clock_restore frozen, promoted ",
		", curator clock restored)",
		"  "+imageNew,
		"  scrubbed: postgres []",
		"  scrubbed: stripe [deliveries, _veris_requests, webhook_endpoints.url]",
		"! webhook destinations under the curator's callback URL became veris://client/…",
		"✓ Sandbox deleted: "+sbID,
		"→ https://studio.example/environments/"+ciID,
		"→ Next: veris up")
	if strings.Contains(stderr, "veris down &&") {
		t.Errorf("the source is gone and the pointer forgotten; `veris down` would fail:\n%s", stderr)
	}
	if got := p.sandboxDeletes(); len(got) != 1 || got[0] != ciID+"/"+sbID {
		t.Errorf("source deletes = %v", got)
	}
	if ptr := sbPointer(t, b); ptr != nil {
		t.Errorf("deleting the source must forget the pointer; got %+v", ptr)
	}
	ledger := loadLedger(t, b)
	if len(ledger) != 1 {
		t.Fatalf("ledger = %+v, want one entry", ledger)
	}
	if r := ledger[0]; r.EnvironmentID != ciID || r.Revision != wrldNew || r.Image != imageNew || r.SourceSandbox != sbID || r.PromotedAt == "" {
		t.Errorf("ledger entry = %+v", r)
	}
	var resp api.PromoteResponse
	if json.Unmarshal([]byte(stdout), &resp) != nil || resp.Baseline.RevisionID != wrldNew || resp.SizeBytes != 4508876 {
		t.Errorf("--json stdout = %q", stdout)
	}
	if p.listCount() != 0 {
		t.Errorf("promote never lists snapshots; lists = %d", p.listCount())
	}
}

func TestBaselinePromoteKeepsTheSourceWhenAsked(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.envs[ciID].Baseline = oldBaseline()
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) {
			pin := newBaseline()
			p.envs[ciID].Baseline = &pin
			return 200, api.PromoteResponse{EnvironmentID: ciID, SandboxID: sbID, Baseline: pin,
				ClockRestore: api.ClockToday, SizeBytes: 4508876, CuratorClockRestored: false,
				Scrubbed: map[string][]string{"stripe": {"deliveries"}}}
		}
	})
	code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--keep-source")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"Pin sandbox "+sbID+" as the baseline of 'ci' (current: "+wrldOld+")?",
		"✓ Baseline pinned: "+wrldNew+" (4.3 MB, clock_restore today, promoted ",
		"  scrubbed: stripe [deliveries]",
		"! the source sandbox "+sbID+" could not be handed its clock back; it stays frozen with delivery paused",
		"! the source sandbox "+sbID+" is frozen and scrubbed; delete it: veris down",
		"→ Next: veris down && veris up")
	if strings.Contains(stderr, "curator clock restored") || strings.Contains(stderr, "webhook destinations") {
		t.Errorf("a clock left frozen and no rebound destinations must not read otherwise:\n%s", stderr)
	}
	if len(p.sandboxDeletes()) != 0 {
		t.Errorf("--keep-source deleted %v", p.sandboxDeletes())
	}
	if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
		t.Errorf("--keep-source must keep the pointer; got %+v", ptr)
	}
	if ledger := loadLedger(t, b); len(ledger) != 1 || ledger[0].Revision != wrldNew {
		t.Errorf("ledger = %+v", ledger)
	}
}

func TestBaselinePromotePollsWhenTheLoadBalancerDrops(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.envs[ciID].Baseline = oldBaseline()
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) {
			return 502, rawBody("<html><body>502 Bad Gateway</body></html>")
		}
		// The first read is promote's own look at the current pin; the pin
		// moves on the third, so one poll sees the old world first.
		p.onEnvRead = func(p *capturePlane, n int) {
			if n == 3 {
				pin := newBaseline()
				p.envs[ciID].Baseline = &pin
			}
		}
	})
	code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if n := len(p.promoteBodies()); n != 1 {
		t.Errorf("the promote was sent %d times; a dropped answer must never be re-sent", n)
	}
	sbInOrder(t, stderr,
		"! The load balancer answered 502 after ",
		"  polling GET /v1/environments/"+ciID+" for baseline.source_sandbox="+shortID(sbID),
		"✓ Baseline pinned: "+wrldNew+" (clock_restore today, promoted ",
		"  "+imageNew,
		"  scrub details unavailable: the load balancer dropped the response",
		"✓ Sandbox deleted: "+sbID,
		"→ Next: veris up")
	if strings.Contains(stderr, wrldOld+" (") {
		t.Errorf("the previous pin was taken for the new one:\n%s", stderr)
	}
	if ledger := loadLedger(t, b); len(ledger) != 1 || ledger[0].Revision != wrldNew || ledger[0].SourceSandbox != sbID {
		t.Errorf("ledger = %+v", ledger)
	}
}

func TestBaselinePromotePollsAfterAReadTimeout(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.hold = 5 * time.Second
		p.envs[ciID].Baseline = oldBaseline()
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) {
			t.Error("the held answer must never be written: the client should have left")
			return 500, nil
		}
		// The first read is promote's own look at the current pin; the pin
		// moves on the second, the poll's first.
		p.onEnvRead = func(p *capturePlane, n int) {
			if n == 2 {
				pin := newBaseline()
				p.envs[ciID].Baseline = &pin
			}
		}
	})
	code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--timeout", "150ms")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if n := len(p.promoteBodies()); n != 1 {
		t.Errorf("the promote was sent %d times; a timed-out answer must never be re-sent", n)
	}
	sbInOrder(t, stderr,
		"! No answer after ",
		"; the control plane is still capturing —",
		"  polling GET /v1/environments/"+ciID+" for baseline.source_sandbox="+shortID(sbID),
		"✓ Baseline pinned: "+wrldNew+" (clock_restore today, promoted ",
		"✓ Sandbox deleted: "+sbID)
	if ledger := loadLedger(t, b); len(ledger) != 1 || ledger[0].Revision != wrldNew {
		t.Errorf("ledger = %+v", ledger)
	}
}

// The pin is real once the control plane confirms it, whatever happens to
// the source afterwards: the ledger entry, the hint and the --json body
// must not depend on the DELETE, which only owns the exit code.
func TestBaselinePromoteRecordsThePinBeforeADeleteThatFails(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.deleteSandboxStatus = 503
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) {
			pin := newBaseline()
			p.envs[ciID].Baseline = &pin
			return 200, api.PromoteResponse{EnvironmentID: ciID, SandboxID: sbID, Baseline: pin,
				ClockRestore: api.ClockToday, SizeBytes: 4508876, CuratorClockRestored: true}
		}
	})
	code, stdout, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1 for the failed delete\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"✓ Baseline pinned: "+wrldNew,
		"✗ Failed to delete sandbox "+sbID+": [503]",
		"! the source sandbox "+sbID+" was not deleted; it is frozen and scrubbed: veris down",
		"→ https://studio.example/environments/"+ciID,
		"→ Next: veris down && veris up")
	if ledger := loadLedger(t, b); len(ledger) != 1 || ledger[0].Revision != wrldNew {
		t.Errorf("the pin must be recorded before the delete: ledger = %+v", ledger)
	}
	var resp api.PromoteResponse
	if json.Unmarshal([]byte(stdout), &resp) != nil || resp.Baseline.RevisionID != wrldNew {
		t.Errorf("--json must still carry the pin: stdout = %q", stdout)
	}
	if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
		t.Errorf("a source that was not deleted keeps the pointer; got %+v", ptr)
	}
}

func TestBaselinePromoteExits4WhenThePinNeverMoves(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.envs[ciID].Baseline = oldBaseline()
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) { return 504, rawBody("timeout") }
	})
	code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--timeout", "60ms")
	if code != 4 {
		t.Fatalf("exit %d, want 4\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "! Capture unconfirmed after 60ms; check veris baseline get before re-running") {
		t.Errorf("missing the exit-4 line:\n%s", stderr)
	}
	if len(p.sandboxDeletes()) != 0 || len(loadLedger(t, b)) != 0 {
		t.Errorf("an unconfirmed promote must neither delete the source nor write the ledger: %v %v", p.sandboxDeletes(), loadLedger(t, b))
	}
}

func TestBaselinePromoteRefusalsAndPrompts(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) {
			return 409, map[string]string{"detail": "another promote is capturing sandbox " + sbID}
		}
	})
	code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes")
	if code != 1 || !strings.Contains(stderr, "✗ Failed to promote sandbox "+sbID+": [409] another promote is capturing sandbox "+sbID) {
		t.Errorf("409: exit %d\n%s", code, stderr)
	}
	if n := len(p.promoteBodies()); n != 1 {
		t.Errorf("promotes = %d, want one", n)
	}
	if len(p.sandboxDeletes()) != 0 || len(loadLedger(t, b)) != 0 {
		t.Errorf("a refused promote must neither delete nor record: %v %v", p.sandboxDeletes(), loadLedger(t, b))
	}

	code, _, stderr = runSandboxCLI(t, "baseline", "promote")
	if code != 1 || !strings.Contains(stderr, "Interactive prompt requires a TTY. Pass --yes") {
		t.Errorf("off a TTY without --yes: exit %d\n%s", code, stderr)
	}
	code, _, stderr = runSandboxCLI(t, "baseline", "promote", "--yes", "--clock-restore", "never")
	if code != 1 || !strings.Contains(stderr, "✗ --clock-restore must be today, frozen or rebase (got 'never')") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	if n := len(p.promoteBodies()); n != 1 {
		t.Errorf("promotes = %d after the refused prompts, want still one", n)
	}
}

func TestBaselineSetRepointsThePin(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.envs[ciID].Baseline = oldBaseline()
		p.snapshots = twoSnapshots()
	})
	code, _, stderr := runSandboxCLI(t, "baseline", "set", "empty-stripe", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"Repoint ci's baseline to snapshot empty-stripe (snap-"+snapOldID+", …@sha256:41aa…)? Running sandboxes are unaffected. y",
		"✓ Baseline now snap-"+snapDupID+" (was "+wrldOld+")",
		"  "+imageOld,
		"→ Next: veris down && veris up")
	resets := p.resetBodies()
	if len(resets) != 1 || resets[0].BaselineImage == nil || *resets[0].BaselineImage != imageOld {
		t.Errorf("reset bodies = %+v", resets)
	}
	ledger := loadLedger(t, b)
	if len(ledger) != 1 || ledger[0].Revision != "snap-"+snapDupID || ledger[0].Image != imageOld ||
		ledger[0].SourceSandbox != otherSbID || ledger[0].PromotedAt != "2026-08-28T16:05:00Z" {
		t.Errorf("ledger = %+v", ledger)
	}

	code, stdout, stderr := runSandboxCLI(t, "baseline", "set", imageNew, "--yes", "--json")
	if code != 0 {
		t.Fatalf("digest: exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "Repoint ci's baseline to "+imageNew+"? Running sandboxes are unaffected. y", "✓ Baseline now snap-"+snapDupID+" (was snap-"+snapDupID+")")
	if resets = p.resetBodies(); len(resets) != 2 || *resets[1].BaselineImage != imageNew {
		t.Errorf("reset bodies = %+v", resets)
	}
	var env api.Environment
	if json.Unmarshal([]byte(stdout), &env) != nil || env.Baseline == nil || env.Baseline.Image != imageNew {
		t.Errorf("--json stdout = %q", stdout)
	}
	if ledger = loadLedger(t, b); len(ledger) != 2 || ledger[1].Image != imageNew {
		t.Errorf("ledger = %+v", ledger)
	}

	code, _, stderr = runSandboxCLI(t, "baseline", "set", "nope", "--yes")
	if code != 1 || !strings.Contains(stderr, "✗ No snapshot named 'nope' in environment 'ci'") {
		t.Errorf("unknown name: exit %d\n%s", code, stderr)
	}
	p.script(func(p *capturePlane) {
		p.resetStatus, p.resetDetail = 422, "baseline_image must be a digest ref under europe-west1-docker.pkg.dev/veris/env-images/env-"+ciID+"@sha256:..."
	})
	code, _, stderr = runSandboxCLI(t, "baseline", "set", "other@sha256:0000", "--yes")
	if code != 1 || !strings.Contains(stderr, "✗ Failed to set baseline of 'ci': [422] baseline_image must be a digest ref under") {
		t.Errorf("422: exit %d\n%s", code, stderr)
	}
	if n := len(p.resetBodies()); n != 3 {
		t.Errorf("resets = %d, want three (the unknown name sends none)", n)
	}
	code, _, stderr = runSandboxCLI(t, "baseline", "set")
	if code != 1 || !strings.Contains(stderr, "baseline set takes one snapshot id, name or image digest") {
		t.Errorf("no arg: exit %d\n%s", code, stderr)
	}
}

func TestBaselineClear(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	code, _, stderr := runSandboxCLI(t, "baseline", "clear", "--yes")
	if code != 0 || !strings.Contains(stderr, "! 'ci' has no baseline pinned; sandboxes already boot the packaged bundle") {
		t.Errorf("nothing pinned: exit %d\n%s", code, stderr)
	}
	if len(p.resetBodies()) != 0 {
		t.Errorf("nothing pinned must send no reset: %+v", p.resetBodies())
	}
	// Under --json the environment still prints, with its null baseline.
	code, stdout, stderr := runSandboxCLI(t, "baseline", "clear", "--yes", "--json")
	var unpinned api.Environment
	if code != 0 || json.Unmarshal([]byte(stdout), &unpinned) != nil || unpinned.ID != ciID || unpinned.Baseline != nil {
		t.Errorf("nothing pinned under --json: exit %d stdout %q\n%s", code, stdout, stderr)
	}

	p.script(func(p *capturePlane) { p.envs[ciID].Baseline = oldBaseline() })
	code, _, stderr = runSandboxCLI(t, "baseline", "clear")
	if code != 1 || !strings.Contains(stderr, "Interactive prompt requires a TTY. Pass --yes") {
		t.Errorf("off a TTY without --yes: exit %d\n%s", code, stderr)
	}
	code, stdout, stderr = runSandboxCLI(t, "baseline", "clear", "--yes", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"Clear ci's baseline (current: "+wrldOld+")? Sandboxes will boot the packaged bundle; running ones are unaffected. y",
		"✓ Baseline cleared; sandboxes boot the packaged bundle",
		"→ Next: veris down && veris up")
	resets := p.resetBodies()
	if len(resets) != 1 || resets[0].BaselineImage != nil {
		t.Errorf("reset bodies = %+v, want one null", resets)
	}
	var env api.Environment
	if json.Unmarshal([]byte(stdout), &env) != nil || env.Baseline != nil {
		t.Errorf("--json stdout = %q", stdout)
	}
}

func TestBaselineListIsTheLocalLedger(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	code, _, stderr := runSandboxCLI(t, "baseline", "list")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "Baseline ledger (this machine's record; the platform keeps no promote history)",
		"No baselines recorded on this machine", "→ Next: veris baseline promote")

	b.local(cfg.Local{Baselines: []cfg.BaselineRef{
		{EnvironmentID: devID, Revision: wrldOld, Image: imageOld, PromotedAt: "2026-08-28T12:00:00Z", SourceSandbox: otherSbID},
		{EnvironmentID: ciID, Revision: wrldNew, Image: imageNew, PromotedAt: "2026-09-02T12:00:00Z", SourceSandbox: sbID},
	}})
	code, stdout, stderr := runSandboxCLI(t, "baseline", "list")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d stdout %q\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "Baseline ledger (this machine's record; the platform keeps no promote history)",
		"Environment", "Revision", "Source", "Promoted", "Image",
		"  ci", wrldNew, shortID(sbID), "2026-09-0", imageNew,
		"  dev", wrldOld, shortID(otherSbID), "2026-08-2", imageOld)
	code, stdout, _ = runSandboxCLI(t, "baseline", "list", "--json")
	var refs []cfg.BaselineRef
	if code != 0 || json.Unmarshal([]byte(stdout), &refs) != nil || len(refs) != 2 {
		t.Errorf("--json: exit %d stdout %q", code, stdout)
	}
}

func TestBaselineListNeedsAProject(t *testing.T) {
	p := newCapturePlane(t)
	sandboxBench(t, p.srv.URL)
	code, _, stderr := runSandboxCLI(t, "baseline", "list")
	if code != 1 || !strings.Contains(stderr, "✗ No .veris/twin.yaml found") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
}
