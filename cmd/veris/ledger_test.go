package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/proxy"
	"github.com/veris-ai/veris-cli/internal/routes"
	"github.com/veris-ai/veris-cli/internal/twin"
)

// ledgerTwins is a stripe twin with a request log and a postgres twin with
// none, under /s/<sandbox>/<twin>. The stripe twin is also the vendor the
// engine routes api.stripe.com at: a vendor-surface request lands on its
// catch-all and, when record is on, is written to the log the way a real
// twin's handler tier would write it -- which is what makes the two ledgers
// comparable in a test.
type ledgerTwins struct {
	srv *httptest.Server
	mu  sync.Mutex

	rows   []twin.Request // the stripe log, ascending by id
	nextID int
	// since is how GET /veris/requests treats since_id: "honour" filters,
	// "ignore" drops the parameter the way FastAPI drops an unknown one,
	// "reject" answers 422 naming it.
	since string
	// queries records every GET /veris/requests query, in order.
	queries []url.Values
	// record writes a handler row for every vendor-surface request.
	record bool
	// failAfter makes GET /veris/requests answer 500 after this many calls;
	// 0 never fails.
	failAfter     int
	requestsCalls int
	// noLog makes the stripe twin serve no request log at all: its
	// /veris/requests is FastAPI's 404, as on a twin built without one.
	noLog bool
	// yrows is the yente twin's log (yenteTwin), ascending by id.
	yrows []twin.Request
	ynext int
}

func newLedgerTwins(t *testing.T) *ledgerTwins {
	t.Helper()
	f := &ledgerTwins{since: "honour", record: true}
	mux := http.NewServeMux()
	stripe := "/s/" + sbID + "/stripe"
	pg := "/s/" + sbID + "/postgres"
	mux.HandleFunc(stripe+"/veris/health", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 200, map[string]any{"status": "ok", "service": "stripe", "state_version": 3})
	})
	mux.HandleFunc(pg+"/veris/health", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 200, map[string]any{"status": "ok", "service": "postgres"})
	})
	mux.HandleFunc(stripe+"/veris/data", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			sbJSON(w, 200, map[string]any{"counts": map[string]int{"customers": 1, "clock": 1, "client": 1}, "state_version": 3})
		case http.MethodPost:
			var body struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			added := map[string]int{}
			for table, rows := range body.Data {
				if list, ok := rows.([]any); ok {
					added[table] = len(list)
				}
			}
			sbJSON(w, 200, map[string]any{"added": added})
		}
	})
	mux.HandleFunc(pg+"/veris/data", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 404, map[string]any{"detail": "Not Found"})
	})
	mux.HandleFunc(pg+"/veris/requests", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 404, map[string]any{"detail": "Not Found"})
	})
	// A second HTTP twin with a log the child never writes to, for a
	// sandbox that mixes logged and unlogged twins.
	github := "/s/" + sbID + "/github"
	mux.HandleFunc(github+"/veris/health", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 200, map[string]any{"status": "ok", "service": "github", "state_version": 1})
	})
	mux.HandleFunc(github+"/veris/requests", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 200, map[string]any{"requests": []twin.Request{}})
	})
	mux.HandleFunc(github+"/", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 200, map[string]any{})
	})
	f.yenteTwin(mux)
	mux.HandleFunc(stripe+"/veris/requests", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.noLog {
			sbJSON(w, 404, map[string]any{"detail": "Not Found"})
			return
		}
		f.requestsCalls++
		q := r.URL.Query()
		f.queries = append(f.queries, q)
		if f.failAfter > 0 && f.requestsCalls > f.failAfter {
			sbJSON(w, 500, map[string]any{"detail": "the log is unavailable"})
			return
		}
		since := 0
		if v := q.Get("since_id"); v != "" {
			switch f.since {
			case "reject":
				sbJSON(w, 422, map[string]any{"detail": []map[string]any{{
					"loc": []any{"query", "since_id"}, "msg": "Extra inputs are not permitted", "type": "extra_forbidden",
				}}})
				return
			case "honour":
				since, _ = strconv.Atoi(v)
			}
		}
		limit := 50
		if v := q.Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		out := make([]twin.Request, 0, len(f.rows))
		for i := len(f.rows) - 1; i >= 0; i-- {
			if f.rows[i].ID > since {
				out = append(out, f.rows[i])
			}
		}
		if q.Get("order") == "asc" {
			sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		}
		if len(out) > limit {
			out = out[:limit]
		}
		sbJSON(w, 200, map[string]any{"requests": out})
	})
	// The vendor surface: what the engine routes api.stripe.com at.
	mux.HandleFunc(stripe+"/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.record {
			f.add(r.Method, strings.TrimPrefix(r.URL.Path, stripe), twin.TierHandler, 200)
		}
		sbJSON(w, 200, map[string]any{"object": "list", "data": []any{}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// add appends one row to the stripe log; status 0 is a hang (null).
func (f *ledgerTwins) add(method, path, tier string, status int) twin.Request {
	f.nextID++
	row := twin.Request{ID: f.nextID, TS: int(time.Now().Unix()), Method: method, Path: path, Tier: tier}
	if status != 0 {
		s := status
		row.Status = &s
	}
	f.rows = append(f.rows, row)
	return row
}

func (f *ledgerTwins) script(fn func(f *ledgerTwins)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *ledgerTwins) seen() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.queries...)
}

func (f *ledgerTwins) stripe() *twin.Client { return twin.New(f.srv.URL + "/s/" + sbID + "/stripe") }

// services is the sandbox's service list: stripe proxied and logged,
// postgres a data plane with a control URL that serves no log.
func (f *ledgerTwins) services() []api.ServiceInfo {
	stripe := f.srv.URL + "/s/" + sbID + "/stripe"
	return []api.ServiceInfo{
		{Name: "stripe", Status: "ready", URL: stripe, ControlURL: stripe, EnvHint: "STRIPE_API_BASE",
			Routes: []routes.Entry{{Host: "api.stripe.com"}}},
		{Name: "postgres", Status: "ready", URL: "postgresql://app:app@10.0.0.5:5432/sb?sslmode=require",
			ControlURL: f.srv.URL + "/s/" + sbID + "/postgres", EnvHint: "DATABASE_URL"},
	}
}

// withGithub is services plus the github twin, an HTTP twin with a log.
func (f *ledgerTwins) withGithub() []api.ServiceInfo {
	github := f.srv.URL + "/s/" + sbID + "/github"
	return append(f.services(), api.ServiceInfo{Name: "github", Status: "ready", URL: github,
		ControlURL: github, EnvHint: "GITHUB_API_BASE", Routes: []routes.Entry{{Host: "api.github.com"}}})
}

// withYente is services plus the yente twin: an http twin with a log that
// the proxy has no hostname for -- the control plane serves none, and there
// is no other source -- so its URL is handed to the command under
// YENTE_API_BASE and called directly.
func (f *ledgerTwins) withYente() []api.ServiceInfo {
	yente := f.srv.URL + "/s/" + sbID + "/yente"
	return append(f.services(), api.ServiceInfo{Name: "yente", Status: "ready", URL: yente, ControlURL: yente, EnvHint: "YENTE_API_BASE"})
}

// yenteTwin serves the yente twin under /s/<sandbox>/yente on mux: health,
// a request log that honours since_id, and a catch-all that records every
// vendor-surface call as a handler row.
func (f *ledgerTwins) yenteTwin(mux *http.ServeMux) {
	yente := "/s/" + sbID + "/yente"
	mux.HandleFunc(yente+"/veris/health", func(w http.ResponseWriter, _ *http.Request) {
		sbJSON(w, 200, map[string]any{"status": "ok", "service": "yente"})
	})
	mux.HandleFunc(yente+"/veris/requests", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		q := r.URL.Query()
		since, _ := strconv.Atoi(q.Get("since_id"))
		limit := 50
		if v := q.Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		out := []twin.Request{}
		for i := len(f.yrows) - 1; i >= 0 && len(out) < limit; i-- {
			if f.yrows[i].ID > since {
				out = append(out, f.yrows[i])
			}
		}
		sbJSON(w, 200, map[string]any{"requests": out})
	})
	mux.HandleFunc(yente+"/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ynext++
		f.yrows = append(f.yrows, twin.Request{ID: f.ynext, TS: int(time.Now().Unix()), Method: r.Method,
			Path: strings.TrimPrefix(r.URL.Path, yente), Tier: twin.TierHandler})
		sbJSON(w, 200, map[string]any{"responses": []any{}})
	})
}

// ledgerBench is a logged-in bench, a control plane serving sbID ready
// with the fake twins, and the twins themselves.
func ledgerBench(t *testing.T) (*bench, *sandboxPlane, *ledgerTwins) {
	t.Helper()
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	t.Setenv(discovery.EnvConfig, "")
	twins := newLedgerTwins(t)
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(), time.Now().Add(30*time.Minute)) }
	})
	return b, plane, twins
}

// runLine runs cmdRun with stderr captured.
func runLine(t *testing.T, args ...string) (error, string) {
	t.Helper()
	var err error
	stderr := captureStderr(t, func() { err = cmdRun(args) })
	return err, stderr
}

func wantExit(t *testing.T, err error, code int) {
	t.Helper()
	var got exitCode
	if code == 0 {
		if err != nil {
			t.Fatalf("err = %v, want exit 0", err)
		}
		return
	}
	if !errors.As(err, &got) || int(got) != code {
		t.Fatalf("err = %v, want exit %d", err, code)
	}
}

func TestLedgerWatermarkAndAfterRead(t *testing.T) {
	_, _, twins := ledgerBench(t)
	// Rows from before the run: a seed, a harness read, an earlier call. The
	// watermark puts every one of them below the line.
	twins.script(func(f *ledgerTwins) {
		f.add("POST", "/veris/data", twin.TierControl, 200)
		f.add("GET", "/v1/customers", twin.TierHandler, 200)
		f.add("GET", "/veris/requests", twin.TierControl, 200)
	})
	argv := child(t, "call")
	err, stderr := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--"}, argv...)...)
	wantExit(t, err, 0)
	sbInOrder(t, stderr,
		"veris: watermark stripe:3 postgres:—\n",
		"veris: the sandbox received 1 request(s):\n",
		"veris: the sandbox recorded 1 request(s) since the watermark:\n",
		"  stripe                       1\n",
		"veris: ✓ required stripe ≥1: saw 1   ✓ ledgers agree (1 = 1)\n",
	)
	for _, absent := range []string{"control-plane (/veris/*)", "could not be read", "✗"} {
		if strings.Contains(stderr, absent) {
			t.Errorf("stderr should not carry %q:\n%s", absent, stderr)
		}
	}
	// The watermark is one row newest-first; the after-read asks for
	// everything above it, a full page at a time.
	seen := twins.seen()
	if len(seen) != 2 {
		t.Fatalf("GET /veris/requests was asked %d times, want 2 (watermark, after-read): %v", len(seen), seen)
	}
	if seen[0].Get("limit") != "1" || seen[0].Get("order") != "desc" {
		t.Errorf("watermark query = %v, want limit=1&order=desc", seen[0])
	}
	if seen[1].Get("since_id") != "3" || seen[1].Get("limit") != "1000" || seen[1].Get("order") != "desc" {
		t.Errorf("after-read query = %v, want since_id=3&limit=1000&order=desc", seen[1])
	}
}

// since_id is newer than some twins. One that validates its query refuses
// it with a 422 naming the parameter and is asked again without; one that
// ignores it answers the whole page, which is filtered here either way.
func TestLedgerSinceIDNegotiation(t *testing.T) {
	twins := newLedgerTwins(t)
	twins.script(func(f *ledgerTwins) {
		for i := 0; i < 5; i++ {
			f.add("GET", "/v1/charges", twin.TierHandler, 200)
		}
	})
	for _, mode := range []string{"honour", "ignore", "reject"} {
		t.Run(mode, func(t *testing.T) {
			twins.script(func(f *ledgerTwins) { f.since, f.queries = mode, nil })
			rows, capped, err := readSince(context.Background(), twins.stripe(), 3)
			if err != nil {
				t.Fatalf("readSince: %v", err)
			}
			if capped || len(rows) != 2 || rows[0].ID != 5 || rows[1].ID != 4 {
				t.Errorf("rows above 3 = %+v (capped %v), want ids 5 and 4", rows, capped)
			}
			seen := twins.seen()
			wantCalls := 1
			if mode == "reject" {
				wantCalls = 2
			}
			if len(seen) != wantCalls {
				t.Fatalf("%d queries, want %d: %v", len(seen), wantCalls, seen)
			}
			if seen[0].Get("since_id") != "3" {
				t.Errorf("the first query must carry since_id=3, got %v", seen[0])
			}
			if mode == "reject" && seen[1].Has("since_id") {
				t.Errorf("the retry must drop since_id, got %v", seen[1])
			}
		})
	}
	// A mark of 0 (an empty log before the run) sends no since_id at all.
	twins.script(func(f *ledgerTwins) { f.since, f.queries = "reject", nil })
	if _, _, err := readSince(context.Background(), twins.stripe(), 0); err != nil {
		t.Fatalf("mark 0: %v", err)
	}
	if seen := twins.seen(); len(seen) != 1 || seen[0].Has("since_id") {
		t.Errorf("mark 0 must not send since_id: %v", seen)
	}
	if !rejectsSinceID(&twin.Error{Status: 422, Detail: "query.since_id: Extra inputs are not permitted"}) ||
		rejectsSinceID(&twin.Error{Status: 422, Detail: "limit must be <= 1000"}) ||
		rejectsSinceID(&twin.Error{Status: 500, Detail: "since_id"}) {
		t.Error("rejectsSinceID must match only a 422 naming since_id")
	}
}

// proofFor is a proof over the fake twins with the marks already taken.
func proofFor(t *testing.T, twins *ledgerTwins) *proof {
	t.Helper()
	p := &proof{sandboxID: sbID, envID: ciID, services: twins.services(), twinFor: twin.New}
	p.watermark(context.Background(), os.Stderr, true)
	return p
}

func TestLedgerControlRowsAreExcludedAndFaultsCounted(t *testing.T) {
	twins := newLedgerTwins(t)
	p := proofFor(t, twins)
	if got := formatWatermark(p.marks); got != "stripe:0 postgres:—" {
		t.Errorf("watermark = %q", got)
	}
	twins.script(func(f *ledgerTwins) {
		f.add("POST", "/v1/charges", twin.TierHandler, 200)
		f.add("POST", "/v1/charges", twin.TierFault, 429)
		f.add("GET", "/veris/data", twin.TierControl, 200)
		f.add("GET", "/veris/requests", twin.TierControl, 200)
		f.add("POST", "https://odd-forest.trycloudflare.com/hooks/stripe", twin.TierDelivery, 200)
		f.add("GET", "/v1/balance", twin.TierHandler, 0) // a hang: no status
	})
	l := p.read(context.Background())
	if len(l.Twins) != 1 || l.Twins[0].Name != "stripe" {
		t.Fatalf("twins = %+v, want stripe alone (postgres has no log)", l.Twins)
	}
	st := l.Twins[0]
	if st.Count != 3 || st.Faults != 1 || st.Control != 2 || st.Capped {
		t.Errorf("stripe = %+v, want count 3 (1 fault), control 2", st)
	}
	if len(l.Deliveries) != 1 || l.Deliveries[0].Path != "/hooks/stripe" || l.Deliveries[0].Count != 1 {
		t.Errorf("deliveries = %+v", l.Deliveries)
	}
	var out strings.Builder
	printLedger(&out, l)
	sbInOrder(t, out.String(),
		"veris: the sandbox recorded 3 request(s) since the watermark:\n",
		"  stripe                       3   (1 fault)\n",
		"  control-plane (/veris/*)     2   not counted\n",
		"veris: the sandbox delivered 1 callback(s):\n",
		"  POST   /hooks/stripe                  1 -> 200   (stripe)\n",
	)

	// Nothing but harness traffic: the doc's one-line form.
	twins.script(func(f *ledgerTwins) {
		f.rows = nil
		f.add("GET", "/veris/data", twin.TierControl, 200)
	})
	p = proofFor(t, twins)
	twins.script(func(f *ledgerTwins) {
		f.add("GET", "/veris/schema", twin.TierControl, 200)
		f.add("GET", "/veris/manual", twin.TierControl, 200)
	})
	out.Reset()
	printLedger(&out, p.read(context.Background()))
	if out.String() != "veris: the sandbox recorded 0 request(s) since the watermark (control-plane 2, not counted)\n" {
		t.Errorf("empty ledger printed:\n%s", out.String())
	}
}

// A read that fills the page may have older rows behind it: the count is a
// floor, printed as ≥, and it decides only the assertions it can.
func TestLedgerCapIsAFloorAndUndecidedAssertionsAreIndeterminate(t *testing.T) {
	twins := newLedgerTwins(t)
	p := proofFor(t, twins)
	twins.script(func(f *ledgerTwins) {
		for i := 0; i < ledgerPageLimit+10; i++ {
			f.add("GET", "/v1/charges", twin.TierHandler, 200)
		}
	})
	l := p.read(context.Background())
	if st := l.twin("stripe"); st == nil || !st.Capped || st.Count != ledgerPageLimit {
		t.Fatalf("stripe = %+v, want capped at %d", l.twin("stripe"), ledgerPageLimit)
	}
	var out strings.Builder
	printLedger(&out, l)
	if !strings.Contains(out.String(), "recorded ≥1000 request(s)") || !strings.Contains(out.String(), "  stripe                       ≥1000\n") {
		t.Errorf("a capped read must print ≥1000:\n%s", out.String())
	}
	got := assertLedger(l, p.marks, []requirement{
		{kind: "service", name: "stripe", count: 1},
		{kind: "service", name: "stripe", count: 2000},
	}, nil, false)
	if len(got) != 2 || !got[0].OK || got[0].Indeterminate {
		t.Errorf("stripe:1 under a cap of 1000 is met: %+v", got)
	}
	if len(got) != 2 || got[1].OK || !got[1].Indeterminate || !strings.Contains(got[1].Why, "cap") {
		t.Errorf("stripe:2000 under a cap of 1000 is undecidable: %+v", got)
	}

	// A full page that reaches below the mark covered everything above it:
	// not capped, however many rows came back.
	twins.script(func(f *ledgerTwins) { f.since = "ignore" })
	p.marks[0].mark = 600
	l = p.read(context.Background())
	if st := l.twin("stripe"); st == nil || st.Capped || st.Count != 410 {
		t.Errorf("above mark 600 = %+v, want 410 rows, not capped", l.twin("stripe"))
	}
}

func TestLedgerDeliveriesAndCallbackAssertions(t *testing.T) {
	twins := newLedgerTwins(t)
	p := proofFor(t, twins)
	twins.script(func(f *ledgerTwins) {
		f.add("POST", "https://odd-forest.trycloudflare.com/hooks/stripe", twin.TierDelivery, 200)
		f.add("POST", "https://odd-forest.trycloudflare.com/hooks/stripe", twin.TierDelivery, 200)
		f.add("POST", "https://odd-forest.trycloudflare.com/hooks/stripe", twin.TierDelivery, 500)
		f.add("POST", "https://odd-forest.trycloudflare.com/hooks/other", twin.TierDelivery, 0)
	})
	l := p.read(context.Background())
	if l.total() != 0 {
		t.Errorf("deliveries are not requests the app sent: total = %d", l.total())
	}
	if len(l.Deliveries) != 3 {
		t.Fatalf("deliveries = %+v, want three lines (200 x2, 500, hang)", l.Deliveries)
	}
	var out strings.Builder
	printLedger(&out, l)
	sbInOrder(t, out.String(),
		"veris: the sandbox delivered 4 callback(s):\n",
		"  POST   /hooks/other                   1 -> —   (stripe)\n",
		"  POST   /hooks/stripe                  2 -> 200   (stripe)\n",
		"  POST   /hooks/stripe                  1 -> 500   (stripe)\n",
	)
	got := assertLedger(l, p.marks, nil, []requirement{
		{kind: "callback", name: "/hooks/stripe", count: 2},
		{kind: "callback", name: "/hooks/stripe", count: 3},
		{kind: "callback", name: "*", count: 1},
		{kind: "callback", name: "/hooks/other", count: 1},
	}, false)
	want := []struct {
		ok  bool
		got int64
	}{{true, 2}, {false, 2}, {true, 2}, {false, 0}}
	for i, w := range want {
		if got[i].OK != w.ok || got[i].Got != w.got || got[i].Indeterminate {
			t.Errorf("callback assertion %d = %+v, want ok %v got %d", i, got[i], w.ok, w.got)
		}
	}
	var v strings.Builder
	verdict := printVerdict(&v, l, got[3:], nil, false)
	if !verdict.Fatal {
		t.Error("an unmet callback is fatal")
	}
	sbInOrder(t, v.String(),
		"veris: ✗ the run required callback /hooks/other at least 1 time(s) but the sandbox delivered it 0 time(s)\n",
		"            — no callback reached your app",
	)
}

func TestLedgerAssertionOutcomes(t *testing.T) {
	marks := []twinMark{{name: "stripe", envHint: "STRIPE_API_BASE", mark: 3}, {name: "postgres", noLog: true}}
	l := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Count: 6}}}
	engine := &proxy.Receipt{Total: 7, ByService: map[string]int64{"stripe": 7}}

	t.Run("unmet is exit 3 with the doc's lines", func(t *testing.T) {
		var out strings.Builder
		v := printVerdict(&out, &ledger{Twins: []*ledgerTwin{{Name: "stripe"}}},
			assertLedger(&ledger{Twins: []*ledgerTwin{{Name: "stripe"}}}, marks,
				[]requirement{{kind: "service", name: "stripe", count: 1}}, nil, false), nil, false)
		if !v.Fatal || v.Indeterminate {
			t.Errorf("verdict = %+v, want fatal", v)
		}
		sbInOrder(t, out.String(),
			"veris: ✗ the run required service stripe at least 1 time(s) but the sandbox received it 0 time(s)\n",
			"            — your tests never touched the sandbox (is STRIPE_API_BASE overridden in your test setup?)\n",
		)
	})
	t.Run("met, and the ledgers compared", func(t *testing.T) {
		var out strings.Builder
		v := printVerdict(&out, l, assertLedger(l, marks, []requirement{{kind: "service", name: "stripe", count: 5}}, nil, false), engine, false)
		if v.Fatal || v.Indeterminate {
			t.Errorf("verdict = %+v", v)
		}
		if !strings.Contains(out.String(), "veris: ! ledgers differ (engine 7, sandbox 6)\n") ||
			!strings.Contains(out.String(), "veris: ✓ required stripe ≥5: saw 6\n") {
			t.Errorf("printed:\n%s", out.String())
		}
		out.Reset()
		printVerdict(&out, l, nil, &proxy.Receipt{Total: 6, ByService: map[string]int64{"stripe": 6}}, false)
		if out.String() != "veris: ✓ ledgers agree (6 = 6)\n" {
			t.Errorf("printed:\n%s", out.String())
		}
		// Compared over the twins the ledger holds: what the engine sent to
		// a twin without a log is not a disagreement.
		out.Reset()
		printVerdict(&out, l, nil, &proxy.Receipt{Total: 9, ByService: map[string]int64{"stripe": 6, "postgres": 3}}, false)
		if out.String() != "veris: ✓ ledgers agree (6 = 6)\n" {
			t.Errorf("printed:\n%s", out.String())
		}
		// An incomplete ledger cannot be compared, and says nothing.
		out.Reset()
		printVerdict(&out, &ledger{Twins: l.Twins, Unreadable: []string{"github: [500]"}}, nil, &proxy.Receipt{Total: 6}, false)
		if out.String() != "" {
			t.Errorf("an incomplete ledger must not be compared, printed:\n%s", out.String())
		}
	})
	t.Run("host requirements are the engine's alone", func(t *testing.T) {
		if got := assertLedger(l, marks, []requirement{{kind: "host", name: "api.stripe.com", count: 1}}, nil, false); len(got) != 0 {
			t.Errorf("a host requirement judged on the ledger: %+v", got)
		}
	})
	t.Run("a twin that keeps no log is the engine's alone", func(t *testing.T) {
		// The ledger never observed postgres: neither refuted (exit 3) nor
		// undecided (exit 4), just not its to judge.
		if got := assertLedger(l, marks, []requirement{{kind: "service", name: "postgres", count: 1}}, nil, false); len(got) != 0 {
			t.Errorf("a requirement on a twin without a log judged on the ledger: %+v", got)
		}
		// A twin the sandbox does not have at all is still counted 0.
		if got := assertLedger(l, marks, []requirement{{kind: "service", name: "shopify", count: 1}}, nil, false); len(got) != 1 || got[0].OK || got[0].Indeterminate {
			t.Errorf("an unknown twin: %+v", got)
		}
	})
	t.Run("an unreadable twin is indeterminate, exit 4", func(t *testing.T) {
		broken := &ledger{Unreadable: []string{"stripe: [500] the log is unavailable"}}
		var out strings.Builder
		v := printVerdict(&out, broken, assertLedger(broken, marks, []requirement{{kind: "service", name: "stripe", count: 1}}, nil, false), engine, false)
		if v.Fatal || !v.Indeterminate {
			t.Errorf("verdict = %+v, want indeterminate", v)
		}
		if !strings.Contains(out.String(), "veris: ! cannot decide whether the run required service stripe at least 1 time(s): stripe's ledger could not be read\n") {
			t.Errorf("printed:\n%s", out.String())
		}
		// A requirement on another, readable twin is decided as usual.
		if got := assertLedger(&ledger{Twins: l.Twins, Unreadable: broken.Unreadable}, marks,
			[]requirement{{kind: "service", name: "stripe", count: 1}}, nil, false); len(got) != 1 || !got[0].OK {
			t.Errorf("stripe readable beside an unreadable twin: %+v", got)
		}
	})
	t.Run("a fresh run asserts a non-empty ledger by default", func(t *testing.T) {
		empty := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Control: 4}}}
		got := assertLedger(empty, marks, nil, nil, true)
		if len(got) != 1 || got[0].Kind != "ledger" || got[0].OK {
			t.Fatalf("fresh default = %+v", got)
		}
		var out strings.Builder
		if v := printVerdict(&out, empty, got, nil, false); !v.Fatal {
			t.Error("an empty ledger on a fresh run is fatal")
		}
		if !strings.Contains(out.String(), "veris: ✗ this run deployed a sandbox and the sandbox recorded nothing since the watermark\n") {
			t.Errorf("printed:\n%s", out.String())
		}
		// Explicit requirements take the judgement over; traffic satisfies it;
		// attaching asserts nothing.
		if got := assertLedger(empty, marks, []requirement{{kind: "service", name: "stripe", count: 1}}, nil, true); len(got) != 1 || got[0].Kind != "service" {
			t.Errorf("explicit requirements must own the fresh verdict: %+v", got)
		}
		if got := assertLedger(l, marks, nil, nil, true); len(got) != 1 || !got[0].OK {
			t.Errorf("a non-empty ledger meets the fresh default: %+v", got)
		}
		if got := assertLedger(empty, marks, nil, nil, false); len(got) != 0 {
			t.Errorf("attaching must assert nothing: %+v", got)
		}
		// Unreadable and empty: undecidable rather than refuted.
		if got := assertLedger(nil, nil, nil, nil, true); len(got) != 1 || !got[0].Indeterminate {
			t.Errorf("no ledger at all: %+v", got)
		}
	})
}

// The engine can be satisfied while the sandbox was not: a twin that did not
// record what the proxy forwarded. The requirement passes on the engine's
// count -- one verdict, naming the side that answered and what the other
// recorded -- and the disagreement between the ledgers is still printed.
func TestRunRequireServiceMetByTheEngineAloneIsOneVerdict(t *testing.T) {
	_, _, twins := ledgerBench(t)
	twins.script(func(f *ledgerTwins) { f.record = false })
	argv := child(t, "call")
	err, stderr := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--"}, argv...)...)
	wantExit(t, err, 0)
	sbInOrder(t, stderr,
		"veris: the sandbox received 1 request(s):\n",
		"veris: the sandbox recorded 0 request(s) since the watermark\n",
		"veris: ! ledgers differ (engine 1, sandbox 0)\n",
		"veris: ✓ required stripe ≥1: saw 1 (engine; the sandbox ledger recorded 0)\n",
	)
	for _, absent := range []string{"✗", "received it 0 time(s)", "saw it 0 time(s)"} {
		if strings.Contains(stderr, absent) {
			t.Errorf("one verdict, not two; stderr should not carry %q:\n%s", absent, stderr)
		}
	}
}

// A twin the proxy does not route -- yente: an http twin with no hostname to
// intercept -- is handed to the command as its env hint, judged on the
// sandbox ledger alone, and left out of the two-ledger comparison, which the
// engine's 0 for it would otherwise always fail. One verdict per
// requirement, whichever ledger saw the traffic.
func TestRunHandsAnUnproxiedTwinItsHintAndJudgesItOnTheSandboxLedger(t *testing.T) {
	// yenteRun runs the child role against a sandbox of stripe, postgres and
	// yente, requiring stripe and yente, and returns the yente twin's URL
	// beside the outcome.
	yenteRun := func(t *testing.T, role string, extra ...string) (string, error, string) {
		t.Helper()
		_, plane, twins := ledgerBench(t)
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.withYente(), time.Now().Add(30*time.Minute)) }
		})
		argv := child(t, role)
		line := append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--require-service", "yente"}, extra...)
		err, stderr := runLine(t, append(append(line, "--"), argv...)...)
		return twins.srv.URL + "/s/" + sbID + "/yente", err, stderr
	}

	t.Run("handed, met on the sandbox ledger, compared without it", func(t *testing.T) {
		url, err, stderr := yenteRun(t, "yente")
		wantExit(t, err, 0)
		sbInOrder(t, stderr,
			"veris: watermark stripe:0 postgres:— yente:0\n",
			"veris: postgres: not proxied; handed DATABASE_URL=postgresql://app:app@10.0.0.5:5432/sb?sslmode=require\n",
			"veris: yente: not proxied; handed YENTE_API_BASE="+url+"\n",
			"veris: the sandbox received 1 request(s):\n",
			"veris: the sandbox recorded 2 request(s) since the watermark:\n",
			"  stripe                       1\n",
			"  yente                        1   (not proxied)\n",
			"veris: ✓ required stripe ≥1: saw 1   ✓ required yente ≥1: saw 1 (sandbox ledger; not proxied)   ✓ ledgers agree (1 = 1)\n",
		)
		for _, absent := range []string{"✗", "ledgers differ", "cannot decide", "saw it 0 time(s)"} {
			if strings.Contains(stderr, absent) {
				t.Errorf("stderr should not carry %q:\n%s", absent, stderr)
			}
		}
	})
	t.Run("a -e of the user's is never overwritten, and is set for the command", func(t *testing.T) {
		// The child reaches yente through the user's own value of the
		// variable, which the host tier sets as -e says; the run hands
		// nothing for it and says nothing about it. The fake twin's port is
		// only known once the bench exists, hence the placeholder rewritten
		// by the run line's own fixture.
		_, plane, twins := ledgerBench(t)
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.withYente(), time.Now().Add(30*time.Minute)) }
		})
		url := twins.srv.URL + "/s/" + sbID + "/yente"
		argv := child(t, "yente")
		err, stderr := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0",
			"--require-service", "stripe", "--require-service", "yente", "-e", "YENTE_API_BASE=" + url, "--"}, argv...)...)
		wantExit(t, err, 0)
		if strings.Contains(stderr, "handed YENTE_API_BASE") {
			t.Errorf("a variable set with -e must not be handed over it:\n%s", stderr)
		}
		if !strings.Contains(stderr, "veris: postgres: not proxied; handed DATABASE_URL=") {
			t.Errorf("the other handoff still happens:\n%s", stderr)
		}
		if !strings.Contains(stderr, "✓ required yente ≥1: saw 1 (sandbox ledger; not proxied)") {
			t.Errorf("stderr:\n%s", stderr)
		}
	})
	t.Run("neither ledger shows it: exit 3, one line", func(t *testing.T) {
		_, err, stderr := yenteRun(t, "call")
		wantExit(t, err, exitRequirementUnmet)
		sbInOrder(t, stderr,
			"veris: ✗ the run required service yente at least 1 time(s) but the sandbox received it 0 time(s)\n",
			"            — your tests never touched the sandbox (is YENTE_API_BASE overridden in your test setup?)\n",
			"veris: ✓ required stripe ≥1: saw 1   ✓ ledgers agree (1 = 1)\n",
		)
		if strings.Contains(stderr, "required service yente at least 1 time(s) but the sandbox saw it 0 time(s)") {
			t.Errorf("the engine must not judge a twin the ledger judges:\n%s", stderr)
		}
	})
}

// vouch merges the engine's receipt into the ledger's assertions: either
// side showing the count meets the requirement, an unproxied twin is the
// ledger's alone, and an engine that forwarded nothing refutes a
// requirement the ledger could not read.
func TestVouchMergesTheEngineIntoTheVerdict(t *testing.T) {
	marks := []twinMark{{name: "stripe"}, {name: "yente", notProxied: true}}
	engine := &proxy.Receipt{Total: 3, ByService: map[string]int64{"stripe": 3}}
	req := func(name string, n int64) []requirement {
		return []requirement{{kind: "service", name: name, count: n}}
	}

	t.Run("the ledger met it and the engine saw less", func(t *testing.T) {
		l := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Count: 5}}}
		got := assertLedger(l, marks, req("stripe", 5), nil, false)
		vouch(got, engine)
		if len(got) != 1 || !got[0].OK || got[0].Source != "ledger" || got[0].met() != "required stripe ≥5: saw 5 (sandbox ledger; the engine saw 3)" {
			t.Errorf("%+v: %s", got, got[0].met())
		}
	})
	t.Run("the engine met it and the ledger recorded less", func(t *testing.T) {
		l := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Count: 1}}}
		got := assertLedger(l, marks, req("stripe", 2), nil, false)
		vouch(got, engine)
		if len(got) != 1 || !got[0].OK || got[0].Got != 3 || got[0].Source != "engine" || got[0].met() != "required stripe ≥2: saw 3 (engine; the sandbox ledger recorded 1)" {
			t.Errorf("%+v: %s", got, got[0].met())
		}
	})
	t.Run("both agree: no source named", func(t *testing.T) {
		l := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Count: 3}}}
		got := assertLedger(l, marks, req("stripe", 3), nil, false)
		vouch(got, engine)
		if len(got) != 1 || !got[0].OK || got[0].Source != "" || got[0].met() != "required stripe ≥3: saw 3" {
			t.Errorf("%+v: %s", got, got[0].met())
		}
	})
	t.Run("neither shows it", func(t *testing.T) {
		l := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Count: 3}}}
		got := assertLedger(l, marks, req("stripe", 4), nil, false)
		vouch(got, engine)
		if len(got) != 1 || got[0].OK || got[0].Indeterminate || got[0].Got != 3 {
			t.Errorf("%+v", got)
		}
	})
	t.Run("an unproxied twin is the ledger's alone, whatever the engine says", func(t *testing.T) {
		l := &ledger{Twins: []*ledgerTwin{{Name: "yente", Count: 4, NotProxied: true}}}
		got := assertLedger(l, marks, req("yente", 1), nil, false)
		vouch(got, &proxy.Receipt{ByService: map[string]int64{"yente": 0}})
		if len(got) != 1 || !got[0].OK || !got[0].NotProxied || got[0].met() != "required yente ≥1: saw 4 (sandbox ledger; not proxied)" {
			t.Errorf("%+v: %s", got, got[0].met())
		}
		got = assertLedger(&ledger{Twins: []*ledgerTwin{{Name: "yente", NotProxied: true}}}, marks, req("yente", 1), nil, false)
		vouch(got, &proxy.Receipt{ByService: map[string]int64{"yente": 9}})
		if len(got) != 1 || got[0].OK || got[0].Source != "" {
			t.Errorf("the engine's count of an unproxied twin is no evidence: %+v", got)
		}
	})
	t.Run("an unreadable ledger: the engine vouches, or refutes", func(t *testing.T) {
		broken := &ledger{Unreadable: []string{"stripe: [500] the log is unavailable"}}
		got := assertLedger(broken, marks, req("stripe", 1), nil, false)
		vouch(got, engine)
		if len(got) != 1 || !got[0].OK || got[0].Indeterminate || got[0].met() != "required stripe ≥1: saw 3 (engine; stripe's ledger could not be read)" {
			t.Errorf("%+v: %s", got, got[0].met())
		}
		got = assertLedger(broken, marks, req("stripe", 4), nil, false)
		vouch(got, engine)
		if len(got) != 1 || got[0].OK || got[0].Indeterminate ||
			got[0].unmet() != "the run required service stripe at least 4 time(s) but the engine saw it 3 time(s) and stripe's ledger could not be read" {
			t.Errorf("%+v: %s", got, got[0].unmet())
		}
		// Read to the cap, rows may yet exist: undecided stays undecided.
		capped := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Count: 3, Capped: true}}}
		got = assertLedger(capped, marks, req("stripe", 4), nil, false)
		vouch(got, engine)
		if len(got) != 1 || !got[0].Indeterminate {
			t.Errorf("a capped ledger is not refuted by the engine: %+v", got)
		}
		// Without the engine's receipt nothing changes.
		got = assertLedger(broken, marks, req("stripe", 1), nil, false)
		vouch(got, nil)
		if len(got) != 1 || !got[0].Indeterminate {
			t.Errorf("%+v", got)
		}
	})
	t.Run("the comparison leaves unproxied twins out", func(t *testing.T) {
		l := &ledger{Twins: []*ledgerTwin{{Name: "stripe", Count: 3}, {Name: "yente", Count: 4, NotProxied: true}}}
		var out strings.Builder
		printVerdict(&out, l, nil, engine, false)
		if out.String() != "veris: ✓ ledgers agree (3 = 3)\n" {
			t.Errorf("printed:\n%s", out.String())
		}
		// Only unproxied twins: nothing to compare, so nothing is claimed.
		out.Reset()
		printVerdict(&out, &ledger{Twins: []*ledgerTwin{{Name: "yente", Count: 4, NotProxied: true}}}, nil, engine, false)
		if out.String() != "" {
			t.Errorf("printed:\n%s", out.String())
		}
	})
}

// A suite that crashed before it reached the sandbox is exit 3, not its
// own code: "never touched the sandbox" is the verdict that matters,
// whatever the runner reported, and a harness reading 7 would go looking
// for a test bug instead of a wiring one.
func TestRunRequireUnmetOutranksAFailingChild(t *testing.T) {
	_, _, twins := ledgerBench(t)
	twins.script(func(f *ledgerTwins) { f.record = false })
	argv := child(t, "fail")
	err, stderr := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--"}, argv...)...)
	wantExit(t, err, exitRequirementUnmet)
	if !strings.Contains(stderr, "veris: ✗ the run required service stripe at least 1 time(s) but the sandbox received it 0 time(s)\n") {
		t.Errorf("stderr lacks the unmet line:\n%s", stderr)
	}
}

// A sandbox that mixes a logged twin with one that keeps no log: a
// requirement on the unlogged twin, which the child did call, is the
// engine's verdict alone, and the two ledgers are compared over the twins
// that have one.
func TestRunRequireServiceOnATwinWithoutALogIsTheEnginesAlone(t *testing.T) {
	_, plane, twins := ledgerBench(t)
	twins.script(func(f *ledgerTwins) { f.noLog = true })
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(twins.withGithub(), time.Now().Add(30*time.Minute)) }
	})
	argv := child(t, "call")
	err, stderr := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--"}, argv...)...)
	wantExit(t, err, 0)
	sbInOrder(t, stderr,
		"veris: watermark stripe:— postgres:— github:0\n",
		"veris: the sandbox received 1 request(s):\n",
		"veris: the sandbox recorded 0 request(s) since the watermark\n",
		"veris: ✓ ledgers agree (0 = 0)\n",
	)
	for _, absent := range []string{"✗", "received it 0 time(s)", "cannot decide", "ledgers differ"} {
		if strings.Contains(stderr, absent) {
			t.Errorf("stderr should not carry %q:\n%s", absent, stderr)
		}
	}
}

// cacheSandboxAt is cacheSandbox with the stripe service at upstream, so
// the engine's receipt can be satisfied by a child that calls it.
func cacheSandboxAt(t *testing.T, id, upstream string) {
	t.Helper()
	snapshot := discovery.Snapshot{SandboxID: id, Status: "ready", APIBase: "http://control.test", FetchedAt: time.Now().UTC(),
		Services: []discovery.Service{{Name: "stripe", URL: upstream, Status: "ready",
			Routes: []routes.Entry{{Host: "api.stripe.com"}}}}}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(discovery.Dir(), "sandboxes", id+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A ledger that cannot be read leaves a service requirement to the engine:
// what the proxy forwarded meets it (exit 0, the line saying the ledger was
// not read) or refutes it (exit 3). A run without assertions keeps its own
// status, with the line that says why.
func TestRunTheEngineAnswersWhenTheLedgerIsUnreadable(t *testing.T) {
	t.Run("the twin's log fails after the watermark", func(t *testing.T) {
		_, _, twins := ledgerBench(t)
		twins.script(func(f *ledgerTwins) { f.failAfter = 1 })
		argv := child(t, "call")
		err, stderr := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--"}, argv...)...)
		wantExit(t, err, 0)
		sbInOrder(t, stderr,
			"veris: watermark stripe:0 postgres:—\n",
			"veris: the sandbox received 1 request(s):\n",
			"veris: ! the sandbox ledger could not be read (stripe: [500] the log is unavailable)\n",
			"veris: ✓ required stripe ≥1: saw 1 (engine; stripe's ledger could not be read)\n",
		)
		if strings.Contains(stderr, "cannot decide") {
			t.Errorf("the engine showed the count:\n%s", stderr)
		}

		// The engine forwarded nothing: refuted, in one line, exit 3.
		twins.script(func(f *ledgerTwins) { f.requestsCalls = 0 })
		argv = child(t, "silent")
		err, stderr = runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--"}, argv...)...)
		wantExit(t, err, exitRequirementUnmet)
		sbInOrder(t, stderr,
			"veris: ! the sandbox ledger could not be read (stripe: [500] the log is unavailable)\n",
			"veris: ✗ the run required service stripe at least 1 time(s) but the engine saw it 0 time(s) and stripe's ledger could not be read\n",
			"            — your tests never touched the sandbox (is STRIPE_API_BASE overridden in your test setup?)\n",
		)
		for _, absent := range []string{"cannot decide", "saw it 0 time(s)\n"} {
			if strings.Contains(stderr, absent) {
				t.Errorf("one verdict; stderr should not carry %q:\n%s", absent, stderr)
			}
		}

		// No assertion depended on it: the child's status, and the line.
		twins.script(func(f *ledgerTwins) { f.requestsCalls = 0 })
		argv = child(t, "call")
		err, stderr = runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--"}, argv...)...)
		wantExit(t, err, 0)
		if !strings.Contains(stderr, "veris: ! the sandbox ledger could not be read (stripe: [500] the log is unavailable)\n") {
			t.Errorf("stderr:\n%s", stderr)
		}
		if strings.Contains(stderr, "cannot decide") {
			t.Errorf("nothing depended on the ledger:\n%s", stderr)
		}
	})
	t.Run("no key to read the sandbox with", func(t *testing.T) {
		isolateHome(t)
		cacheSandboxAt(t, "sbx_nokey", sandbox(t))
		argv := child(t, "call")
		err, stderr := runLine(t, append([]string{"--sandbox", "sbx_nokey", "--listen", "127.0.0.1:0", "--require-service", "stripe", "--"}, argv...)...)
		wantExit(t, err, 0)
		sbInOrder(t, stderr,
			"veris: ! the sandbox ledger could not be read (no API key to read sandbox sbx_nokey with",
			"veris: ✓ required stripe ≥1: saw 1 (engine; the sandbox ledger could not be read)\n",
		)
		if strings.Contains(stderr, "watermark") {
			t.Errorf("no watermark can be taken without the sandbox:\n%s", stderr)
		}
	})
}

// A sandbox whose twins keep no log -- every service a data plane, or a
// control plane that serves no control URLs -- has no ledger, and the run
// reads exactly as it did before there was one: the engine's receipt, the
// engine's verdict, nothing about a watermark.
func TestRunWithoutAControlURLTwinPrintsNoLedger(t *testing.T) {
	_, plane, twins := ledgerBench(t)
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox {
			return readySandbox([]api.ServiceInfo{
				{Name: "stripe", Status: "ready", URL: twins.srv.URL + "/s/" + sbID + "/stripe",
					EnvHint: "STRIPE_API_BASE", Routes: []routes.Entry{{Host: "api.stripe.com"}}},
			}, time.Now().Add(30*time.Minute))
		}
	})
	argv := child(t, "call")
	err, stderr := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "github", "--"}, argv...)...)
	wantExit(t, err, exitRequirementUnmet)
	if !strings.Contains(stderr, "veris: the run required service github at least 1 time(s) but the sandbox saw it 0 time(s)\n") {
		t.Errorf("the engine's verdict should stand alone:\n%s", stderr)
	}
	for _, absent := range []string{"watermark", "since the watermark", "could not be read", "ledgers"} {
		if strings.Contains(stderr, absent) {
			t.Errorf("stderr should not mention %q:\n%s", absent, stderr)
		}
	}
	// A config-file run names no sandbox at all: the same silence.
	cfgPath := writeConfig(t, sandbox(t))
	err, stderr = runLine(t, append([]string{"--config", cfgPath, "--require-service", "stripe", "--"}, argv...)...)
	wantExit(t, err, 0)
	if strings.Contains(stderr, "watermark") || strings.Contains(stderr, "ledger") {
		t.Errorf("a config-file run has no ledger:\n%s", stderr)
	}
}

func TestRunReceiptFileShape(t *testing.T) {
	_, _, twins := ledgerBench(t)
	twins.script(func(f *ledgerTwins) {
		f.add("GET", "/veris/data", twin.TierControl, 200)
	})
	path := filepath.Join(t.TempDir(), "receipt.json")
	argv := child(t, "call")
	before := time.Now().Add(-time.Second)
	err, _ := runLine(t, append([]string{"--sandbox", sbID, "--listen", "127.0.0.1:0", "--require-service", "stripe", "--receipt", path, "--"}, argv...)...)
	wantExit(t, err, 0)
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("the receipt was not written: %v", rerr)
	}
	var r struct {
		SandboxID     string    `json:"sandbox_id"`
		EnvironmentID string    `json:"environment_id"`
		StartedAt     time.Time `json:"started_at"`
		FinishedAt    time.Time `json:"finished_at"`
		ExitCode      int       `json:"exit_code"`
		Engine        struct {
			Services  map[string]int64 `json:"services"`
			Callbacks []any            `json:"callbacks"`
		} `json:"engine"`
		Sandbox struct {
			Requests map[string]struct {
				Count  int64 `json:"count"`
				Faults int64 `json:"faults"`
			} `json:"requests"`
			ControlPlane int64 `json:"control_plane"`
			Deliveries   []any `json:"deliveries"`
		} `json:"sandbox"`
		Assertions []struct {
			Kind   string `json:"kind"`
			Target string `json:"target"`
			Want   int64  `json:"want"`
			Got    int64  `json:"got"`
			OK     bool   `json:"ok"`
		} `json:"assertions"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("receipt is not the expected JSON: %v\n%s", err, raw)
	}
	if r.SandboxID != sbID || r.EnvironmentID != ciID || r.ExitCode != 0 {
		t.Errorf("ids and exit: %+v", r)
	}
	if r.StartedAt.Before(before) || r.FinishedAt.Before(r.StartedAt) {
		t.Errorf("timestamps: started %s finished %s", r.StartedAt, r.FinishedAt)
	}
	if r.Engine.Services["stripe"] != 1 || r.Engine.Callbacks == nil {
		t.Errorf("engine half: %+v", r.Engine)
	}
	if r.Sandbox.Requests["stripe"].Count != 1 || r.Sandbox.ControlPlane != 0 || r.Sandbox.Deliveries == nil {
		t.Errorf("sandbox half: %+v", r.Sandbox)
	}
	if len(r.Assertions) != 1 || r.Assertions[0].Kind != "service" || r.Assertions[0].Target != "stripe" ||
		r.Assertions[0].Want != 1 || r.Assertions[0].Got != 1 || !r.Assertions[0].OK {
		t.Errorf("assertions: %+v", r.Assertions)
	}
	// Every key the contract names is present, whatever its value.
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	for _, k := range []string{"sandbox_id", "environment_id", "started_at", "finished_at", "exit_code", "engine", "sandbox", "assertions"} {
		if _, ok := keys[k]; !ok {
			t.Errorf("receipt lacks %q", k)
		}
	}
	if strings.Contains(string(raw), sbTestKey) {
		t.Error("the receipt must never carry the key")
	}
}

// --fresh is up, run and down in one process: the environment's sandbox is
// deployed as up deploys it (data files included), the run happens against
// it, and it is deleted afterwards whatever the run's outcome -- unless the
// outcome is indeterminate or --keep was asked, when it is kept and
// remembered.
func TestRunFreshLifecycle(t *testing.T) {
	fresh := func(t *testing.T, extra []string, role string) (*bench, *sandboxPlane, *ledgerTwins, error, string) {
		t.Helper()
		b, plane, twins := ledgerBench(t)
		ciProject(t, b, customersJSON)
		argv := child(t, role)
		line := append([]string{"--fresh", "--listen", "127.0.0.1:0"}, extra...)
		err, stderr := runLine(t, append(append(line, "--"), argv...)...)
		return b, plane, twins, err, stderr
	}

	t.Run("deploys, seeds, runs, asserts and deletes", func(t *testing.T) {
		b, plane, _, err, stderr := fresh(t, []string{"--require-service", "stripe"}, "call")
		wantExit(t, err, 0)
		sbInOrder(t, stderr,
			"Starting 'ci' (checkout-ci: stripe, postgres) · boot bundle · ttl 20 min\n",
			"✓ Sandbox created: "+sbID+"\n",
			"✓ Added data/customers.json: stripe customers 1, payment_methods 1\n",
			"veris: sandbox ready sandbox_id="+sbID+" (2/2 twins routable, ",
			"veris: watermark stripe:0 postgres:—\n",
			"veris: the sandbox received 1 request(s):\n",
			"veris: the sandbox recorded 1 request(s) since the watermark:\n",
			"veris: ✓ required stripe ≥1: saw 1   ✓ ledgers agree (1 = 1)\n",
			"veris: sandbox deleted "+sbID+" (",
		)
		if got := plane.deletedIDs(); len(got) != 1 || got[0] != ciID+"/"+sbID {
			t.Errorf("deleted %v, want the fresh sandbox once", got)
		}
		if req := plane.createdReq(); req == nil || req.TTLMinutes == nil || *req.TTLMinutes != 20 {
			t.Errorf("the environment config's ttl should have been sent: %+v", req)
		}
		if ptr := sbPointer(t, b); ptr != nil {
			t.Errorf("a fresh sandbox must not become the folder's pointer: %+v", ptr)
		}
		if strings.Contains(stderr, "this folder") {
			t.Errorf("a fresh run routes at its own sandbox, never the folder's:\n%s", stderr)
		}
	})
	t.Run("--ttl beats the config", func(t *testing.T) {
		_, plane, _, err, _ := fresh(t, []string{"--ttl", "5"}, "call")
		wantExit(t, err, 0)
		if req := plane.createdReq(); req == nil || req.TTLMinutes == nil || *req.TTLMinutes != 5 {
			t.Errorf("--ttl 5 should have been sent: %+v", req)
		}
	})
	t.Run("an empty ledger is exit 3 and the sandbox still goes", func(t *testing.T) {
		_, plane, _, err, stderr := fresh(t, nil, "silent")
		wantExit(t, err, exitRequirementUnmet)
		sbInOrder(t, stderr,
			"veris: the sandbox recorded 0 request(s) since the watermark\n",
			"veris: ✗ this run deployed a sandbox and the sandbox recorded nothing since the watermark\n",
			"veris: sandbox deleted "+sbID,
		)
		if got := plane.deletedIDs(); len(got) != 1 {
			t.Errorf("deleted %v", got)
		}
	})
	t.Run("a failing child that never reached the sandbox is exit 3 and the sandbox still goes", func(t *testing.T) {
		// The child's own 7 is outranked: a fresh run's default requirement
		// (a non-empty ledger) is unmet, and that verdict is the one a
		// harness needs.
		_, plane, _, err, stderr := fresh(t, nil, "fail")
		wantExit(t, err, exitRequirementUnmet)
		if got := plane.deletedIDs(); len(got) != 1 {
			t.Errorf("deleted %v", got)
		}
		if !strings.Contains(stderr, "veris: sandbox deleted "+sbID) {
			t.Errorf("stderr:\n%s", stderr)
		}
	})
	t.Run("--keep leaves it running as this folder's", func(t *testing.T) {
		b, plane, _, err, stderr := fresh(t, []string{"--keep"}, "call")
		wantExit(t, err, 0)
		if got := plane.deletedIDs(); len(got) != 0 {
			t.Errorf("--keep must not delete: %v", got)
		}
		if !strings.Contains(stderr, "veris: sandbox "+sbID+" kept (--keep)\n") {
			t.Errorf("stderr:\n%s", stderr)
		}
		if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID || ptr.EnvironmentID != ciID {
			t.Errorf("a kept sandbox is remembered: %+v", ptr)
		}
	})
	t.Run("an indeterminate outcome keeps the world", func(t *testing.T) {
		b, plane, twins := ledgerBench(t)
		ciProject(t, b, customersJSON)
		argv := child(t, "call")
		// The watermark is the first read of the log; the seeding before it
		// reads nothing. The after-read is the second, and fails: the fresh
		// run's own rule -- that the sandbox recorded anything -- cannot be
		// decided, and the engine has no say in it.
		twins.script(func(f *ledgerTwins) { f.failAfter = 1 })
		err, stderr := runLine(t, append([]string{"--fresh", "--listen", "127.0.0.1:0", "--"}, argv...)...)
		wantExit(t, err, exitIndeterminate)
		if got := plane.deletedIDs(); len(got) != 0 {
			t.Errorf("exit 4 must keep the sandbox: %v", got)
		}
		if !strings.Contains(stderr, "veris: ! sandbox "+sbID+" is kept: the run's outcome is indeterminate") {
			t.Errorf("stderr:\n%s", stderr)
		}
		if ptr := sbPointer(t, b); ptr == nil || ptr.ID != sbID {
			t.Errorf("a kept sandbox is remembered: %+v", ptr)
		}
	})
	t.Run("a sandbox that fails to come up is torn down", func(t *testing.T) {
		b, plane, _ := ledgerBench(t)
		ciProject(t, b, customersJSON)
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox {
				sb := readySandbox(nil, time.Now().Add(30*time.Minute))
				sb.Status, sb.FailureReason = api.StatusFailed, "image pull failed"
				return sb
			}
		})
		argv := child(t, "call")
		err, stderr := runLine(t, append([]string{"--fresh", "--listen", "127.0.0.1:0", "--"}, argv...)...)
		var reported printedError
		if !errors.As(err, &reported) || reported.code != 1 {
			t.Fatalf("err = %v, want the printed exit 1", err)
		}
		sbInOrder(t, stderr, "✗ Sandbox "+sbID+" failed: image pull failed\n", "veris: sandbox deleted "+sbID)
		if got := plane.deletedIDs(); len(got) != 1 {
			t.Errorf("deleted %v, want the failed sandbox", got)
		}
		if strings.Contains(stderr, "watermark") {
			t.Errorf("nothing ran:\n%s", stderr)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		b, plane, _ := ledgerBench(t)
		ciProject(t, b, customersJSON)
		for _, tc := range []struct{ line, want string }{
			{"--fresh --sandbox " + sbID, "--fresh deploys a sandbox of its own and --sandbox attaches"},
			{"--fresh --config proxy.json", "--fresh deploys a sandbox and --config routes"},
			{"--fresh --environment " + ciID, "--fresh and --environment both deploy a sandbox"},
			{"--keep", "--keep only applies to a sandbox --fresh deploys"},
			{"--ttl 5", "--ttl only applies to a sandbox --fresh deploys"},
		} {
			err := cmdRun(append(strings.Fields(tc.line), "--", "true"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: err = %v, want %q", tc.line, err, tc.want)
			}
		}
		if req := plane.createdReq(); req != nil {
			t.Error("a refused line must deploy nothing")
		}
	})
}
