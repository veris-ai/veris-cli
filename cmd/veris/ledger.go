package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/proxy"
	"github.com/veris-ai/veris-cli/internal/routes"
	"github.com/veris-ai/veris-cli/internal/twin"
)

// The sandbox-side proof of a run. The engine's receipt says what LEFT the
// code under test; the twins' own request ledgers say what ARRIVED. A run is
// done when the two agree, which is why both are printed: a proxy that
// counted requests the sandbox never served, or a sandbox that served
// requests the proxy never saw, is a finding either way.
//
// The ledger is read after a watermark. Before the child starts every twin
// with a control URL answers GET /veris/requests?limit=1&order=desc, and the
// newest id is the mark; ids are monotonic, so "since the watermark" is
// exactly "id > mark", and rows the harness itself wrote earlier (seeding,
// manuals) never count. Control-tier rows -- the CLI's own /veris/* traffic
// -- are counted apart, as the engine does; delivery-tier rows are the
// twin's outbound callbacks and are printed as the deliveries.
//
// Deviation from the milestone contract, on purpose: the deliveries are read
// from the delivery-tier rows of GET /veris/requests rather than from
// GET /veris/data?entity_type=deliveries. One read per twin answers both
// halves, and the trace rows carry the destination's status and the timing
// the block prints. It leans on the twin writing a delivery-tier trace row
// per attempt (world/delivery.py does), and the deliveries share the
// page with the request rows, so a run whose twin recorded a full page
// after the mark reports its deliveries as far as the page reached.

// ledgerPageLimit is the most rows one GET /veris/requests returns. A read
// that fills the page may have missed older rows since the mark, so its count
// is "at least this many" and an assertion it cannot decide is indeterminate.
const ledgerPageLimit = 1000

// twinMark is one twin's watermark: the newest request id before the run.
type twinMark struct {
	name       string
	controlURL string
	envHint    string
	// mark is the newest id before the child ran; 0 when the log was empty.
	mark int
	// noLog is a twin with no request log to read: no control URL (a data
	// plane handed to the app), or a control plane whose /veris/requests is
	// absent (the postgres twin). It prints as "—" and asserts nothing.
	noLog bool
	// notProxied is a twin the engine does not route (a DSN, or an http
	// twin with no hostname to intercept, such as yente): it is reached by
	// the handed env hint instead, so its traffic never passes the engine,
	// the engine's count of it is no evidence either way, and the sandbox
	// ledger answers for it alone.
	notProxied bool
	// err is a watermark that could not be taken; without a mark, "since the
	// watermark" is undefined and the twin's ledger is unreadable.
	err error
}

// ledgerTwin is what one twin recorded since its mark.
type ledgerTwin struct {
	Name string `json:"-"`
	// Count is the vendor-surface rows: handler and fault tiers.
	Count int64 `json:"count"`
	// Faults is how many of Count the fault tier answered.
	Faults int64 `json:"faults"`
	// Control is the /veris/* rows, kept out of Count.
	Control int64 `json:"-"`
	// Capped is a read that filled the page: Count is a floor.
	Capped bool `json:"capped,omitempty"`
	// NotProxied is a twin the engine does not route (twinMark.notProxied):
	// its count is left out of the two-ledger comparison, which the
	// engine's 0 against it would otherwise always fail.
	NotProxied bool `json:"not_proxied,omitempty"`
	err        error
}

// delivery is one line of the deliveries block: the twin's outbound
// callbacks since the mark, grouped by method, path and status.
type delivery struct {
	Twin   string `json:"twin"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Status is nil when the destination never answered.
	Status *int  `json:"status"`
	Count  int64 `json:"count"`
}

// ledger is every twin's ledger since the watermark, read after the child.
type ledger struct {
	// Twins holds one entry per twin with a log, in the sandbox's order.
	Twins []*ledgerTwin
	// Unreadable names the twins whose ledger could not be read, with why.
	Unreadable []string
	Deliveries []delivery
}

func (l *ledger) total() int64 {
	var n int64
	for _, t := range l.Twins {
		n += t.Count
	}
	return n
}

func (l *ledger) control() int64 {
	var n int64
	for _, t := range l.Twins {
		n += t.Control
	}
	return n
}

func (l *ledger) capped() bool {
	for _, t := range l.Twins {
		if t.Capped {
			return true
		}
	}
	return false
}

func (l *ledger) twin(name string) *ledgerTwin {
	for _, t := range l.Twins {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// complete is a ledger every twin answered in full: the one that can be
// compared numerically with the engine's receipt.
func (l *ledger) complete() bool {
	return len(l.Unreadable) == 0 && !l.capped()
}

// proof carries the sandbox-side reading of one run from the watermark to
// the verdict. A nil *proof is a run with no sandbox to ask -- a config-file
// run -- and every method on it is a no-op, so the callers stay linear.
type proof struct {
	sandboxID string
	envID     string
	services  []api.ServiceInfo
	// readErr is why the sandbox could not be described: no key, or the
	// control plane refused. The ledger is then unreadable as a whole.
	readErr error
	// none is a sandbox with no twin that keeps a request log (every
	// service data plane, or a control plane that serves no control URLs):
	// there is no ledger, and the run reads exactly as it did before there
	// was one.
	none    bool
	marks   []twinMark
	start   time.Time
	twinFor func(controlURL string) *twin.Client
	// overrides is the run's --route, so a twin routed only by an override
	// counts as proxied here as it does in the engine.
	overrides map[string][]routes.Entry
}

// ledgerSandbox is the sandbox id a run's ledger belongs to, following
// resolveConfig's precedence exactly: a config file (--config or
// $VERIS_PROXY_CONFIG) names no sandbox the ledger can be read from.
func ledgerSandbox(src configSources) string {
	if src.File != "" {
		return ""
	}
	if src.Sandbox != "" {
		return src.Sandbox
	}
	if os.Getenv(discovery.EnvConfig) != "" {
		return ""
	}
	return firstNonEmpty(os.Getenv(discovery.EnvSandboxID), src.Local)
}

// newProof describes the sandbox the run routes at, from the control plane,
// so the twins' control URLs are known. nil when there is no sandbox; a
// proof with readErr when there is one that cannot be read, so the run can
// say the ledger was unreadable rather than silently print none. overrides
// is the run's --route, which decides with the sandbox's own routes which
// twins the engine proxies.
func newProof(ctx context.Context, sandboxID string, c *api.Client, overrides map[string][]routes.Entry) *proof {
	if sandboxID == "" {
		return nil
	}
	p := &proof{sandboxID: sandboxID, twinFor: twin.New, overrides: overrides}
	if c == nil || c.Key == "" {
		p.readErr = fmt.Errorf("no API key to read sandbox %s with (log in, or set %s)", sandboxID, discovery.EnvAPIKey)
		return p
	}
	sb, err := c.GetSandbox(ctx, sandboxID)
	if err != nil {
		p.readErr = err
		return p
	}
	p.envID, p.services = sb.EnvironmentID, sb.Services
	return p
}

// ledgerClient is the control-plane client a run reads the sandbox with. The
// sources already carry the merged answer -- the flag, then the login, then
// the environment -- so the ledger is read with exactly the key discovery
// used; nil when nothing supplied one.
func ledgerClient(s *session, src configSources) *api.Client {
	key := firstNonEmpty(src.APIKey, os.Getenv(discovery.EnvAPIKey))
	if key == "" {
		return nil
	}
	c := api.New(firstNonEmpty(src.APIBase, os.Getenv(discovery.EnvAPIBase)), key)
	if s != nil {
		c.UserAgent = "veris/" + s.ver
	}
	return c
}

// watermark reads every twin's newest request id and prints the mark line:
//
//	veris: watermark stripe:1234 postgres:—
//
// It is taken once the engine is live and before the child starts, so
// nothing the child sends can land below it.
func (p *proof) watermark(ctx context.Context, w io.Writer, quiet bool) {
	if p == nil || p.readErr != nil {
		return
	}
	p.start = time.Now()
	for _, svc := range p.services {
		m := twinMark{name: svc.Name, controlURL: svc.ControlURL, envHint: svc.EnvHint,
			notProxied: notProxied(svc, p.overrides)}
		if svc.ControlURL == "" {
			m.noLog = true
			p.marks = append(p.marks, m)
			continue
		}
		mctx, cancel := context.WithTimeout(ctx, ledgerReadTimeout)
		rows, err := p.twinFor(svc.ControlURL).Requests(mctx, twin.RequestsQuery{Limit: 1, Order: "desc"})
		cancel()
		var te *twin.Error
		switch {
		case errors.Is(err, twin.ErrNotSupported), errors.As(err, &te) && te.Status == http.StatusNotFound:
			m.noLog = true
		case err != nil:
			m.err = err
		case len(rows) > 0:
			m.mark = rows[0].ID
		}
		p.marks = append(p.marks, m)
	}
	p.none = true
	for _, m := range p.marks {
		if !m.noLog {
			p.none = false
		}
	}
	if !quiet && !p.none {
		fmt.Fprintf(w, "veris: watermark %s\n", formatWatermark(p.marks))
	}
}

// ledgerReadTimeout bounds each twin read; a twin that does not answer in
// this long is reported unreadable rather than holding the verdict open.
const ledgerReadTimeout = 15 * time.Second

func formatWatermark(marks []twinMark) string {
	parts := make([]string, 0, len(marks))
	for _, m := range marks {
		switch {
		case m.err != nil:
			parts = append(parts, m.name+":!")
		case m.noLog:
			parts = append(parts, m.name+":—")
		default:
			parts = append(parts, fmt.Sprintf("%s:%d", m.name, m.mark))
		}
	}
	return strings.Join(parts, " ")
}

// read is the after-read: every twin's rows above its mark, counted by tier,
// with the delivery rows folded into the deliveries block. nil when there
// was no watermark to read after.
func (p *proof) read(ctx context.Context) *ledger {
	if p == nil || p.readErr != nil || p.none {
		return nil
	}
	l := &ledger{}
	for _, m := range p.marks {
		if m.noLog {
			continue
		}
		if m.err != nil {
			l.Unreadable = append(l.Unreadable, fmt.Sprintf("%s: watermark not taken (%v)", m.name, m.err))
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, ledgerReadTimeout)
		rows, capped, err := readSince(rctx, p.twinFor(m.controlURL), m.mark)
		cancel()
		if err != nil {
			l.Unreadable = append(l.Unreadable, fmt.Sprintf("%s: %v", m.name, err))
			continue
		}
		t := &ledgerTwin{Name: m.name, Capped: capped, NotProxied: m.notProxied}
		for _, r := range rows {
			switch r.Tier {
			case twin.TierControl:
				t.Control++
			case twin.TierDelivery:
				l.Deliveries = addDelivery(l.Deliveries, m.name, r)
			case twin.TierFault:
				t.Faults++
				t.Count++
			default:
				t.Count++
			}
		}
		l.Twins = append(l.Twins, t)
	}
	// By destination, then status, so two runs print the same block.
	sort.SliceStable(l.Deliveries, func(i, j int) bool {
		a, b := l.Deliveries[i], l.Deliveries[j]
		if a.Twin != b.Twin {
			return a.Twin < b.Twin
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Method != b.Method {
			return a.Method < b.Method
		}
		switch {
		case a.Status == nil:
			return false
		case b.Status == nil:
			return true
		}
		return *a.Status < *b.Status
	})
	return l
}

// readSince reads one twin's rows above mark. since_id is sent first: a twin
// that serves it answers only the rows wanted, and one that does not either
// ignores the parameter (FastAPI drops unknown query parameters) or refuses
// it with a 422 naming it, which is retried without. The rows are filtered
// client-side either way, so the answer is right under both. A full page
// whose every row is above the mark may have older rows behind it: capped.
func readSince(ctx context.Context, c *twin.Client, mark int) (rows []twin.Request, capped bool, err error) {
	q := url.Values{"limit": {strconv.Itoa(ledgerPageLimit)}, "order": {"desc"}}
	if mark > 0 {
		q.Set("since_id", strconv.Itoa(mark))
	}
	page, err := twinRequests(ctx, c, q)
	if err != nil && rejectsSinceID(err) {
		q.Del("since_id")
		page, err = twinRequests(ctx, c, q)
	}
	if err != nil {
		return nil, false, err
	}
	full := len(page) >= ledgerPageLimit
	for _, r := range page {
		if r.ID > mark {
			rows = append(rows, r)
		}
	}
	return rows, full && len(rows) == len(page), nil
}

// rejectsSinceID is a 422 that names since_id: the twin validates its query
// and does not know the parameter.
func rejectsSinceID(err error) bool {
	var te *twin.Error
	return errors.As(err, &te) && te.Status == http.StatusUnprocessableEntity &&
		strings.Contains(te.Detail, "since_id")
}

// twinRequests is GET /veris/requests with an arbitrary query, which the
// twin client's Requests does not take: since_id is newer than it. The
// error shape is the client's, so callers read one kind.
func twinRequests(ctx context.Context, c *twin.Client, q url.Values) ([]twin.Request, error) {
	endpoint := c.ControlURL + "/veris/requests?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the twin at %s: %w", c.ControlURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read GET /veris/requests: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		e := &twin.Error{Status: resp.StatusCode, Method: http.MethodGet, Path: "/veris/requests?" + q.Encode()}
		var envelope struct {
			Detail json.RawMessage `json:"detail"`
		}
		if json.Unmarshal(raw, &envelope) == nil && len(envelope.Detail) > 0 {
			var text string
			if json.Unmarshal(envelope.Detail, &text) == nil {
				e.Detail = text
			} else {
				e.Detail = string(envelope.Detail)
			}
		}
		if e.Detail = strings.TrimSpace(e.Detail); e.Detail == "" {
			e.Detail = http.StatusText(resp.StatusCode)
		}
		return nil, e
	}
	var out struct {
		Requests []twin.Request `json:"requests"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("GET /veris/requests answered %d with a body that is not the trace log: %w", resp.StatusCode, err)
	}
	return out.Requests, nil
}

// addDelivery folds one delivery-tier row into the block. The row's path is
// the destination URL as the twin sent it; the line shows its path, which is
// what --require-callback names.
func addDelivery(list []delivery, twinName string, r twin.Request) []delivery {
	path := r.Path
	if u, err := url.Parse(r.Path); err == nil && u.Host != "" {
		path = u.Path
		if path == "" {
			path = "/"
		}
	}
	for i := range list {
		d := &list[i]
		if d.Twin == twinName && d.Method == r.Method && d.Path == path && sameStatus(d.Status, r.Status) {
			d.Count++
			return list
		}
	}
	return append(list, delivery{Twin: twinName, Method: r.Method, Path: path, Status: r.Status, Count: 1})
}

func sameStatus(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// printLedger is the sandbox's side of the receipt:
//
//	veris: the sandbox recorded 7 request(s) since the watermark:
//	  stripe                       7   (1 fault)
//	  control-plane (/veris/*)     6   not counted
//	veris: the sandbox delivered 2 callback(s):
//	  POST   /hooks/stripe                2 -> 200   (stripe)
func printLedger(w io.Writer, l *ledger) {
	total := l.total()
	if total == 0 && !l.capped() {
		if n := l.control(); n > 0 {
			fmt.Fprintf(w, "veris: the sandbox recorded 0 request(s) since the watermark (control-plane %d, not counted)\n", n)
		} else {
			fmt.Fprintln(w, "veris: the sandbox recorded 0 request(s) since the watermark")
		}
	} else {
		count := fmt.Sprintf("%d", total)
		if l.capped() {
			count = "≥" + count
		}
		fmt.Fprintf(w, "veris: the sandbox recorded %s request(s) since the watermark:\n", count)
		for _, t := range l.Twins {
			line := fmt.Sprintf("  %-28s %s", t.Name, countLabel(t.Count, t.Capped))
			if t.Faults > 0 {
				line += fmt.Sprintf("   (%d fault)", t.Faults)
			}
			if t.NotProxied {
				line += "   (not proxied)"
			}
			fmt.Fprintln(w, line)
		}
		if n := l.control(); n > 0 {
			fmt.Fprintf(w, "  %-28s %d   not counted\n", "control-plane (/veris/*)", n)
		}
	}
	if len(l.Deliveries) > 0 {
		var n int64
		for _, d := range l.Deliveries {
			n += d.Count
		}
		fmt.Fprintf(w, "veris: the sandbox delivered %d callback(s):\n", n)
		for _, d := range l.Deliveries {
			status := "—"
			if d.Status != nil {
				status = strconv.Itoa(*d.Status)
			}
			fmt.Fprintf(w, "  %-6s %-30s %d -> %s   (%s)\n", d.Method, d.Path, d.Count, status, d.Twin)
		}
	}
	for _, u := range l.Unreadable {
		fmt.Fprintf(w, "veris: ! the sandbox ledger could not be read (%s)\n", u)
	}
}

func countLabel(n int64, capped bool) string {
	if capped {
		return fmt.Sprintf("≥%d", n)
	}
	return strconv.FormatInt(n, 10)
}

// assertion is one requirement judged on the sandbox ledger. Kind is
// "service" or "callback" for a --require-* flag, "ledger" for the fresh
// run's own rule that the sandbox recorded something at all.
type assertion struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Want   int64  `json:"want"`
	// Got is the count the verdict rests on: the sandbox ledger's, or the
	// engine's when the engine is the side that showed the count (vouch).
	Got int64 `json:"got"`
	OK  bool  `json:"ok"`
	// Indeterminate is an assertion the ledger could not decide: the twin was
	// unreadable, or the read was capped below the count wanted.
	Indeterminate bool   `json:"indeterminate,omitempty"`
	Why           string `json:"why,omitempty"`
	// Source names the side that decided a service requirement when the two
	// ledgers did not agree on it: "ledger" or "engine". Empty when both
	// showed the count, or when the engine's was not there to compare.
	Source string `json:"source,omitempty"`
	// NotProxied is a requirement on a twin the engine does not route: the
	// sandbox ledger's alone to decide (see twinMark.notProxied).
	NotProxied bool `json:"not_proxied,omitempty"`
	// note is the parenthesis after a ✓ line, or the clause after an ✗ one,
	// that says which side answered and what the other side saw.
	note string
	// unreadable is an indeterminate assertion whose twin's ledger was not
	// read at all, as against one read to its cap, where rows may yet exist.
	unreadable bool
	hint       string
}

// assertLedger judges the run's requirements on the ledger. Service
// requirements read the twin's count; callback requirements count the
// deliveries to that path (or any path, for *) the destination answered 2xx;
// host requirements are the engine's alone, since the ledger knows twins,
// not hostnames, and so is a requirement on a twin that keeps no request
// log (a data plane, or a control plane without /veris/requests): the
// ledger never observed it, and the engine's verdict on it stands alone. A
// fresh run with no explicit service requirement asserts a non-empty
// ledger, mirroring the engine's rule for a sandbox it deployed. A nil
// ledger (never read) makes every assertion indeterminate. The engine's
// side of a service requirement is merged in afterwards by vouch.
func assertLedger(l *ledger, marks []twinMark, reqs, callbackReqs []requirement, freshDefault bool, childExit int) []assertion {
	var out []assertion
	unreadable := l == nil || len(l.Unreadable) > 0
	for _, req := range reqs {
		if req.kind != "service" || keepsNoLog(marks, req.name) {
			continue
		}
		a := assertion{Kind: "service", Target: req.name, Want: req.count, NotProxied: notProxiedTwin(marks, req.name)}
		var t *ledgerTwin
		if l != nil {
			t = l.twin(req.name)
		}
		switch {
		case l == nil:
			a.Indeterminate, a.unreadable, a.Why = true, true, "the sandbox ledger could not be read"
		case t == nil && twinUnreadable(l, req.name):
			a.Indeterminate, a.unreadable, a.Why = true, true, req.name+"'s ledger could not be read"
		case t == nil:
			a.Got = 0
		default:
			a.Got = t.Count
			if t.Capped && a.Got < req.count {
				a.Indeterminate, a.Why = true, fmt.Sprintf("%s's ledger was read to the %d-row cap", req.name, ledgerPageLimit)
			}
		}
		a.OK = !a.Indeterminate && a.Got >= a.Want
		if !a.OK {
			// Kept on an indeterminate one too: vouch may turn it into a
			// refusal, and the hint belongs under that line.
			a.hint = serviceHint(marks, req.name, a.Got, childExit)
		}
		if a.OK && a.NotProxied {
			// The engine never sees this twin's traffic, so the sandbox
			// ledger's count is the only one there could be; the line says
			// so, or "saw 4" beside the engine's receipt of 0 reads as a
			// contradiction.
			a.Source, a.note = "ledger", "sandbox ledger; not proxied"
		}
		out = append(out, a)
	}
	for _, req := range callbackReqs {
		a := assertion{Kind: "callback", Target: req.name, Want: req.count}
		if l != nil {
			for _, d := range l.Deliveries {
				if (req.name == "*" || d.Path == req.name) && d.Status != nil && *d.Status/100 == 2 {
					a.Got += d.Count
				}
			}
		}
		if a.Got < req.count && (unreadable || (l != nil && l.capped())) {
			a.Indeterminate, a.Why = true, "the sandbox ledger could not be read in full"
		}
		a.OK = !a.Indeterminate && a.Got >= a.Want
		if !a.OK && !a.Indeterminate {
			a.hint = "no callback reached your app through the sandbox (is the endpoint registered from your code, and is the sandbox clock live?)"
		}
		out = append(out, a)
	}
	if freshDefault && len(reqs) == 0 {
		a := assertion{Kind: "ledger", Target: "any service", Want: 1}
		if l != nil {
			a.Got = l.total()
		}
		if a.Got == 0 && unreadable {
			a.Indeterminate, a.Why = true, "the sandbox ledger could not be read"
		}
		a.OK = !a.Indeterminate && a.Got >= a.Want
		if !a.OK && !a.Indeterminate {
			a.hint = "this run deployed a sandbox and the sandbox recorded nothing since the watermark: either the suite never called its dependencies, or interception missed them"
		}
		out = append(out, a)
	}
	return out
}

// keepsNoLog reports whether the named twin was marked as having no request
// log to read. A name the marks do not know is not one: the assertion on it
// is judged on the ledger, which counts it 0.
func keepsNoLog(marks []twinMark, name string) bool {
	for _, m := range marks {
		if m.name == name {
			return m.noLog
		}
	}
	return false
}

// notProxiedTwin reports whether the named twin was marked as one the
// engine does not route. A name the marks do not know is taken as proxied:
// the engine's count of it is then the ordinary evidence.
func notProxiedTwin(marks []twinMark, name string) bool {
	for _, m := range marks {
		if m.name == name {
			return m.notProxied
		}
	}
	return false
}

// vouch merges the engine's receipt into the service assertions, so each
// requirement has one verdict whichever side saw the traffic: it passes when
// EITHER ledger shows the count. The sandbox ledger's count stands where it
// met the requirement; where it did not, the engine's count stands in when
// that met it -- a twin that did not record what the proxy forwarded, or a
// ledger that could not be read, is reported beside the ✓ rather than turned
// into a refusal of what the proxy demonstrably delivered. Neither side
// showing the count is unmet, exit 3: the engine refutes it even where the
// ledger could not be read, since the proxy forwarded nothing to the twin;
// only a ledger read to its cap stays undecided, since rows may yet exist
// behind the cap. A twin the engine does not route is left to the ledger:
// the engine's 0 for it is no evidence, and vouch does not touch it.
func vouch(assertions []assertion, engine *proxy.Receipt) {
	if engine == nil {
		return
	}
	for i := range assertions {
		a := &assertions[i]
		if a.Kind != "service" || a.NotProxied {
			continue
		}
		sent := engine.ByService[a.Target]
		switch {
		case a.OK:
			if sent < a.Want {
				a.Source, a.note = "ledger", fmt.Sprintf("sandbox ledger; the engine saw %d", sent)
			}
		case sent >= a.Want:
			other := fmt.Sprintf("the sandbox ledger recorded %d", a.Got)
			if a.Indeterminate {
				other = a.Why
			}
			a.OK, a.Indeterminate, a.hint = true, false, ""
			a.Got, a.Source, a.note = sent, "engine", "engine; "+other
		case a.Indeterminate && a.unreadable:
			a.Indeterminate = false
			a.Got, a.Source, a.note = sent, "engine", a.Why
		}
	}
}

func twinUnreadable(l *ledger, name string) bool {
	for _, u := range l.Unreadable {
		if strings.HasPrefix(u, name+":") {
			return true
		}
	}
	return false
}

// serviceHint is the line under an unmet service requirement, ordered by
// what the run already knows rather than by what is most often true.
//
// childExit is the command's own exit code. When it is not zero, that comes
// first: a suite that died before it called the vendor explains an empty
// count by itself, and the base-URL question below would send the reader at
// the one change every skill forbids -- measured, on a run whose real cause
// was "No module named pytest" three lines above. Only a command that
// succeeded and still sent nothing is evidence of a base URL pointed
// somewhere else, and only then is the env hint worth naming.
func serviceHint(marks []twinMark, name string, got int64, childExit int) string {
	if got > 0 {
		return "fewer requests reached the sandbox than the run required"
	}
	if childExit != 0 {
		return fmt.Sprintf("the command exited %d: read its own output first, since a command that failed before calling %s proves nothing about the wiring", childExit, name)
	}
	for _, m := range marks {
		if m.name == name && m.envHint != "" {
			return fmt.Sprintf("your tests never touched the sandbox (is %s overridden in your test setup?)", m.envHint)
		}
	}
	return "your tests never touched the sandbox (is the service's base URL overridden in your test setup?)"
}

// verdict is what the ledger says about the run's exit code, printed as it
// is decided.
type verdict struct {
	Assertions []assertion
	// Fatal is an unmet assertion: exit 3.
	Fatal bool
	// Indeterminate is an assertion the ledger could not decide: exit 4.
	Indeterminate bool
}

// printVerdict reports the assertions and the two-ledger comparison:
//
//	veris: ✗ the run required service stripe at least 1 time(s) but the sandbox received it 0 time(s)
//	            — the command exited 1: read its own output first, since a command
//	              that failed before calling stripe proves nothing about the wiring
//	veris: ✓ required stripe ≥1: saw 7   ✓ required yente ≥1: saw 4 (sandbox ledger; not proxied)   ✓ ledgers agree (7 = 7)
//
// engine is nil when the engine's receipt is not available numerically (it
// could not be read), and the comparison is then left out.
func printVerdict(w io.Writer, l *ledger, assertions []assertion, engine *proxy.Receipt, quiet bool) verdict {
	v := verdict{Assertions: assertions}
	var oks []string
	for _, a := range assertions {
		switch {
		case a.Indeterminate:
			v.Indeterminate = true
			fmt.Fprintf(w, "veris: ! cannot decide whether %s: %s\n", a.describe(), a.Why)
		case !a.OK:
			v.Fatal = true
			fmt.Fprintf(w, "veris: ✗ %s\n", a.unmet())
			if a.hint != "" {
				fmt.Fprintf(w, "            — %s\n", a.hint)
			}
		default:
			oks = append(oks, "✓ "+a.met())
		}
	}
	if l != nil && engine != nil && l.complete() {
		// Compared over the twins the ledger holds and the engine routes: a
		// twin that keeps no log has nothing to agree with, and one the
		// engine does not route never shows in its receipt, so neither is a
		// disagreement. With no such twin there is nothing to compare.
		if sent, recorded, any := comparedCounts(engine, l); any {
			if sent == recorded {
				oks = append(oks, fmt.Sprintf("✓ ledgers agree (%d = %d)", sent, recorded))
			} else {
				fmt.Fprintf(w, "veris: ! ledgers differ (engine %d, sandbox %d)\n", sent, recorded)
				if recorded > sent {
					fmt.Fprintln(w, "            — sandbox totals can include sibling token verification or concurrent traffic; compare per-twin traces. Requirements are judged separately.")
				}
			}
		}
	}
	if len(oks) > 0 && !quiet {
		fmt.Fprintf(w, "veris: %s\n", strings.Join(oks, "   "))
	}
	return v
}

// comparedCounts is the two sides of the ledger comparison over the twins
// the ledger holds and the engine routes: what the engine's receipt says it
// sent them, and what they recorded. any is false when no twin qualifies.
func comparedCounts(engine *proxy.Receipt, l *ledger) (sent, recorded int64, any bool) {
	for _, t := range l.Twins {
		if t.NotProxied {
			continue
		}
		any = true
		sent += engine.ByService[t.Name]
		recorded += t.Count
	}
	return sent, recorded, any
}

func (a assertion) describe() string {
	switch a.Kind {
	case "callback":
		return fmt.Sprintf("the run required callback %s at least %d time(s)", a.Target, a.Want)
	case "ledger":
		return "the sandbox recorded anything since the watermark"
	}
	return fmt.Sprintf("the run required service %s at least %d time(s)", a.Target, a.Want)
}

func (a assertion) unmet() string {
	switch a.Kind {
	case "callback":
		return fmt.Sprintf("%s but the sandbox delivered it %d time(s)", a.describe(), a.Got)
	case "ledger":
		return "this run deployed a sandbox and the sandbox recorded nothing since the watermark"
	}
	if a.Source == "engine" {
		// Refuted by the engine over a ledger that could not be read.
		return fmt.Sprintf("%s but the engine saw it %d time(s) and %s", a.describe(), a.Got, a.note)
	}
	return fmt.Sprintf("%s but the sandbox received it %d time(s)", a.describe(), a.Got)
}

func (a assertion) met() string {
	switch a.Kind {
	case "callback":
		return fmt.Sprintf("callback %s ≥%d: delivered %d", a.Target, a.Want, a.Got)
	case "ledger":
		return fmt.Sprintf("sandbox recorded %d", a.Got)
	}
	line := fmt.Sprintf("required %s ≥%d: saw %d", a.Target, a.Want, a.Got)
	if a.note != "" {
		line += " (" + a.note + ")"
	}
	return line
}

// finish is the whole after-read: the ledger printed, the assertions judged
// and printed, the verdict returned. engine is the run's receipt when it
// could be read, and answers with the ledger for each service requirement
// (vouch). It is called after the engine's own receipt and unmet lines, so
// the two sides read in the doc's order.
func (p *proof) finish(ctx context.Context, w io.Writer, engine *proxy.Receipt,
	reqs, callbackReqs []requirement, freshDefault, quiet bool, childExit int,
) (*ledger, verdict) {
	if p == nil || p.none {
		return nil, verdict{}
	}
	if p.readErr != nil {
		fmt.Fprintf(w, "veris: ! the sandbox ledger could not be read (%v)\n", p.readErr)
		assertions := assertLedger(nil, nil, reqs, callbackReqs, freshDefault, childExit)
		vouch(assertions, engine)
		return nil, printVerdict(w, nil, assertions, engine, quiet)
	}
	l := p.read(ctx)
	if !quiet {
		printLedger(w, l)
	} else {
		for _, u := range l.Unreadable {
			fmt.Fprintf(w, "veris: ! the sandbox ledger could not be read (%s)\n", u)
		}
	}
	assertions := assertLedger(l, p.marks, reqs, callbackReqs, freshDefault, childExit)
	vouch(assertions, engine)
	if !freshDefault && len(assertions) == 0 && l.total() == 0 && l.complete() && !quiet {
		fmt.Fprintln(w, "veris: ! the sandbox recorded no service request since the watermark")
	}
	return l, printVerdict(w, l, assertions, engine, quiet)
}

// enginesAlone is the subset of reqs the sandbox ledger will not judge, for
// the engine to judge by itself: every host requirement, and the service
// requirements when there is no ledger at all (no sandbox, or no twin with
// a log) or the twin keeps none. A service requirement the ledger judges is
// judged once, there, with the engine's count merged in by vouch -- so the
// engine printing its own verdict on it would be the same requirement
// answered twice, in two voices.
func (p *proof) enginesAlone(reqs []requirement) []requirement {
	if p == nil || p.none {
		return reqs
	}
	var out []requirement
	for _, r := range reqs {
		if r.kind != "service" || (p.readErr == nil && keepsNoLog(p.marks, r.name)) {
			out = append(out, r)
		}
	}
	return out
}

// sandboxServices is the sandbox's service list as the control plane
// described it, nil for a run with no sandbox or one that could not be read.
func (p *proof) sandboxServices() []api.ServiceInfo {
	if p == nil {
		return nil
	}
	return p.services
}

// --- the receipt file ---------------------------------------------------------

// runReceipt is --receipt PATH's document: both ledgers and the verdict, for
// a harness that wants numbers rather than stderr prose.
type runReceipt struct {
	SandboxID     string          `json:"sandbox_id"`
	EnvironmentID string          `json:"environment_id"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at"`
	ExitCode      int             `json:"exit_code"`
	Engine        *engineReceipt  `json:"engine"`
	Sandbox       *sandboxReceipt `json:"sandbox"`
	Assertions    []assertion     `json:"assertions"`
}

type engineReceipt struct {
	Services  map[string]int64 `json:"services"`
	Callbacks []proxy.Callback `json:"callbacks"`
}

type sandboxReceipt struct {
	Requests     map[string]*ledgerTwin `json:"requests"`
	ControlPlane int64                  `json:"control_plane"`
	Deliveries   []delivery             `json:"deliveries"`
}

// writeReceipt writes the run's receipt to path, never to stdout. A file
// that cannot be written is reported but does not change the verdict: the
// run happened, and its exit code is what a harness reads first.
func writeReceipt(w io.Writer, path string, p *proof, l *ledger, engine *proxy.Receipt,
	inbound *proxy.InboundReceipt,
	assertions []assertion, started, finished time.Time, code int,
) {
	if path == "" {
		return
	}
	r := runReceipt{StartedAt: started.UTC(), FinishedAt: finished.UTC(), ExitCode: code, Assertions: assertions}
	if r.Assertions == nil {
		r.Assertions = []assertion{}
	}
	if p != nil {
		r.SandboxID, r.EnvironmentID = p.sandboxID, p.envID
	}
	if engine != nil {
		r.Engine = &engineReceipt{Services: engine.ByService, Callbacks: []proxy.Callback{}}
		if inbound != nil && inbound.Callbacks != nil {
			r.Engine.Callbacks = inbound.Callbacks
		}
		if r.Engine.Services == nil {
			r.Engine.Services = map[string]int64{}
		}
	}
	if l != nil {
		r.Sandbox = &sandboxReceipt{Requests: map[string]*ledgerTwin{}, ControlPlane: l.control(), Deliveries: l.Deliveries}
		for _, t := range l.Twins {
			r.Sandbox.Requests[t.Name] = t
		}
		if r.Sandbox.Deliveries == nil {
			r.Sandbox.Deliveries = []delivery{}
		}
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err == nil {
		err = os.WriteFile(path, append(raw, '\n'), 0o600)
	}
	if err != nil {
		fmt.Fprintf(w, "veris: could not write the receipt to %s (%v)\n", path, err)
	}
}

// --- run --fresh --------------------------------------------------------------

// freshRun is a sandbox the run made for itself: up before the child, down
// after it, whatever happened in between.
type freshRun struct {
	s     *session
	c     *api.Client
	sb    *api.Sandbox
	start time.Time
	keep  bool
}

// startFresh is up, as the up verb does it (the same code path, data files
// included), without the folder's pointer: the sandbox is this run's own
// and is gone before the run returns unless it is kept. A sandbox that came
// up but could not be made ready is torn down here, so a failed --fresh
// leaves nothing behind either.
func startFresh(ctx context.Context, s *session, envFlag string, ttl int, keep bool, w io.Writer) (*freshRun, error) {
	start := time.Now()
	us, sb, err := upSandbox(s.ctx, envFlag, upOptions{ttl: ttl, timeout: defaultUpTimeout}, false)
	if err != nil {
		if sb != nil {
			f := &freshRun{s: us, sb: sb, start: start, keep: keep}
			if f.c, _ = us.client(); f.c != nil {
				f.teardown(ctx, w, exitFrom(err))
			}
		}
		return nil, err
	}
	c, err := us.client()
	if err != nil {
		return nil, err
	}
	routable := 0
	for _, svc := range sb.Services {
		if svc.ControlURL != "" {
			routable++
		}
	}
	fmt.Fprintf(w, "veris: sandbox ready sandbox_id=%s (%d/%d twins routable, %.1f s)\n",
		sb.ID, routable, len(sb.Services), time.Since(start).Seconds())
	return &freshRun{s: us, c: c, sb: sb, start: start, keep: keep}, nil
}

// exitFrom is the exit code an error carries: nil is 0, an exitCode or a
// printedError its own, anything else 1.
func exitFrom(err error) int {
	if err == nil {
		return 0
	}
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	var reported printedError
	if errors.As(err, &reported) {
		return reported.code
	}
	return 1
}

// teardown deletes the sandbox, or keeps it when asked (--keep) or when the
// run's outcome is indeterminate, where the world is the evidence. A kept
// sandbox is remembered as the folder's, so veris status and veris down find
// it. Signals are held while this runs: a second Ctrl-C must not leave the
// sandbox alive until its TTL.
func (f *freshRun) teardown(ctx context.Context, w io.Writer, code int) {
	held := holdSignals()
	defer held()
	switch {
	case f.keep:
		fmt.Fprintf(w, "veris: sandbox %s kept (--keep)\n", f.sb.ID)
		f.remember()
	case code == exitIndeterminate:
		fmt.Fprintf(w, "veris: ! sandbox %s is kept: the run's outcome is indeterminate, and the world is the evidence (veris down deletes it)\n", f.sb.ID)
		f.remember()
	default:
		dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err := f.c.DeleteSandbox(dctx, f.sb.EnvironmentID, f.sb.ID)
		if err != nil && !api.IsStatus(err, http.StatusNotFound) {
			fmt.Fprintf(w, "veris: could not delete sandbox %s; it will expire on its TTL (%v)\n", f.sb.ID, err)
			return
		}
		fmt.Fprintf(w, "veris: sandbox deleted %s (%.1f s total)\n", f.sb.ID, time.Since(f.start).Seconds())
	}
}

func (f *freshRun) remember() {
	if err := f.s.rememberSandbox(f.sb); err != nil {
		fmt.Fprintf(os.Stderr, "veris: could not remember sandbox %s for this folder (%v)\n", f.sb.ID, err)
	}
}

// holdSignals parks Ctrl-C and SIGTERM until the returned release runs, so
// a teardown in progress finishes: the child is already gone by then, and
// the only thing an interrupt could still do is leak the sandbox.
func holdSignals() (release func()) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	return func() { signal.Stop(sigs) }
}
