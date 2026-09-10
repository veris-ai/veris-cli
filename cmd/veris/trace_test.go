package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/twin"
)

// sinceMode is how a fake twin treats since_id: as the twins with PR #1052
// do, as a twin whose FastAPI drops the unknown query parameter, or as one
// that validates its query and refuses the parameter with a 422.
type sinceMode int

const (
	sinceSupported sinceMode = iota
	sinceIgnored
	sinceRefused
)

// traceFake serves GET /veris/requests for a stripe and a github twin
// under /s/<sandbox>/<twin>, and FastAPI's 404 for postgres, which keeps no
// trace. Rows are scripted per twin; every query is recorded per twin.
type traceFake struct {
	srv *httptest.Server
	mu  sync.Mutex

	rows    map[string][]twin.Request // per twin, any order
	since   sinceMode
	queries map[string][]string // raw query strings seen, per twin
	polls   int                 // GET /veris/requests served, every twin
	// onPoll, when set, runs under the lock before the nth poll is answered
	// (1-based), so a follow test can add rows between polls.
	onPoll func(f *traceFake, n int)
}

func newTraceTwins(t *testing.T) *traceFake {
	t.Helper()
	f := &traceFake{rows: map[string][]twin.Request{}, queries: map[string][]string{}}
	mux := http.NewServeMux()
	for _, name := range []string{"stripe", "github"} {
		name := name
		mux.HandleFunc("GET /s/"+sbID+"/"+name+"/veris/requests", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.polls++
			if f.onPoll != nil {
				f.onPoll(f, f.polls)
			}
			f.queries[name] = append(f.queries[name], r.URL.RawQuery)
			q := r.URL.Query()
			if q.Get("since_id") != "" && f.since == sinceRefused {
				sbJSON(w, 422, map[string]any{"detail": []map[string]any{{
					"loc": []any{"query", "since_id"}, "msg": "Extra inputs are not permitted", "type": "extra_forbidden"}}})
				return
			}
			since := -1
			if q.Get("since_id") != "" && f.since == sinceSupported {
				since, _ = strconv.Atoi(q.Get("since_id"))
			}
			limit := 50
			if q.Get("limit") != "" {
				limit, _ = strconv.Atoi(q.Get("limit"))
			}
			var out []twin.Request
			for _, row := range f.rows[name] {
				if since >= 0 && row.ID <= since {
					continue
				}
				if tier := q.Get("tier"); tier != "" && row.Tier != tier {
					continue
				}
				out = append(out, row)
			}
			asc := q.Get("order") == "asc"
			sort.Slice(out, func(i, j int) bool { return (out[i].ID < out[j].ID) == asc })
			if len(out) > limit {
				out = out[:limit]
			}
			if out == nil {
				out = []twin.Request{}
			}
			sbJSON(w, 200, map[string]any{"requests": out})
		})
	}
	mux.HandleFunc("/s/"+sbID+"/postgres/veris/requests", func(w http.ResponseWriter, r *http.Request) {
		sbJSON(w, 404, map[string]any{"detail": "Not Found"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected twin request %s %s", r.Method, r.URL.RequestURI())
		sbJSON(w, 404, map[string]any{"detail": "Not Found"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *traceFake) script(fn func(f *traceFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *traceFake) queriesOf(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries[name]...)
}

func (f *traceFake) control(name string) string {
	return f.srv.URL + "/s/" + sbID + "/" + name
}

// services is stripe and github as HTTP twins and postgres as a data-plane
// twin with a control URL of its own (health, schema, seed; no trace).
func (f *traceFake) services() []api.ServiceInfo {
	return []api.ServiceInfo{
		{Name: "stripe", Status: "ready", URL: f.control("stripe"), ControlURL: f.control("stripe"), EnvHint: "STRIPE_API_BASE"},
		{Name: "github", Status: "ready", URL: f.control("github"), ControlURL: f.control("github"), EnvHint: "GITHUB_API_BASE"},
		{Name: "postgres", Status: "ready", URL: "postgresql://app:app@10.0.0.5:5432/sb?sslmode=require", ControlURL: f.control("postgres"), EnvHint: "DATABASE_URL"},
	}
}

// traceBench is a logged-in bench whose folder points at sbID, ready in ci
// with the fake twins' services. The plane is returned so a test can script
// its ledger route; left unscripted it answers 404, the pre-ledger control
// plane every twin-merge test below is written against.
func traceBench(t *testing.T, twins *traceFake) (*bench, *sandboxPlane) {
	t.Helper()
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	services := twins.services()
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(services, time.Now().Add(time.Hour)) }
	})
	return b, plane
}

func intp(n int) *int       { return &n }
func strp(s string) *string { return &s }

// req is one trace row: ts on the sandbox clock, the tier, and the HTTP
// facts; a nil status is a hang.
func req(id, ts int, tier, method, path string, status *int, ms int) twin.Request {
	return twin.Request{ID: id, TS: ts, Tier: tier, Method: method, Path: path, Status: status, DurationMS: ms}
}

// traceFixture is two twins' worth of rows on 2026-03-01: stripe ids 1..4,
// github ids 1..2, with one hang and one delivery.
func traceFixture(f *traceFake) {
	base := 1772355600 // 2026-03-01T09:00:00Z
	f.rows["stripe"] = []twin.Request{
		req(1, base+5, twin.TierHandler, "GET", "/v1/customers/cus_dev_ada", intp(200), 6),
		req(2, base+8, twin.TierFault, "POST", "/v1/charges", intp(402), 3004),
		req(3, base+8, twin.TierHandler, "POST", "/v1/payment_intents/pi_1/confirm", intp(200), 21),
		req(4, base+9, twin.TierDelivery, "POST", "https://odd-forest.example/hooks/stripe", intp(200), 143),
	}
	f.rows["github"] = []twin.Request{
		req(1, base+7, twin.TierHandler, "GET", "/repos/acme/app", intp(200), 12),
		req(2, base+8, twin.TierFault, "POST", "/repos/acme/app/issues", nil, 30000),
	}
}

func TestSandboxTraceMergesNewestFirst(t *testing.T) {
	twins := newTraceTwins(t)
	twins.script(traceFixture)
	traceBench(t, twins)

	t.Run("table", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout should be empty without --json, got:\n%s", stdout)
		}
		// Newest first by ts, then by id; the postgres twin's 404 is silence.
		sbInOrder(t, stderr,
			"  Time", "Twin", "Tier", "Method", "Path", "Status", "ms",
			"09:00:09.000", "stripe", "delivery", "POST", "https://odd-forest.example/hooks/stripe", "200", "143",
			"09:00:08.000", "stripe", "handler", "POST", "/v1/payment_intents/pi_1/confirm", "200", "21",
			"09:00:08.000", "github", "fault", "POST", "/repos/acme/app/issues", "—", "30000",
			"09:00:08.000", "stripe", "fault", "POST", "/v1/charges", "402", "3004",
			"09:00:07.000", "github", "handler", "GET", "/repos/acme/app", "200", "12",
			"09:00:05.000", "stripe", "handler", "GET", "/v1/customers/cus_dev_ada", "200", "6",
			"→ veris sandbox trace --body 4 --service stripe   (headers and bodies of one entry)",
		)
		if strings.Contains(stderr, "postgres") {
			t.Errorf("the postgres twin has no trace and should not be mentioned:\n%s", stderr)
		}
		for _, name := range []string{"stripe", "github"} {
			q := twins.queriesOf(name)
			if len(q) != 1 || q[0] != "limit=50&order=desc" {
				t.Errorf("%s was asked %q, want one limit=50&order=desc", name, q)
			}
		}
	})

	t.Run("limit cuts the merge and tier is sent", func(t *testing.T) {
		twins.script(func(f *traceFake) { f.queries = map[string][]string{} })
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--limit", "2", "--tier", "fault")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "/repos/acme/app/issues", "/v1/charges")
		if strings.Contains(stderr, "confirm") || strings.Contains(stderr, "cus_dev_ada") {
			t.Errorf("handler rows leaked through --tier fault:\n%s", stderr)
		}
		if q := twins.queriesOf("github"); len(q) != 1 || q[0] != "limit=2&order=desc&tier=fault" {
			t.Errorf("github was asked %q", q)
		}
	})

	t.Run("one twin", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--service", "github")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if strings.Contains(stderr, "stripe") {
			t.Errorf("--service github printed stripe rows:\n%s", stderr)
		}
		sbInOrder(t, stderr, "/repos/acme/app/issues", "/repos/acme/app", "→ veris sandbox trace --body 2   (headers")
	})

	t.Run("json", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--json", "--limit", "3")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("stdout is not a JSON list: %v\n%s", err, stdout)
		}
		if len(rows) != 3 {
			t.Fatalf("%d rows, want 3:\n%s", len(rows), stdout)
		}
		if rows[0]["twin"] != "stripe" || rows[0]["id"] != float64(4) || rows[0]["tier"] != "delivery" {
			t.Errorf("first row %v", rows[0])
		}
		// github's hang ties stripe's fault on ts and id; the twin name
		// breaks the tie, and its missing status is null, not 0.
		if rows[2]["twin"] != "github" || rows[2]["id"] != float64(2) || rows[2]["status"] != nil {
			t.Errorf("third row %v", rows[2])
		}
		if _, ok := rows[1]["request_body"]; !ok {
			t.Errorf("the raw fields are not carried: %v", rows[1])
		}
	})

	t.Run("nothing recorded", func(t *testing.T) {
		twins.script(func(f *traceFake) { f.rows = map[string][]twin.Request{} })
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--tier", "delivery")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "No requests recorded of tier delivery", "→ Next: veris run")
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		code, stdout, _ = runSandboxCLI(t, "sandbox", "trace", "--json")
		if code != 0 || strings.TrimSpace(stdout) != "[]" {
			t.Errorf("exit %d, stdout %q; want 0 and []", code, stdout)
		}
	})
}

func TestSandboxTraceSinceNegotiation(t *testing.T) {
	cases := []struct {
		name string
		mode sinceMode
		// wantQueries is what stripe is asked, in order.
		wantQueries []string
	}{
		{"the twin serves since_id", sinceSupported, []string{"limit=50&order=desc&since_id=2"}},
		{"the twin ignores since_id", sinceIgnored, []string{"limit=50&order=desc&since_id=2"}},
		{"the twin refuses since_id", sinceRefused, []string{"limit=50&order=desc&since_id=2", "limit=50&order=desc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			twins := newTraceTwins(t)
			twins.script(func(f *traceFake) { traceFixture(f); f.since = tc.mode })
			traceBench(t, twins)
			code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--since", "2", "--json")
			if code != 0 {
				t.Fatalf("exit %d, stderr:\n%s", code, stderr)
			}
			if got := twins.queriesOf("stripe"); strings.Join(got, " ") != strings.Join(tc.wantQueries, " ") {
				t.Errorf("stripe was asked %q, want %q", got, tc.wantQueries)
			}
			var rows []map[string]any
			if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
				t.Fatal(err)
			}
			// Rows above id 2 in each twin, whatever the twin did with the
			// parameter: stripe 4 and 3, nothing from github.
			var ids []string
			for _, r := range rows {
				ids = append(ids, r["twin"].(string)+":"+strconv.Itoa(int(r["id"].(float64))))
			}
			if got := strings.Join(ids, " "); got != "stripe:4 stripe:3" {
				t.Errorf("rows %s, want stripe:4 stripe:3", got)
			}
			if strings.Contains(stderr, "422") || strings.Contains(stderr, "could not read") {
				t.Errorf("the refusal leaked:\n%s", stderr)
			}
		})
	}
}

// followFor runs `sandbox trace --follow` with the given extra args and
// ends it once the twins have served polls requests, the way Ctrl-C would.
func followFor(t *testing.T, twins *traceFake, polls int, args ...string) (int, string, string) {
	t.Helper()
	interval, mk := traceFollowInterval, traceFollowContext
	traceFollowInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	traceFollowContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { traceFollowInterval, traceFollowContext = interval, mk; cancel() })
	twins.script(func(f *traceFake) {
		prev := f.onPoll
		f.onPoll = func(f *traceFake, n int) {
			if prev != nil {
				prev(f, n)
			}
			if n >= polls {
				cancel()
			}
		}
	})
	return runSandboxCLI(t, append([]string{"sandbox", "trace", "--follow"}, args...)...)
}

func TestSandboxTraceFollow(t *testing.T) {
	t.Run("prints only what arrived, oldest first", func(t *testing.T) {
		twins := newTraceTwins(t)
		base := 1772355600
		twins.script(func(f *traceFake) {
			f.rows["stripe"] = []twin.Request{req(1, base+5, twin.TierHandler, "GET", "/v1/customers/cus_1", intp(200), 6)}
			f.rows["github"] = nil
			f.onPoll = func(f *traceFake, n int) {
				// The initial read is polls 1 and 2 (one per twin). Rows land
				// before the second and third follow rounds.
				switch n {
				case 5:
					f.rows["stripe"] = append(f.rows["stripe"],
						req(2, base+20, twin.TierFault, "POST", "/v1/charges", intp(402), 3004),
						req(3, base+21, twin.TierHandler, "POST", "/v1/refunds", intp(200), 9))
					f.rows["github"] = append(f.rows["github"],
						req(1, base+19, twin.TierHandler, "GET", "/repos/acme/app", intp(200), 12))
				case 7:
					f.rows["stripe"] = append(f.rows["stripe"],
						req(4, base+30, twin.TierDelivery, "POST", "https://odd-forest.example/hooks/stripe", nil, 0))
				}
			}
		})
		traceBench(t, twins)
		// 2 initial + 3 empty-or-not rounds of 2 + 2 more: ten polls, then
		// the context is cancelled and the loop returns 0.
		code, stdout, stderr := followFor(t, twins, 10)
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		sbInOrder(t, stderr,
			"  Time", "09:00:05.000", "/v1/customers/cus_1",
			"09:00:19.000", "github", "/repos/acme/app",
			"09:00:20.000", "stripe", "/v1/charges",
			"09:00:21.000", "stripe", "/v1/refunds",
			"09:00:30.000", "stripe", "delivery", "https://odd-forest.example/hooks/stripe", "—",
		)
		for _, once := range []string{"/v1/customers/cus_1", "/v1/charges", "/v1/refunds", "/hooks/stripe"} {
			if n := strings.Count(stderr, once); n != 1 {
				t.Errorf("%s printed %d times, want once:\n%s", once, n, stderr)
			}
		}
		// Every follow poll of stripe carries the watermark, which advances.
		q := twins.queriesOf("stripe")
		if len(q) < 4 {
			t.Fatalf("stripe was asked only %q", q)
		}
		if q[0] != "limit=50&order=desc" {
			t.Errorf("initial read %q", q[0])
		}
		if q[1] != "limit=1000&order=desc&since_id=1" {
			t.Errorf("first follow poll %q", q[1])
		}
		if last := q[len(q)-1]; last != "limit=1000&order=desc&since_id=4" {
			t.Errorf("last follow poll %q", last)
		}
	})

	t.Run("a twin the first batch's cut dropped is not replayed", func(t *testing.T) {
		twins := newTraceTwins(t)
		base := 1772355600
		twins.script(func(f *traceFake) {
			f.rows["github"] = []twin.Request{
				req(1, base+1, twin.TierHandler, "GET", "/repos/acme/app", intp(200), 12),
				req(2, base+2, twin.TierHandler, "GET", "/repos/acme/app/issues", intp(200), 15),
			}
			for i := 1; i <= 10; i++ {
				f.rows["stripe"] = append(f.rows["stripe"],
					req(i, base+10+i, twin.TierHandler, "GET", fmt.Sprintf("/v1/customers/cus_%02d", i), intp(200), 6))
			}
		})
		traceBench(t, twins)
		// 2 initial + 2 follow rounds of 2: every stripe row is newer than
		// github's, so --limit 3 shows stripe 8..10 and nothing of github,
		// whose two rows were still SEEN and must not arrive on the poll.
		code, stdout, stderr := followFor(t, twins, 6, "--limit", "3")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		sbInOrder(t, stderr, "  Time", "/v1/customers/cus_08", "/v1/customers/cus_09", "/v1/customers/cus_10",
			"→ veris sandbox trace --body 10 --service stripe")
		if n := strings.Count(stderr, "/v1/customers/cus_"); n != 3 {
			t.Errorf("%d stripe rows printed, want the 3 of the first batch:\n%s", n, stderr)
		}
		if strings.Contains(stderr, "/repos/acme/app") {
			t.Errorf("github's history was replayed as arrivals:\n%s", stderr)
		}
		if q := twins.queriesOf("github"); len(q) < 2 || q[1] != "limit=1000&order=desc&since_id=2" {
			t.Errorf("github's first follow poll must start above its newest id as read, got %q", q)
		}
		if q := twins.queriesOf("stripe"); len(q) < 2 || q[1] != "limit=1000&order=desc&since_id=10" {
			t.Errorf("stripe's first follow poll %q", q)
		}
	})

	t.Run("json is one row per line", func(t *testing.T) {
		twins := newTraceTwins(t)
		base := 1772355600
		twins.script(func(f *traceFake) {
			f.rows["stripe"] = []twin.Request{req(1, base+5, twin.TierHandler, "GET", "/v1/customers/cus_1", intp(200), 6)}
			f.onPoll = func(f *traceFake, n int) {
				if n == 3 {
					f.rows["stripe"] = append(f.rows["stripe"], req(2, base+20, twin.TierFault, "POST", "/v1/charges", intp(402), 3004))
				}
			}
		})
		traceBench(t, twins)
		code, stdout, stderr := followFor(t, twins, 6, "--json", "--service", "stripe")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) != 2 {
			t.Fatalf("%d lines, want 2:\n%s", len(lines), stdout)
		}
		for i, want := range []string{`"id":1`, `"id":2`} {
			var row map[string]any
			if err := json.Unmarshal([]byte(lines[i]), &row); err != nil {
				t.Errorf("line %d is not JSON: %v", i, err)
			}
			if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], `"twin":"stripe"`) {
				t.Errorf("line %d = %s, want %s from stripe", i, lines[i], want)
			}
		}
	})

	t.Run("a refused since_id is negotiated once", func(t *testing.T) {
		twins := newTraceTwins(t)
		base := 1772355600
		twins.script(func(f *traceFake) {
			f.since = sinceRefused
			f.rows["stripe"] = []twin.Request{req(1, base+5, twin.TierHandler, "GET", "/v1/customers/cus_1", intp(200), 6)}
			f.onPoll = func(f *traceFake, n int) {
				if n == 4 {
					f.rows["stripe"] = append(f.rows["stripe"], req(2, base+20, twin.TierFault, "POST", "/v1/charges", intp(402), 3004))
				}
			}
		})
		traceBench(t, twins)
		code, _, stderr := followFor(t, twins, 6, "--service", "stripe")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "/v1/customers/cus_1", "/v1/charges")
		if n := strings.Count(stderr, "/v1/charges"); n != 1 {
			t.Errorf("/v1/charges printed %d times:\n%s", n, stderr)
		}
		q := twins.queriesOf("stripe")
		refused := 0
		for _, s := range q {
			if strings.Contains(s, "since_id") {
				refused++
			}
		}
		if refused != 1 {
			t.Errorf("since_id was sent %d times, want once (then remembered as refused): %q", refused, q)
		}
		if strings.Contains(stderr, "422") {
			t.Errorf("the refusal leaked:\n%s", stderr)
		}
	})
}

func TestSandboxTraceBody(t *testing.T) {
	twins := newTraceTwins(t)
	twins.script(func(f *traceFake) {
		traceFixture(f)
		f.rows["stripe"][1].RequestHeaders = strp(`{"authorization":"Bearer [redacted]","content-type":"application/x-www-form-urlencoded","host":"api.stripe.com"}`)
		f.rows["stripe"][1].RequestBody = strp("amount=2000&currency=usd")
		f.rows["stripe"][1].ResponseHeaders = strp(`{"content-type":"application/json"}`)
		f.rows["stripe"][1].ResponseBody = strp(`{"error":{"code":"card_declined","type":"card_error"}}`)
		f.rows["github"][1].RequestBody = strp("")
	})
	traceBench(t, twins)

	t.Run("renders headers and bodies", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "2", "--service", "stripe")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		sbInOrder(t, stderr,
			"stripe #2  fault  POST /v1/charges → 402  3004 ms  09:00:08.000",
			"Request headers",
			"  authorization: Bearer [redacted]",
			"  content-type: application/x-www-form-urlencoded",
			"  host: api.stripe.com",
			"Request body",
			"  amount=2000&currency=usd",
			"Response headers",
			"  content-type: application/json",
			"Response body",
			"  {", `"error": {`, `"code": "card_declined"`, "  }",
		)
		// Found in one query: since_id answered the exact row.
		if q := twins.queriesOf("stripe"); len(q) != 1 || q[0] != "limit=1&order=asc&since_id=1" {
			t.Errorf("stripe was asked %q", q)
		}
	})

	t.Run("a hang has no response, and ids are per twin", func(t *testing.T) {
		twins.script(func(f *traceFake) { f.since = sinceIgnored; f.queries = map[string][]string{} })
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "2")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"! entry 2 exists in stripe and github; showing stripe (pass --service to choose)",
			"stripe #2  fault",
		)
		// With since_id ignored the asc/limit=1 answer is the oldest row,
		// not the one asked for, and the newest page is scanned instead.
		if q := twins.queriesOf("github"); len(q) != 2 || q[1] != "limit=1000&order=desc" {
			t.Errorf("github was asked %q", q)
		}
		code, _, stderr = runSandboxCLI(t, "sandbox", "trace", "--body", "2", "--service", "github")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"github #2  fault  POST /repos/acme/app/issues → —  30000 ms",
			"Request headers", "  (none)",
			"Request body", "  (empty)",
			"Response headers", "  (none)",
			"Response body", "  (none)",
		)
	})

	t.Run("json", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "2", "--service", "stripe", "--json")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(stdout), &row); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
		}
		if row["twin"] != "stripe" || row["id"] != float64(2) || row["request_body"] != "amount=2000&currency=usd" {
			t.Errorf("row %v", row)
		}
	})

	t.Run("no such entry", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "99")
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "✗ No trace entry 99 in the twins asked", "→ Next: veris sandbox trace")
	})
}

func TestSandboxTraceRefusals(t *testing.T) {
	twins := newTraceTwins(t)
	twins.script(traceFixture)
	traceBench(t, twins)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad tier", []string{"--tier", "routed"}, "--tier must be handler, fault, control or delivery (got 'routed')"},
		{"limit too high", []string{"--limit", "5000"}, "--limit must be between 1 and 1000 (got 5000)"},
		{"body with follow", []string{"--body", "2", "--follow"}, "--body prints one entry; it cannot be combined with --follow"},
		{"unknown twin", []string{"--service", "shopify"}, "✗ No twin named 'shopify' in sandbox " + sbID + " (have: stripe, github, postgres)"},
		{"data-plane twin", []string{"--service", "postgres"}, "✗ postgres keeps no request trace (data plane)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runSandboxCLI(t, append([]string{"sandbox", "trace"}, tc.args...)...)
			if code != 1 {
				t.Errorf("exit %d, want 1:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout:\n%s", stdout)
			}
		})
	}
	// Data-plane twins without a control URL are simply not asked.
	t.Run("a twin with no control URL is skipped", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--service", "github", "--limit", "1")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestSandboxTraceTwinFailureIsAWarning(t *testing.T) {
	twins := newTraceTwins(t)
	twins.script(traceFixture)
	b, _ := traceBench(t, twins)
	// github's control URL points at a closed port: the read fails, stripe
	// still prints, and the failure is one ! line.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	plane := newSandboxPlane(t)
	services := twins.services()
	services[1].ControlURL = dead.URL + "/s/" + sbID + "/github"
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(services, time.Now().Add(time.Hour)) }
	})
	b.global(cfg.Global{
		ActiveProfile: "default",
		Profiles:      map[string]cfg.Profile{"default": {APIBase: plane.srv.URL, APIKey: sbTestKey}},
	})
	code, _, stderr := runSandboxCLI(t, "sandbox", "trace")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	sbInOrder(t, stderr, "! github: could not read the trace: cannot reach the twin at", "/v1/charges")
}

// --- the sandbox ledger -----------------------------------------------------
//
// The tests above drive the per-twin merge, which is what trace does against
// a control plane that keeps no ledger (newSandboxPlane answers its ledger
// routes 404 until a test scripts them). The ones below script them, and
// hold trace to the ledger's own shapes: one sandbox-wide seq per event,
// sources that may not all be legible, and a follow that drains.

// lgAt is an event's wall clock as the fixtures write it, and lgLocal is how
// the table must render it: the reader's own zone, to the millisecond.
const (
	lgAt1 = "2026-03-01T09:00:05.120Z"
	lgAt2 = "2026-03-01T09:00:08.355Z"
	lgAt3 = "2026-03-01T09:00:09.900Z"
	lgAt4 = "2026-03-01T09:00:11.010Z"
)

func lgLocal(t *testing.T, at string) string {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatalf("fixture time %q: %v", at, err)
	}
	return ts.Local().Format("15:04:05.000")
}

// lgEvent is one ledger event with the whole common header, so a test also
// proves the fields trace does not model survive --json.
func lgEvent(seq int64, at, source, protocol, plane, typ, tier string, ms *int64,
	outcome map[string]any, payload map[string]any) map[string]any {
	src := map[string]any{"kind": "service", "name": source, "protocol": protocol}
	if source == "" {
		src = map[string]any{"kind": "sandbox", "name": nil, "protocol": nil}
	}
	e := map[string]any{
		"seq": seq, "id": seq - 5000, "source": src,
		"plane": plane, "type": typ, "direction": "inbound", "tier": tier,
		"at": at, "world_time": "2026-03-01T09:00:00Z", "duration_ms": ms,
		"session":     nil,
		"origin":      map[string]any{"ip": "10.8.3.21", "application": nil, "user_agent": "python-httpx/0.27"},
		"actor":       map[string]any{"credential": "api_key", "ref": "acct_clinic_admin"},
		"state":       map[string]any{"before": 12, "after": 12, "mutating": false},
		"outcome":     outcome,
		"correlation": map[string]any{"request_id": "req-" + strconv.FormatInt(seq, 10), "caused_by": nil},
		"redacted":    []string{"http.request.headers.authorization"},
	}
	e[typ] = payload
	return e
}

func lgMS(ms int64) *int64 { return &ms }

func lgOK(code string) map[string]any {
	return map[string]any{"ok": true, "code": code, "fault": nil, "error": nil}
}

// lgFixture is four events of one sandbox, seqs 5103..5106: a stripe call,
// a hang, a postgres statement and the sandbox's own clock change.
func lgFixture() []map[string]any {
	return []map[string]any{
		lgEvent(5103, lgAt1, "stripe", "http", "vendor", "http", twin.TierHandler, lgMS(6), lgOK("200"),
			map[string]any{"method": "GET", "path": "/v1/customers/cus_dev_ada", "query": map[string]any{},
				"op": nil, "request": map[string]any{"bytes": 0, "truncated": false},
				"response": map[string]any{"status": 200, "bytes": 812, "truncated": false}}),
		lgEvent(5104, lgAt2, "stripe", "http", "vendor", "http", twin.TierFault, nil,
			map[string]any{"ok": false, "code": nil, "fault": map[string]any{"kind": "hang", "id": "flt_7"}, "error": nil},
			map[string]any{"method": "POST", "path": "/v1/charges", "query": map[string]any{},
				"op": nil, "request": map[string]any{"bytes": 24, "truncated": false},
				"response": map[string]any{"status": nil, "bytes": 0, "truncated": false}}),
		lgEvent(5105, lgAt3, "postgres", "postgres", "vendor", "sql", twin.TierHandler, lgMS(3), lgOK(""),
			map[string]any{"database": "app", "role": "app", "application": "psycopg",
				"statement":   "UPDATE invoices SET status = 'paid' WHERE id = $1 AND tenant = $2 RETURNING id",
				"command_tag": "UPDATE 1", "protocol_phase": "execute", "rows": 1}),
		lgEvent(5106, lgAt4, "", "", "world", "world", twin.TierControl, nil, lgOK(""),
			map[string]any{"event": "clock", "detail": map[string]any{"mode": "frozen"}, "by": "api:clock"}),
	}
}

func lgSeq(e map[string]any) int64 {
	switch v := e["seq"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

// lgSources is the envelope's sources: every member and the sandbox itself,
// each with the state of its own ledger.
func lgSources(states map[string]string) []map[string]any {
	out := []map[string]any{}
	for _, name := range []string{"stripe", "github", "postgres"} {
		state := states[name]
		if state == "" {
			state = api.LedgerComplete
		}
		protocol := "http"
		if name == "postgres" {
			protocol = "postgres"
		}
		out = append(out, map[string]any{"kind": "service", "name": name, "protocol": protocol,
			"ledger": state, "last_seq": 5106, "lag_ms": 0, "error": nil})
	}
	state := states["sandbox"]
	if state == "" {
		state = api.LedgerComplete
	}
	out = append(out, map[string]any{"kind": "sandbox", "name": nil, "protocol": nil,
		"ledger": state, "last_seq": 5106, "lag_ms": 0, "error": nil})
	return out
}

// lgServe answers the ledger route off a slice the test can add to between
// polls, honouring after_seq, type, dir and limit, and cutting every page to
// pageSize so a drain can be observed. states scripts the sources' health.
func lgServe(all *[]map[string]any, pageSize int, states map[string]string) func(url.Values) (int, any) {
	return func(q url.Values) (int, any) {
		var after int64
		if v := q.Get("after_seq"); v != "" {
			after, _ = strconv.ParseInt(v, 10, 64)
		}
		limit := 200
		if v := q.Get("limit"); v != "" {
			limit, _ = strconv.Atoi(v)
		}
		if pageSize > 0 && pageSize < limit {
			limit = pageSize
		}
		var watermark int64
		kept := []map[string]any{}
		for _, e := range *all {
			watermark = max(watermark, lgSeq(e))
			if lgSeq(e) <= after {
				continue
			}
			if want := q.Get("type"); want != "" && e["type"] != want {
				continue
			}
			kept = append(kept, e)
		}
		asc := q.Get("dir") != "desc"
		sort.SliceStable(kept, func(i, j int) bool { return (lgSeq(kept[i]) < lgSeq(kept[j])) == asc })
		hasMore := false
		if len(kept) > limit {
			kept, hasMore = kept[:limit], true
		}
		return 200, map[string]any{
			"format": api.LedgerFormat, "sandbox_id": sbID,
			"clock":   map[string]any{"mode": "frozen", "world_time": "2026-03-01T09:00:00Z", "offset_seconds": 0},
			"sources": lgSources(states),
			"events":  kept,
			"page": map[string]any{"order": q.Get("order"), "dir": q.Get("dir"), "limit": limit,
				"count": len(kept), "has_more": hasMore, "next_cursor": nil, "watermark_seq": watermark},
		}
	}
}

// traceLedgerBench is traceBench with the ledger route scripted off events.
func traceLedgerBench(t *testing.T, events *[]map[string]any, pageSize int, states map[string]string) *sandboxPlane {
	t.Helper()
	twins := newTraceTwins(t)
	twins.script(traceFixture)
	_, plane := traceBench(t, twins)
	plane.script(func(p *sandboxPlane) { p.ledger = lgServe(events, pageSize, states) })
	return plane
}

func TestSandboxTraceLedger(t *testing.T) {
	t.Run("table", func(t *testing.T) {
		events := lgFixture()
		plane := traceLedgerBench(t, &events, 0, nil)
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout should be empty without --json, got:\n%s", stdout)
		}
		// Newest first, one stream: the world's own event, a statement, a
		// hang and a call, each with its sandbox-wide seq.
		sbInOrder(t, stderr,
			"  Seq", "At", "Source", "Type", "Plane", "Event", "Outcome", "ms",
			"5106", lgLocal(t, lgAt4), "sandbox", "world", "world", "clock", "—",
			"5105", lgLocal(t, lgAt3), "postgres", "sql", "vendor", "UPDATE 1", "UPDATE invoices SET status", "3",
			"5104", lgLocal(t, lgAt2), "stripe", "http", "vendor", "POST /v1/charges", "hang", "—",
			"5103", lgLocal(t, lgAt1), "stripe", "http", "vendor", "GET /v1/customers/cus_dev_ada", "200", "6",
			"→ veris sandbox trace --body 5106   (request and response of one event)",
		)
		// seq is sandbox-wide, so the hint names no twin.
		if strings.Contains(stderr, "--body 5106 --service") {
			t.Errorf("the ledger's --body hint must not name a source:\n%s", stderr)
		}
		if q := plane.ledgerAsked(); len(q) != 1 || q[0] != "detail=summary&dir=desc&limit=50&order=time" {
			t.Errorf("the ledger was asked %q", q)
		}
	})

	t.Run("the filters ride along", func(t *testing.T) {
		events := lgFixture()
		plane := traceLedgerBench(t, &events, 0, nil)
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--since", "5100", "--limit", "10",
			"--source", "postgres", "--type", "sql", "--plane", "vendor", "--tier", "handler",
			"--session", "66e0a1b2.1f3a", "--mutating")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		want := "after_seq=5100&detail=summary&dir=desc&limit=10&mutating=true&order=time&plane=vendor" +
			"&session=66e0a1b2.1f3a&source=postgres&tier=handler&type=sql"
		if q := plane.ledgerAsked(); len(q) != 1 || q[0] != want {
			t.Errorf("the ledger was asked\n%q\nwant\n%q", q, want)
		}
	})

	t.Run("--service names the source too", func(t *testing.T) {
		events := lgFixture()
		plane := traceLedgerBench(t, &events, 0, nil)
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--service", "stripe")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if q := plane.ledgerAsked(); len(q) != 1 || !strings.Contains(q[0], "source=stripe") {
			t.Errorf("the ledger was asked %q, want source=stripe", q)
		}
	})

	t.Run("json carries what this client does not model", func(t *testing.T) {
		events := lgFixture()
		traceLedgerBench(t, &events, 0, nil)
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--json", "--limit", "2")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("stdout is not a JSON list: %v\n%s", err, stdout)
		}
		if len(rows) != 2 {
			t.Fatalf("%d events, want 2:\n%s", len(rows), stdout)
		}
		if rows[0]["seq"] != float64(5106) || rows[1]["seq"] != float64(5105) {
			t.Errorf("newest first is broken: %v %v", rows[0]["seq"], rows[1]["seq"])
		}
		for _, field := range []string{"origin", "correlation", "redacted", "world_time", "direction"} {
			if _, ok := rows[0][field]; !ok {
				t.Errorf("%s was dropped from --json: %v", field, rows[0])
			}
		}
	})

	t.Run("nothing recorded", func(t *testing.T) {
		var events []map[string]any
		traceLedgerBench(t, &events, 0, nil)
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--type", "sql", "--mutating")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "No events recorded of type sql that changed the world", "→ Next: veris run")
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		code, stdout, _ = runSandboxCLI(t, "sandbox", "trace", "--json")
		if code != 0 || strings.TrimSpace(stdout) != "[]" {
			t.Errorf("exit %d, stdout %q; want 0 and []", code, stdout)
		}
	})

	t.Run("a source that keeps no ledger is a warning", func(t *testing.T) {
		events := lgFixture()
		traceLedgerBench(t, &events, 0, map[string]string{"github": api.LedgerUnavailable, "postgres": api.LedgerPartial})
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "! github: no ledger", "! postgres: ledger partial", "  Seq", "5106")
	})

	t.Run("nothing legible at all is a failure", func(t *testing.T) {
		var events []map[string]any
		traceLedgerBench(t, &events, 0, map[string]string{
			"stripe": api.LedgerUnavailable, "github": api.LedgerUnavailable,
			"postgres": api.LedgerUnavailable, "sandbox": api.LedgerUnavailable})
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace")
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "! stripe: no ledger", "! sandbox: no ledger")
		if strings.Contains(stderr, "No events recorded") {
			t.Errorf("an unreadable sandbox must not read as an empty one:\n%s", stderr)
		}
	})
}

// ledgerFollowFor runs `sandbox trace --follow` against a scripted ledger and
// ends it once the route has been read polls times, the way Ctrl-C would.
func ledgerFollowFor(t *testing.T, plane *sandboxPlane, polls int, args ...string) (int, string, string) {
	t.Helper()
	interval, mk := traceFollowInterval, traceFollowContext
	traceFollowInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	traceFollowContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { traceFollowInterval, traceFollowContext = interval, mk; cancel() })
	plane.script(func(p *sandboxPlane) {
		prev := p.onLedger
		p.onLedger = func(p *sandboxPlane, n int) {
			if prev != nil {
				prev(p, n)
			}
			if n >= polls {
				cancel()
			}
		}
	})
	return runSandboxCLI(t, append([]string{"sandbox", "trace", "--follow"}, args...)...)
}

func TestSandboxTraceLedgerFollow(t *testing.T) {
	t.Run("drains each tick and advances the watermark", func(t *testing.T) {
		events := lgFixture()[:1] // seq 5103 only
		plane := traceLedgerBench(t, &events, 2, nil)
		rest := lgFixture()[1:]
		// The three remaining events land before the second poll, and the
		// page size of 2 means the tick must drain twice to print them all.
		plane.script(func(p *sandboxPlane) {
			p.onLedger = func(p *sandboxPlane, n int) {
				if n == 2 {
					events = append(events, rest...)
				}
			}
		})
		code, stdout, stderr := ledgerFollowFor(t, plane, 6)
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		// The first batch is the newest events, oldest first; the arrivals
		// follow in seq order, all three in the one tick that drained.
		sbInOrder(t, stderr, "  Seq", "5103", "5104", "5105", "5106")
		// Each event is a row once: the first batch's rows, then the
		// arrivals. (5103 also appears in the --body hint under it.)
		for _, once := range []string{"cus_dev_ada", "/v1/charges", "UPDATE invoices", "clock"} {
			if n := strings.Count(stderr, once); n != 1 {
				t.Errorf("%s printed %d times, want once:\n%s", once, n, stderr)
			}
		}
		q := plane.ledgerAsked()
		if len(q) < 3 {
			t.Fatalf("the ledger was asked only %q", q)
		}
		if q[0] != "detail=summary&dir=desc&limit=50&order=time" {
			t.Errorf("first read %q", q[0])
		}
		// Every poll is the ledger's own commit order, above the watermark.
		if q[1] != "after_seq=5103&detail=summary&dir=asc&limit=1000&order=arrival" {
			t.Errorf("first poll %q", q[1])
		}
		if q[2] != "after_seq=5105&detail=summary&dir=asc&limit=1000&order=arrival" {
			t.Errorf("the drain must resume above the page it just read, got %q", q[2])
		}
		if last := q[len(q)-1]; last != "after_seq=5106&detail=summary&dir=asc&limit=1000&order=arrival" {
			t.Errorf("last poll %q", last)
		}
	})

	t.Run("the watermark covers what the filters hid", func(t *testing.T) {
		// Only the sql event matches --type sql, but the watermark the page
		// carries is the whole sandbox's, so the poll starts above every
		// event read past -- not just the one printed.
		events := lgFixture()
		plane := traceLedgerBench(t, &events, 0, nil)
		code, _, stderr := ledgerFollowFor(t, plane, 3, "--type", "sql")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "5105", "UPDATE invoices")
		q := plane.ledgerAsked()
		if len(q) < 2 {
			t.Fatalf("the ledger was asked only %q", q)
		}
		if !strings.Contains(q[1], "after_seq=5106") || !strings.Contains(q[1], "type=sql") {
			t.Errorf("first poll %q, want after_seq=5106 and type=sql", q[1])
		}
	})

	t.Run("a plane that stops answering is one warning", func(t *testing.T) {
		events := lgFixture()[:1]
		plane := traceLedgerBench(t, &events, 0, nil)
		plane.script(func(p *sandboxPlane) {
			serve := p.ledger
			p.ledger = func(q url.Values) (int, any) {
				if q.Get("order") == "arrival" {
					// A refusal, not a 5xx: the client retries those, and a
					// follow must not spend its interval on backoff.
					return 422, map[string]any{"detail": "unknown source 'sandbox'"}
				}
				return serve(q)
			}
		})
		code, _, stderr := ledgerFollowFor(t, plane, 4)
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if n := strings.Count(stderr, "! could not read the ledger"); n != 1 {
			t.Errorf("the failure was reported %d times, want once:\n%s", n, stderr)
		}
		if !strings.Contains(stderr, "unknown source") {
			t.Errorf("the refusal's own words are missing:\n%s", stderr)
		}
	})

	t.Run("json is one event per line", func(t *testing.T) {
		events := lgFixture()[:1]
		plane := traceLedgerBench(t, &events, 0, nil)
		rest := lgFixture()[3:]
		plane.script(func(p *sandboxPlane) {
			p.onLedger = func(p *sandboxPlane, n int) {
				if n == 2 {
					events = append(events, rest...)
				}
			}
		})
		code, stdout, stderr := ledgerFollowFor(t, plane, 4, "--json")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) != 2 {
			t.Fatalf("%d lines, want 2:\n%s", len(lines), stdout)
		}
		for i, want := range []string{`"seq":5103`, `"seq":5106`} {
			var row map[string]any
			if err := json.Unmarshal([]byte(lines[i]), &row); err != nil {
				t.Errorf("line %d is not JSON: %v", i, err)
			}
			if !strings.Contains(lines[i], want) {
				t.Errorf("line %d = %s, want %s", i, lines[i], want)
			}
		}
	})
}

func TestSandboxTraceLedgerBody(t *testing.T) {
	events := lgFixture()
	plane := traceLedgerBench(t, &events, 0, nil)
	full := map[string]any{
		"seq": 5104, "id": 104,
		"source": map[string]any{"kind": "service", "name": "stripe", "protocol": "http"},
		"plane":  "vendor", "type": "http", "direction": "inbound", "tier": twin.TierFault,
		"at": lgAt2, "world_time": "2026-03-01T09:00:00Z", "duration_ms": 3004,
		"session": "66e0a1b2.1f3a",
		"actor":   map[string]any{"credential": "api_key", "ref": "acct_clinic_admin"},
		"state":   map[string]any{"before": 12, "after": 13, "mutating": true},
		"outcome": map[string]any{"ok": false, "code": "402",
			"fault": map[string]any{"kind": "error", "id": "flt_7"}, "error": "card_declined"},
		"http": map[string]any{
			"method": "POST", "path": "/v1/charges", "query": map[string]any{}, "op": nil,
			"request": map[string]any{
				"headers": map[string]any{"authorization": "Bearer [redacted]", "content-type": "application/x-www-form-urlencoded"},
				"body":    "amount=2000&currency=usd", "bytes": 24, "truncated": false, "encoding": "identity"},
			"response": map[string]any{"status": 402,
				"headers": map[string]any{"content-type": "application/json"},
				"body":    map[string]any{"error": map[string]any{"code": "card_declined", "type": "card_error"}},
				"bytes":   57, "truncated": false},
		},
		"redacted": []string{"http.request.headers.authorization"},
	}
	sqlEvent := lgFixture()[2]
	plane.script(func(p *sandboxPlane) {
		p.ledgerEvent = func(seq string) (int, any) {
			switch seq {
			case "5104":
				return 200, full
			case "5105":
				return 200, sqlEvent
			default:
				return 404, map[string]string{"detail": "no event " + seq + " in sandbox " + sbID}
			}
		}
	})

	t.Run("an http event renders headers and bodies", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "5104")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		sbInOrder(t, stderr,
			"stripe #5104  vendor fault  POST /v1/charges → 402  3004 ms  "+lgLocal(t, lgAt2),
			"actor: api_key acct_clinic_admin",
			"session: 66e0a1b2.1f3a",
			"state: 12 → 13 (mutating)",
			"error: card_declined",
			"fault: error flt_7",
			"Request headers",
			"  authorization: Bearer [redacted]",
			"  content-type: application/x-www-form-urlencoded",
			"Request body",
			"  amount=2000&currency=usd",
			"Response headers",
			"  content-type: application/json",
			"Response body",
			"  {", `"code": "card_declined"`,
		)
		if got := plane.ledgerEventsAsked(); len(got) != 1 || got[0] != "5104" {
			t.Errorf("the event route was asked %q", got)
		}
	})

	t.Run("another type prints its payload", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "5105")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "postgres #5105  vendor handler  UPDATE 1 UPDATE invoices",
			"Payload", `"command_tag": "UPDATE 1"`, `"protocol_phase": "execute"`)
	})

	t.Run("json is the event as it arrived", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "5104", "--json")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, stderr)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(stdout), &row); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
		}
		if row["seq"] != float64(5104) {
			t.Errorf("row %v", row)
		}
		if _, ok := row["redacted"]; !ok {
			t.Errorf("redacted was dropped: %v", row)
		}
	})

	t.Run("no such seq", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--body", "99")
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "✗ No ledger event 99 in sandbox "+sbID, "→ Next: veris sandbox trace")
		// The twins were not scanned: one seq names one event, so a wrong
		// one is said rather than hunted for.
		if strings.Contains(stderr, "could not read the trace") {
			t.Errorf("the per-twin scan ran anyway:\n%s", stderr)
		}
	})
}

func TestSandboxTraceLedgerOnlyFlagsNeedALedger(t *testing.T) {
	twins := newTraceTwins(t)
	twins.script(traceFixture)
	traceBench(t, twins) // the plane's ledger routes answer 404
	cases := [][]string{
		{"--type", "sql"},
		{"--plane", "world"},
		{"--source", "sandbox"},
		{"--session", "66e0a1b2.1f3a"},
		{"--mutating"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runSandboxCLI(t, append([]string{"sandbox", "trace"}, args...)...)
			if code != 1 {
				t.Fatalf("exit %d, want 1:\n%s", code, stderr)
			}
			sbInOrder(t, stderr, "✗ Sandbox "+sbID+" keeps no ledger:", "--mutating need one")
			if stdout != "" {
				t.Errorf("stdout:\n%s", stdout)
			}
		})
	}
	t.Run("the twins are still merged for the flags they can answer", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "trace", "--tier", "fault", "--since", "1")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "  Time", "Twin", "/repos/acme/app/issues", "/v1/charges")
	})
}

func TestSandboxTraceLedgerRefusals(t *testing.T) {
	twins := newTraceTwins(t)
	twins.script(traceFixture)
	traceBench(t, twins)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad type", []string{"--type", "grpc"}, "--type must be http, sql, connection, delivery or world (got 'grpc')"},
		{"bad plane", []string{"--plane", "data"}, "--plane must be vendor, control or world (got 'data')"},
		{"both source and service", []string{"--source", "stripe", "--service", "stripe"},
			"--source and --service both name one source; pass one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runSandboxCLI(t, append([]string{"sandbox", "trace"}, tc.args...)...)
			if code != 1 {
				t.Errorf("exit %d, want 1:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout:\n%s", stdout)
			}
		})
	}
}

// Every payload the ledger can carry has to read as one line of the Event
// column: the types the fixtures above do not exercise are held here.
func TestLedgerEventSummaryAndOutcome(t *testing.T) {
	cases := []struct {
		name        string
		event       api.LedgerEvent
		wantSummary string
		wantOutcome string
	}{
		{
			name: "an http call is its method and path",
			event: api.LedgerEvent{Type: "http", HTTP: &api.LedgerHTTP{Method: "GET", Path: "/v1/customers"},
				Outcome: api.LedgerOutcome{OK: true, Code: "200"}},
			wantSummary: "GET /v1/customers", wantOutcome: "200",
		},
		{
			name: "a hang has no status of its own",
			event: api.LedgerEvent{Type: "http", HTTP: &api.LedgerHTTP{Method: "POST", Path: "/v1/charges"},
				Outcome: api.LedgerOutcome{Fault: &api.LedgerFault{Kind: "hang", ID: "flt_7"}}},
			wantSummary: "POST /v1/charges", wantOutcome: "hang",
		},
		{
			name: "a statement is folded to one line and cut",
			event: api.LedgerEvent{Type: "sql", SQL: &api.LedgerSQL{CommandTag: "SELECT 3",
				Statement: "SELECT id,\n       status\n  FROM invoices\n WHERE tenant = $1 AND status = $2 AND created_at > $3"},
				Outcome: api.LedgerOutcome{OK: true}},
			wantSummary: "SELECT 3 SELECT id, status FROM invoices WHERE tenant = $1 AND…", wantOutcome: "ok",
		},
		{
			name: "a failed statement shows its sqlstate",
			event: api.LedgerEvent{Type: "sql", SQL: &api.LedgerSQL{CommandTag: "", Statement: "INSERT INTO invoices VALUES ($1)"},
				Outcome: api.LedgerOutcome{Code: "23505", Error: "duplicate key"}},
			wantSummary: "INSERT INTO invoices VALUES ($1)", wantOutcome: "23505",
		},
		{
			name: "a connection is its turn and its peer",
			event: api.LedgerEvent{Type: "connection", Connection: &api.LedgerConnection{Event: "authenticated", Peer: "app@10.8.3.21"},
				Outcome: api.LedgerOutcome{OK: true}},
			wantSummary: "authenticated app@10.8.3.21", wantOutcome: "ok",
		},
		{
			name: "a delivery is its target",
			event: api.LedgerEvent{Type: "delivery", Delivery: &api.LedgerDelivery{Method: "POST",
				Destination: "https://odd-forest.example/hooks/stripe", Attempt: 2},
				Outcome: api.LedgerOutcome{Code: "500"}},
			wantSummary: "POST https://odd-forest.example/hooks/stripe", wantOutcome: "500",
		},
		{
			name: "a world event is its own name",
			event: api.LedgerEvent{Type: "world", World: &api.LedgerWorld{Event: "reset",
				Detail: json.RawMessage(`{"services":3}`), By: "api:reset"},
				Outcome: api.LedgerOutcome{OK: true}},
			wantSummary: `reset {"services":3}`, wantOutcome: "ok",
		},
		{
			name:        "an outcome with nothing to say says nothing",
			event:       api.LedgerEvent{Type: "message"},
			wantSummary: "", wantOutcome: "—",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ledgerEventSummary(tc.event); got != tc.wantSummary {
				t.Errorf("summary = %q, want %q", got, tc.wantSummary)
			}
			if got := ledgerOutcome(tc.event.Outcome); got != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", got, tc.wantOutcome)
			}
			// A world event and a hang have no duration; the column says so
			// rather than printing a zero that reads as instant.
			row := ledgerTableRows([]api.LedgerEvent{tc.event})[0]
			if row[len(row)-1] != "—" {
				t.Errorf("a nil duration rendered as %q", row[len(row)-1])
			}
		})
	}
}
