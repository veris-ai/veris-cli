package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
)

const (
	snapOldID = "a1b2c3d4e5f6g7h8i9j0k1l2m"
	snapNewID = "c9d2f4h6j8k1m3n5p7q9r2s4t"
	snapDupID = "7h4j2l9n6p1r8t3v0x5z2b7d4"
	imageOld  = "europe-west1-docker.pkg.dev/veris/env-images/env-" + ciID + "@sha256:41aa000000000000000000000000000000000000000000000000000000000000"
	imageNew  = "europe-west1-docker.pkg.dev/veris/env-images/env-" + ciID + "@sha256:9b1e000000000000000000000000000000000000000000000000000000000000"
)

// rawBody is an answer written as it is, with no JSON encoding: what the
// load balancer sends when it gives up on a backend.
type rawBody string

// capturePlane is a control plane for the snapshot and baseline verbs: the
// environments and sandboxes it knows, the snapshot list a test grows
// between polls, and a record of every write. The POST handlers can hold
// their answer so a client deadline passes first, the way the load
// balancer's does.
type capturePlane struct {
	srv *httptest.Server
	mu  sync.Mutex

	envs      map[string]*api.Environment
	sandboxes map[string]*api.Sandbox
	snapshots []api.Snapshot

	lists  int                          // GET …/snapshots calls
	onList func(p *capturePlane, n int) // runs under the lock before the nth list answers

	envReads  int
	onEnvRead func(p *capturePlane, n int)

	hold    time.Duration // POST snapshot/promote hold their answer this long (or until the client leaves)
	create  func(p *capturePlane, body api.CreateSnapshotRequest) (int, any)
	creates []api.CreateSnapshotRequest

	promote  func(p *capturePlane, body api.PromoteRequest) (int, any)
	promotes []api.PromoteRequest

	operation func(http.ResponseWriter, *http.Request)

	resets      []api.ResetEnvironmentRequest
	resetStatus int    // 0 → 200 with the environment as reset
	resetDetail string // the 422's detail

	deletedSnapshots     []string
	deleteSnapshotStatus int // 0 → 204
	deletedSandboxes     []string
	deleteSandboxStatus  int // 0 → 204 and the sandbox is gone
}

func newCapturePlane(t *testing.T) *capturePlane {
	t.Helper()
	p := &capturePlane{
		envs: map[string]*api.Environment{
			devID: {ID: devID, Name: "checkout-svc", Services: []string{"stripe", "postgres"}},
			ciID:  {ID: ciID, Name: "checkout-ci", Services: []string{"stripe", "postgres"}},
		},
		sandboxes: map[string]*api.Sandbox{
			sbID: {ID: sbID, EnvironmentID: ciID, Status: api.StatusReady,
				CreatedAt: at(time.Now().Add(-time.Hour)), ExpiresAt: at(time.Now().Add(time.Hour))},
		},
	}
	answer := func(w http.ResponseWriter, status int, body any) {
		if raw, ok := body.(rawBody); ok {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(raw))
			return
		}
		sbJSON(w, status, body)
	}
	// hold waits out the plane's hold, and reports false when the client
	// gave up first, in which case nothing is written: the connection is
	// what the load balancer would have cut.
	hold := func(r *http.Request) bool {
		p.mu.Lock()
		d := p.hold
		p.mu.Unlock()
		if d <= 0 {
			return true
		}
		select {
		case <-time.After(d):
			return true
		case <-r.Context().Done():
			return false
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/environments/{id}/capture-operations", func(w http.ResponseWriter, r *http.Request) {
		if p.operation != nil {
			p.operation(w, r)
			return
		}
		sbJSON(w, 404, map[string]string{"detail": "Not Found"})
	})
	mux.HandleFunc("GET /v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.envReads++
		if p.onEnvRead != nil {
			p.onEnvRead(p, p.envReads)
		}
		env, ok := p.envs[r.PathValue("id")]
		if !ok {
			sbJSON(w, 404, map[string]string{"detail": "environment " + r.PathValue("id") + " not found"})
			return
		}
		sbJSON(w, 200, env)
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		sb, ok := p.sandboxes[r.PathValue("id")]
		if !ok {
			sbJSON(w, 404, map[string]string{"detail": "sandbox " + r.PathValue("id") + " not found"})
			return
		}
		sbJSON(w, 200, sb)
	})
	mux.HandleFunc("GET /v1/environments/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.lists++
		if p.onList != nil {
			p.onList(p, p.lists)
		}
		var out []api.Snapshot
		for _, sn := range p.snapshots {
			if sn.EnvironmentID == r.PathValue("id") {
				out = append(out, sn)
			}
		}
		if out == nil {
			out = []api.Snapshot{}
		}
		sbJSON(w, 200, out)
	})
	mux.HandleFunc("POST /v1/environments/{id}/snapshots", func(w http.ResponseWriter, r *http.Request) {
		var body api.CreateSnapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sbJSON(w, 422, map[string]string{"detail": err.Error()})
			return
		}
		p.mu.Lock()
		p.creates = append(p.creates, body)
		p.mu.Unlock()
		if !hold(r) {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.create == nil {
			sbJSON(w, 500, map[string]string{"detail": "no create scripted"})
			return
		}
		status, out := p.create(p, body)
		answer(w, status, out)
	})
	mux.HandleFunc("DELETE /v1/environments/{env}/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.deletedSnapshots = append(p.deletedSnapshots, r.PathValue("env")+"/"+r.PathValue("id"))
		switch p.deleteSnapshotStatus {
		case 0:
			w.WriteHeader(http.StatusNoContent)
		case 404:
			sbJSON(w, 404, map[string]string{"detail": "snapshot " + r.PathValue("id") + " not found"})
		default:
			sbJSON(w, p.deleteSnapshotStatus, map[string]string{"detail": "snapshot " + r.PathValue("id") + " is booted by sandbox " + otherSbID})
		}
	})
	mux.HandleFunc("POST /v1/environments/{env}/sandboxes/{id}/promote", func(w http.ResponseWriter, r *http.Request) {
		var body api.PromoteRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sbJSON(w, 422, map[string]string{"detail": err.Error()})
			return
		}
		p.mu.Lock()
		p.promotes = append(p.promotes, body)
		p.mu.Unlock()
		if !hold(r) {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.promote == nil {
			sbJSON(w, 500, map[string]string{"detail": "no promote scripted"})
			return
		}
		status, out := p.promote(p, body)
		answer(w, status, out)
	})
	mux.HandleFunc("DELETE /v1/environments/{env}/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.deletedSandboxes = append(p.deletedSandboxes, r.PathValue("env")+"/"+r.PathValue("id"))
		if p.deleteSandboxStatus != 0 {
			sbJSON(w, p.deleteSandboxStatus, map[string]string{"detail": "the sandbox controller is unavailable"})
			return
		}
		delete(p.sandboxes, r.PathValue("id"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/environments/{id}/reset", func(w http.ResponseWriter, r *http.Request) {
		var body api.ResetEnvironmentRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			sbJSON(w, 422, map[string]string{"detail": err.Error()})
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		p.resets = append(p.resets, body)
		if p.resetStatus != 0 {
			sbJSON(w, p.resetStatus, map[string]string{"detail": p.resetDetail})
			return
		}
		env := p.envs[r.PathValue("id")]
		if body.BaselineImage == nil {
			env.Baseline = nil
		} else {
			env.Baseline = &api.EnvironmentBaseline{Image: *body.BaselineImage, RevisionID: "snap-" + snapDupID,
				PromotedAt: at(time.Date(2026, 8, 28, 16, 5, 0, 0, time.UTC)), SourceSandbox: otherSbID}
		}
		sbJSON(w, 200, env)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		sbJSON(w, 404, map[string]string{"detail": "Not Found"})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *capturePlane) script(fn func(p *capturePlane)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

func (p *capturePlane) createBodies() []api.CreateSnapshotRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]api.CreateSnapshotRequest(nil), p.creates...)
}

func (p *capturePlane) promoteBodies() []api.PromoteRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]api.PromoteRequest(nil), p.promotes...)
}

func (p *capturePlane) listCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lists
}

func (p *capturePlane) sandboxDeletes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deletedSandboxes...)
}

func (p *capturePlane) snapshotDeletes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deletedSnapshots...)
}

func (p *capturePlane) resetBodies() []api.ResetEnvironmentRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]api.ResetEnvironmentRequest(nil), p.resets...)
}

// oldSnapshot is an earlier capture of sbID in ci, an hour old: a row a
// poll must not mistake for the capture it is waiting on.
func oldSnapshot() api.Snapshot {
	return api.Snapshot{ID: snapOldID, EnvironmentID: ciID, Name: "empty-stripe", Image: imageOld,
		RevisionID: "snap-" + snapOldID, CreatedAt: at(time.Now().Add(-time.Hour)), SourceSandbox: sbID,
		ClockRestore: api.ClockToday, SizeBytes: 1153433}
}

// newSnapshot is the capture under test as the control plane records it.
func newSnapshot(name string) api.Snapshot {
	return api.Snapshot{ID: snapNewID, EnvironmentID: ciID, Name: name, Image: imageNew,
		RevisionID: "snap-" + snapNewID, CreatedAt: at(time.Now()), SourceSandbox: sbID,
		ClockRestore: api.ClockToday, SizeBytes: 4404019}
}

// captureBench is a sandboxBench pointed at the capture plane, with the
// project's ci environment in use, sbID as the folder's sandbox, and the
// poll after a dropped answer turned to milliseconds.
func captureBench(t *testing.T, p *capturePlane) *bench {
	t.Helper()
	b := sandboxBench(t, p.srv.URL)
	b.projectFile(cfg.Project{
		Project: "proj",
		Default: "ci",
		Environments: map[string]cfg.EnvConfig{
			"dev": {ID: devID},
			"ci":  {ID: ciID},
		},
	})
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	interval := capturePollInterval
	capturePollInterval = 10 * time.Millisecond
	t.Cleanup(func() { capturePollInterval = interval })
	return b
}

func loadLedger(t *testing.T, b *bench) []cfg.BaselineRef {
	t.Helper()
	l, err := cfg.LoadLocal(filepath.Join(b.project, ".veris", "twin.local.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return l.Baselines
}

// --- snapshot create --------------------------------------------------------

func TestSnapshotCreateRecordsTheWorld(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.create = func(p *capturePlane, body api.CreateSnapshotRequest) (int, any) {
			sn := newSnapshot(body.Name)
			p.snapshots = append(p.snapshots, sn)
			return 201, api.SnapshotResponse{Snapshot: sn, CuratorClockRestored: true,
				Scrubbed: map[string][]string{"stripe": {"deliveries", "_veris_requests", "webhook_endpoints.url"}}}
		}
	})
	code, stdout, stderr := runSandboxCLI(t, "snapshot", "create", "--name", "after-onboarding", "--clock-restore", "frozen", "--keep-external", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	bodies := p.createBodies()
	if len(bodies) != 1 {
		t.Fatalf("POSTs = %d, want exactly one", len(bodies))
	}
	want := api.CreateSnapshotRequest{SandboxID: sbID, Name: "after-onboarding", ClockRestore: api.ClockFrozen, KeepExternalDestinations: true}
	if bodies[0] != want {
		t.Errorf("body = %+v, want %+v", bodies[0], want)
	}
	if p.listCount() != 0 {
		t.Errorf("a 2xx answer must not be followed by a poll; lists = %d", p.listCount())
	}
	sbInOrder(t, stderr,
		"! Capturing freezes and scrubs sandbox "+sbID+"; deploy a fresh one afterwards.",
		"Capturing world",
		"✓ Snapshot recorded: "+snapNewID+` "after-onboarding" (snap-`+snapNewID+", 4.2 MB, clock today, ",
		"  scrubbed: stripe [deliveries, _veris_requests, webhook_endpoints.url]",
		"! webhook destinations under the curator's callback URL became veris://client/…",
		"external destinations were kept",
		"! the source sandbox "+sbID+" is frozen and scrubbed; delete it: veris down",
		"→ https://studio.example/environments/"+ciID,
		"→ Next: veris up --snapshot "+snapNewID)
	var resp api.SnapshotResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil || resp.Snapshot.ID != snapNewID || !resp.CuratorClockRestored {
		t.Errorf("--json stdout = %q (%v)", stdout, err)
	}
	if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
		t.Errorf("the pointer must survive a create without --delete-source; got %+v", ptr)
	}
}

func TestSnapshotCreatePollsWhenTheLoadBalancerDrops(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.snapshots = []api.Snapshot{oldSnapshot()}
		p.create = func(p *capturePlane, body api.CreateSnapshotRequest) (int, any) {
			return 502, rawBody("<html><body>502 Bad Gateway</body></html>")
		}
		// The row appears on the third read: the first two see only the
		// hour-old capture of the same sandbox.
		p.onList = func(p *capturePlane, n int) {
			if n == 3 {
				p.snapshots = append(p.snapshots, newSnapshot("after-onboarding"))
			}
		}
	})
	code, _, stderr := runSandboxCLI(t, "snapshot", "create", "--name", "after-onboarding", "--delete-source")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if n := len(p.createBodies()); n != 1 {
		t.Errorf("the POST was sent %d times; a dropped answer must never be re-sent", n)
	}
	if p.listCount() < 3 {
		t.Errorf("lists = %d, want the poll to keep reading until the row appears", p.listCount())
	}
	sbInOrder(t, stderr,
		"! The load balancer answered 502 after ",
		"; the control plane is still capturing —",
		"  polling GET /v1/environments/"+ciID+"/snapshots for source_sandbox="+shortID(sbID),
		"✓ Snapshot recorded: "+snapNewID+` "after-onboarding" (snap-`+snapNewID+", 4.2 MB, clock today, ",
		"  scrub details unavailable: the load balancer dropped the response",
		"✓ Sandbox deleted: "+sbID,
		"→ Next: veris up --snapshot "+snapNewID)
	if strings.Contains(stderr, snapOldID) {
		t.Errorf("the hour-old capture of the same sandbox was taken for the new one:\n%s", stderr)
	}
	if got := p.sandboxDeletes(); len(got) != 1 || got[0] != ciID+"/"+sbID {
		t.Errorf("--delete-source deleted %v, want [%s/%s]", got, ciID, sbID)
	}
	if ptr := sbPointer(t, b); ptr != nil {
		t.Errorf("--delete-source must forget the folder's pointer; got %+v", ptr)
	}
}

func TestSnapshotCreatePollsAfterAReadTimeout(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.hold = 5 * time.Second
		p.create = func(p *capturePlane, body api.CreateSnapshotRequest) (int, any) {
			t.Error("the held answer must never be written: the client should have left")
			return 500, nil
		}
		p.onList = func(p *capturePlane, n int) {
			if n == 2 {
				p.snapshots = append(p.snapshots, newSnapshot(""))
			}
		}
	})
	code, _, stderr := runSandboxCLI(t, "snapshot", "create", "--timeout", "150ms")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if n := len(p.createBodies()); n != 1 {
		t.Errorf("the POST was sent %d times; a timed-out answer must never be re-sent", n)
	}
	sbInOrder(t, stderr,
		"! No answer after ",
		"; the control plane is still capturing —",
		"  polling GET /v1/environments/"+ciID+"/snapshots",
		"✓ Snapshot recorded: "+snapNewID+" (snap-"+snapNewID+", 4.2 MB, clock today, ")
	if strings.Contains(stderr, `""`) {
		t.Errorf("an unnamed snapshot must not print an empty name:\n%s", stderr)
	}
}

// The snapshot exists once the capture is confirmed; a --delete-source that
// fails owns the exit code but must not swallow the id a script came for.
func TestSnapshotCreateKeepsTheIDWhenTheSourceDeleteFails(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.deleteSandboxStatus = 503
		p.create = func(p *capturePlane, body api.CreateSnapshotRequest) (int, any) {
			sn := newSnapshot(body.Name)
			p.snapshots = append(p.snapshots, sn)
			return 201, api.SnapshotResponse{Snapshot: sn, CuratorClockRestored: true}
		}
	})
	code, stdout, stderr := runSandboxCLI(t, "snapshot", "create", "--name", "kept", "--delete-source", "--json")
	if code != 1 {
		t.Fatalf("exit %d, want 1 for the failed delete\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"✓ Snapshot recorded: "+snapNewID+` "kept"`,
		"✗ Failed to delete sandbox "+sbID+": [503]",
		"! the source sandbox "+sbID+" was not deleted; it is frozen and scrubbed: veris down",
		"→ https://studio.example/environments/"+ciID,
		"→ Next: veris up --snapshot "+snapNewID)
	var resp api.SnapshotResponse
	if json.Unmarshal([]byte(stdout), &resp) != nil || resp.Snapshot.ID != snapNewID {
		t.Errorf("--json must still carry the snapshot: stdout = %q", stdout)
	}
	if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
		t.Errorf("a source that was not deleted keeps the pointer; got %+v", ptr)
	}
}

func TestSnapshotCreateRefusalsAreNotPolled(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   any
		want   string
	}{
		{"a 409 is someone else's capture", 409,
			map[string]string{"detail": "another promote is capturing sandbox " + sbID},
			"✗ Failed to create snapshot of sandbox " + sbID + ": [409] another promote is capturing sandbox " + sbID},
		{"a control-plane 502 carries a detail and is a failure", 502,
			map[string]string{"detail": "capture failed: stream ended early"},
			"✗ Failed to create snapshot of sandbox " + sbID + ": [502] capture failed: stream ended early"},
		{"a 503 says snapshots are unavailable", 503,
			map[string]string{"detail": "snapshots are not available on this deployment"},
			"[503] snapshots are not available on this deployment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newCapturePlane(t)
			captureBench(t, p)
			p.script(func(p *capturePlane) {
				p.create = func(p *capturePlane, body api.CreateSnapshotRequest) (int, any) { return tc.status, tc.body }
			})
			code, _, stderr := runSandboxCLI(t, "snapshot", "create")
			if code != 1 {
				t.Errorf("exit %d, want 1\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("missing %q in:\n%s", tc.want, stderr)
			}
			if p.listCount() != 0 {
				t.Errorf("a refusal must not be polled; lists = %d", p.listCount())
			}
			if strings.Contains(stderr, "✓") {
				t.Errorf("nothing succeeded:\n%s", stderr)
			}
		})
	}
}

func TestSnapshotCreateExits4AtThePollDeadline(t *testing.T) {
	p := newCapturePlane(t)
	b := captureBench(t, p)
	p.script(func(p *capturePlane) {
		p.snapshots = []api.Snapshot{oldSnapshot()}
		p.create = func(p *capturePlane, body api.CreateSnapshotRequest) (int, any) {
			return 504, rawBody("upstream request timeout")
		}
	})
	code, _, stderr := runSandboxCLI(t, "snapshot", "create", "--timeout", "60ms", "--delete-source")
	if code != 4 {
		t.Fatalf("exit %d, want 4\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"! The load balancer answered 504 after ",
		"! Capture unconfirmed after 60ms; check veris snapshot list before re-running")
	if p.listCount() < 2 {
		t.Errorf("lists = %d, want the poll to read more than once inside the deadline", p.listCount())
	}
	if n := len(p.createBodies()); n != 1 {
		t.Errorf("POSTs = %d, want one", n)
	}
	if len(p.sandboxDeletes()) != 0 {
		t.Errorf("an unconfirmed capture must not delete its source: %v", p.sandboxDeletes())
	}
	if ptr := sbPointer(t, b); ptr == nil {
		t.Error("the pointer must survive an unconfirmed capture")
	}
}

func TestSnapshotCreateNeedsASandboxAndAKnownClockMode(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	code, _, stderr := runSandboxCLI(t, "snapshot", "create", "--clock-restore", "yesterday")
	if code != 1 || !strings.Contains(stderr, "✗ --clock-restore must be today, frozen or rebase (got 'yesterday')") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	code, _, stderr = runSandboxCLI(t, "snapshot", "create", "--sandbox", otherSbID)
	if code != 1 || !strings.Contains(stderr, "✗ Failed to read sandbox "+otherSbID+": [404]") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	code, _, stderr = runSandboxCLI(t, "snapshot", "create", "extra")
	if code != 1 || !strings.Contains(stderr, `snapshot create takes no arguments (got "extra")`) {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	if n := len(p.createBodies()); n != 0 {
		t.Errorf("POSTs = %d, want none", n)
	}
}

// --- snapshot list / get / delete -------------------------------------------

func twoSnapshots() []api.Snapshot {
	newer := newSnapshot("after-onboarding")
	newer.CreatedAt = at(time.Date(2026, 9, 2, 12, 22, 0, 0, time.UTC))
	older := oldSnapshot()
	older.CreatedAt = at(time.Date(2026, 8, 28, 16, 5, 0, 0, time.UTC))
	// Served oldest first to prove the CLI orders them itself.
	return []api.Snapshot{older, newer}
}

func TestSnapshotListIsNewestFirst(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	p.script(func(p *capturePlane) { p.snapshots = twoSnapshots() })
	code, stdout, stderr := runSandboxCLI(t, "snapshot", "list")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d stdout %q\n%s", code, stdout, stderr)
	}
	sbInOrder(t, stderr,
		"ID", "Name", "Revision", "Clock", "Size", "Source", "Created",
		snapNewID, "after-onboarding", "snap-"+snapNewID, "today", "4.2 MB", shortID(sbID),
		snapOldID, "empty-stripe", "snap-"+snapOldID, "today", "1.1 MB")

	code, stdout, _ = runSandboxCLI(t, "snapshot", "list", "--json")
	var list []api.Snapshot
	if code != 0 || json.Unmarshal([]byte(stdout), &list) != nil || len(list) != 2 || list[0].ID != snapNewID {
		t.Errorf("--json: exit %d stdout %q", code, stdout)
	}

	p.script(func(p *capturePlane) { p.snapshots = nil })
	code, stdout, stderr = runSandboxCLI(t, "snapshot", "list")
	if code != 0 || !strings.Contains(stderr, "No snapshots of 'ci'") {
		t.Errorf("empty: exit %d\n%s", code, stderr)
	}
	code, stdout, _ = runSandboxCLI(t, "snapshot", "list", "--json")
	if code != 0 || strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty --json: exit %d stdout %q", code, stdout)
	}
}

func TestSnapshotGetResolvesIdsAndNames(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	dup := newSnapshot("after-onboarding")
	dup.ID, dup.RevisionID = snapDupID, "snap-"+snapDupID
	dup.CreatedAt = at(time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC))
	p.script(func(p *capturePlane) { p.snapshots = append(twoSnapshots(), dup) })

	code, _, stderr := runSandboxCLI(t, "snapshot", "get", snapOldID)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "Snapshot "+snapOldID, "Name:        empty-stripe", "Environment: ci",
		"Revision:    snap-"+snapOldID, "Image:       "+imageOld, "Clock:       today", "Size:        1.1 MB",
		"Source:      sandbox "+sbID, "Created:     2026-08-28", "→ Next: veris up --snapshot "+snapOldID)

	code, stdout, stderr := runSandboxCLI(t, "snapshot", "get", "after-onboarding", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "! 2 snapshots are named 'after-onboarding'; using the newest, "+snapNewID) {
		t.Errorf("a name shared by two must pick the newest aloud:\n%s", stderr)
	}
	var sn api.Snapshot
	if json.Unmarshal([]byte(stdout), &sn) != nil || sn.ID != snapNewID {
		t.Errorf("--json stdout = %q", stdout)
	}

	code, _, stderr = runSandboxCLI(t, "snapshot", "get", "nope")
	if code != 1 || !strings.Contains(stderr, "✗ No snapshot named 'nope' in environment 'ci' (have: after-onboarding, after-onboarding, empty-stripe)") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	code, _, stderr = runSandboxCLI(t, "snapshot", "get", otherSbID)
	if code != 1 || !strings.Contains(stderr, "✗ No snapshot "+otherSbID+" in environment 'ci'") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
	code, _, stderr = runSandboxCLI(t, "snapshot", "get")
	if code != 1 || !strings.Contains(stderr, "snapshot get takes one snapshot id or name") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
}

func TestSnapshotDeleteConfirmsThenDeletes(t *testing.T) {
	p := newCapturePlane(t)
	captureBench(t, p)
	p.script(func(p *capturePlane) { p.snapshots = twoSnapshots() })

	code, _, stderr := runSandboxCLI(t, "snapshot", "delete", "empty-stripe")
	if code != 1 || !strings.Contains(stderr, "Interactive prompt requires a TTY. Pass --yes") {
		t.Errorf("off a TTY without --yes: exit %d\n%s", code, stderr)
	}
	if len(p.snapshotDeletes()) != 0 {
		t.Fatalf("deleted without confirmation: %v", p.snapshotDeletes())
	}

	code, _, stderr = runSandboxCLI(t, "snapshot", "delete", "empty-stripe", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	sbInOrder(t, stderr,
		"Delete snapshot "+snapOldID+` "empty-stripe" (snap-`+snapOldID+", 1.1 MB)? y",
		"✓ Snapshot deleted: "+snapOldID)
	if got := p.snapshotDeletes(); len(got) != 1 || got[0] != ciID+"/"+snapOldID {
		t.Errorf("DELETE %v, want [%s/%s]", got, ciID, snapOldID)
	}

	p.script(func(p *capturePlane) { p.deleteSnapshotStatus = 404 })
	code, _, stderr = runSandboxCLI(t, "snapshot", "delete", snapOldID, "--yes")
	if code != 0 || !strings.Contains(stderr, "! Snapshot "+snapOldID+" was already gone") {
		t.Errorf("404: exit %d\n%s", code, stderr)
	}

	p.script(func(p *capturePlane) { p.deleteSnapshotStatus = 409 })
	code, _, stderr = runSandboxCLI(t, "snapshot", "delete", snapNewID, "--yes")
	if code != 1 || !strings.Contains(stderr, "✗ Failed to delete snapshot "+snapNewID+": [409] snapshot "+snapNewID+" is booted by sandbox") {
		t.Errorf("409: exit %d\n%s", code, stderr)
	}
}

func TestSnapshotHelpersRender(t *testing.T) {
	sizes := map[int64]string{0: "0 B", 512: "512 B", 1153433: "1.1 MB", 4404019: "4.2 MB", 3 << 30: "3.0 GB", 2048: "2.0 KB"}
	for n, want := range sizes {
		if got := sizeText(n); got != want {
			t.Errorf("sizeText(%d) = %q, want %q", n, got, want)
		}
	}
	elapsed := map[time.Duration]string{41 * time.Second: "41 s", 221 * time.Second: "3 m 41 s", 3725 * time.Second: "1 h 2 m", 400 * time.Millisecond: "0 s"}
	for d, want := range elapsed {
		if got := elapsedText(d); got != want {
			t.Errorf("elapsedText(%s) = %q, want %q", d, got, want)
		}
	}
	if lbDropped(&api.Error{Status: 502, Body: []byte(`{"detail":"capture failed"}`)}) {
		t.Error("a 502 with a detail is the control plane refusing, not the load balancer dropping")
	}
	if !lbDropped(&api.Error{Status: 502, Body: []byte("<html>502</html>")}) {
		t.Error("a 502 without a detail is the load balancer")
	}
	if lbDropped(&api.Error{Status: 409, Body: []byte(`{"detail":"busy"}`)}) {
		t.Error("a 4xx is never a dropped answer")
	}
	// The poll after a dropped answer takes only a row of the name asked
	// for: a capture named otherwise inside the window is somebody else's.
	since := time.Now().Add(-time.Minute)
	snaps := []api.Snapshot{newSnapshot("a")}
	if newestSnapshotOf(snaps, sbID, "b", since) != nil {
		t.Error("a snapshot named a must not answer a poll for b")
	}
	if got := newestSnapshotOf(snaps, sbID, "a", since); got == nil || got.Name != "a" {
		t.Error("a snapshot named a answers a poll for a")
	}
	if got := newestSnapshotOf(snaps, sbID, "", since); got == nil {
		t.Error("an unnamed poll takes any row of the sandbox")
	}
}
