package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
// with the fake twins' services.
func traceBench(t *testing.T, twins *traceFake) *bench {
	t.Helper()
	plane := newSandboxPlane(t)
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	services := twins.services()
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(services, time.Now().Add(time.Hour)) }
	})
	return b
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
	b := traceBench(t, twins)
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
