package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/veris-ai/veris-cli/internal/api"
)

// The default is one of the environment's snapshots: the list and a get say
// which, baseline get names it, and a promote can label the snapshot it
// records and says which it recorded.

func TestSnapshotListMarksTheDefault(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	snaps := twoSnapshots()
	snaps[1].IsDefault = true // the newer one, served second
	p.script(func(p *capturePlane) { p.snapshots = snaps })

	code, stdout, stderr := runSandboxCLI(t, "snapshot", "list")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d stdout %q\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr, "Created", "Default", snapNewID, "yes", snapOldID, "—")

	code, stdout, _ = runSandboxCLI(t, "snapshot", "list", "--json")
	var list []api.Snapshot
	if code != 0 || json.Unmarshal([]byte(stdout), &list) != nil || len(list) != 2 {
		t.Fatalf("--json: exit %d stdout %q", code, stdout)
	}
	if !list[0].IsDefault || list[1].IsDefault {
		t.Errorf("--json keeps is_default: %+v", list)
	}

	code, _, stderr = runSandboxCLI(t, "snapshot", "get", snapNewID)
	if code != 0 || !strings.Contains(stderr, "Default:     yes") {
		t.Errorf("get the default: exit %d\n%s", code, stderr)
	}
	code, _, stderr = runSandboxCLI(t, "snapshot", "get", snapOldID)
	if code != 0 || !strings.Contains(stderr, "Default:     no") {
		t.Errorf("get another: exit %d\n%s", code, stderr)
	}
}

func TestBaselineGetNamesTheDefaultSnapshot(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	pin := oldBaseline()
	pin.SnapshotID = snapOldID
	p.script(func(p *capturePlane) { p.envs[ciID].Baseline = pin })

	code, _, stderr := runSandboxCLI(t, "baseline", "get")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "Revision:  "+wrldOld, "Snapshot:  "+snapOldID, "Image:     "+imageOld)

	// A control plane that links no snapshot still reads cleanly.
	p.script(func(p *capturePlane) { p.envs[ciID].Baseline = oldBaseline() })
	code, _, stderr = runSandboxCLI(t, "baseline", "get")
	if code != 0 || !strings.Contains(stderr, "Snapshot:  —") {
		t.Errorf("unlinked pin: exit %d\n%s", code, stderr)
	}
}

func promotedSnapshot() api.Snapshot {
	snap := newSnapshot("bank-world-v2")
	snap.RevisionID, snap.Image, snap.IsDefault = wrldNew, imageNew, true
	return snap
}

func TestBaselinePromoteNamesTheSnapshotItRecords(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.promote = func(p *capturePlane, body api.PromoteRequest) (int, any) {
			pin := newBaseline()
			pin.SnapshotID = snapNewID
			p.envs[ciID].Baseline = &pin
			snap := promotedSnapshot()
			return 200, api.PromoteResponse{EnvironmentID: ciID, SandboxID: sbID, Baseline: pin,
				ClockRestore: api.ClockToday, SizeBytes: 4508876, CuratorClockRestored: true, Snapshot: &snap}
		}
	})

	code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--name", "bank-world-v2")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if bodies := p.promoteBodies(); len(bodies) != 1 || bodies[0].Name != "bank-world-v2" {
		t.Errorf("promote bodies = %+v", bodies)
	}
	sbInOrder(t, stderr, "✓ Baseline pinned: "+wrldNew, "  "+imageNew, "  snapshot "+snapNewID)
}

func TestDurablePromoteCarriesTheSnapshotName(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	var sent api.CaptureRequest
	p.operation = func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Error(err)
		}
		pin := newBaseline()
		pin.SnapshotID = snapNewID
		snap := promotedSnapshot()
		sbJSON(w, 202, map[string]any{"id": "cap-named", "status": "succeeded", "phase": "complete",
			"result": api.PromoteResponse{EnvironmentID: ciID, SandboxID: sbID, Baseline: pin,
				ClockRestore: api.ClockToday, SizeBytes: 4508876, CuratorClockRestored: true, Snapshot: &snap}})
	}

	code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--name", "bank-world-v2")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if sent.Kind != "promote" || sent.Name != "bank-world-v2" {
		t.Errorf("capture request = %+v", sent)
	}
	if !strings.Contains(stderr, "  snapshot "+snapNewID) {
		t.Errorf("the recorded snapshot is named:\n%s", stderr)
	}
}
