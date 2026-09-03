package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/twin"
)

const (
	// traceDefaultLimit is how many rows trace shows across the merge when
	// --limit is not given; traceMaxLimit is the twin's own ceiling.
	traceDefaultLimit = 50
	traceMaxLimit     = 1000
)

// traceFollowInterval is the wait between --follow polls. A variable so a
// test can turn seconds into milliseconds; the binary never changes it.
var traceFollowInterval = 2 * time.Second

// traceFollowContext is the context --follow runs under: one that ends on
// Ctrl-C, so the loop returns cleanly (exit 0) instead of dying mid-line.
// Tests replace it with a context they cancel themselves.
var traceFollowContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// traceOptions are trace's flags; zero means "not given".
type traceOptions struct {
	id      string
	service string
	tier    string
	limit   int
	since   int
	body    int
	follow  bool
}

// sandboxTraceCommand is `veris sandbox trace`: the sandbox's own request
// ledger from GET /veris/requests on every twin, merged newest first. It is
// the receipt when you are outside run.
func sandboxTraceCommand() *cli.Command {
	var o traceOptions
	return &cli.Command{
		Name:    "trace",
		Summary: "What the sandbox received, newest first",
		Usage:   "veris sandbox trace [--id ID] [--service NAME] [--tier handler|fault|control|delivery] [--limit N] [--since ID] [--follow] [--body ID] [--json]",
		Help: "trace reads GET /veris/requests on every twin (or --service NAME) and merges the rows newest first;\n" +
			"credentials are already redacted. Times are the sandbox's own clock, in UTC. A status of — is a\n" +
			"hang fault: the twin sent nothing. --since ID keeps rows above that id; --follow polls every 2 s and\n" +
			"prints what arrived, oldest first, until Ctrl-C (with --json, one row per line); its first batch\n" +
			"is the newest rows shown oldest first too, tail -f style. --body ID prints one entry's request\n" +
			"and response headers and bodies.",
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&o.id, "id", "", "sandbox id (default: this folder's)")
			fs.StringVar(&o.service, "service", "", "one twin's trace (default: every twin)")
			fs.StringVar(&o.tier, "tier", "", "only rows of this tier: handler, fault, control or delivery")
			fs.IntVar(&o.limit, "limit", traceDefaultLimit, "rows to show across the merge (1..1000)")
			fs.IntVar(&o.since, "since", 0, "only rows with an id above this")
			fs.BoolVar(&o.follow, "follow", false, "keep polling and print new rows as they arrive")
			fs.IntVar(&o.body, "body", 0, "print the headers and bodies of the entry with this id")
		},
		Run: func(ctx *cli.Context, args []string) error {
			if err := noPositionals(ctx, args); err != nil {
				return err
			}
			return sandboxTrace(ctx, o)
		},
	}
}

// traceRow is one merged row: the twin's own record with the twin named,
// which is what --json prints and what the table is drawn from.
type traceRow struct {
	Twin string `json:"twin"`
	twin.Request
}

// traceTwin is one twin's trace as the command reads it: the client, and
// whether since_id has been refused, so a twin that does not know the
// parameter is asked without it from then on.
type traceTwin struct {
	name    string
	client  *twin.Client
	noSince bool
}

// read is one GET /veris/requests, newest first, filtered to ids above
// since. since_id is sent when since is set and the twin has not refused
// it; a 422 naming it is retried without. The rows are filtered client-side
// either way, so a twin that ignores the parameter answers right too.
func (t *traceTwin) read(ctx context.Context, tier string, limit, since int) ([]twin.Request, error) {
	q := url.Values{"order": {"desc"}, "limit": {strconv.Itoa(limit)}}
	if tier != "" {
		q.Set("tier", tier)
	}
	if since > 0 && !t.noSince {
		q.Set("since_id", strconv.Itoa(since))
	}
	rows, err := twinRequests(ctx, t.client, q)
	if err != nil && rejectsSinceID(err) {
		t.noSince = true
		q.Del("since_id")
		rows, err = twinRequests(ctx, t.client, q)
	}
	if err != nil {
		return nil, err
	}
	if since <= 0 {
		return rows, nil
	}
	kept := rows[:0]
	for _, r := range rows {
		if r.ID > since {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// find is one entry by id. With since_id the twin answers it in one row
// (the first above id-1, oldest first); a twin that refuses or ignores the
// parameter answers something else, and the newest page is scanned instead.
func (t *traceTwin) find(ctx context.Context, id int) (*twin.Request, error) {
	if !t.noSince {
		q := url.Values{"order": {"asc"}, "limit": {"1"}, "since_id": {strconv.Itoa(id - 1)}}
		rows, err := twinRequests(ctx, t.client, q)
		switch {
		case err == nil && len(rows) == 1 && rows[0].ID == id:
			return &rows[0], nil
		case err != nil && rejectsSinceID(err):
			t.noSince = true
		case err != nil:
			return nil, err
		}
	}
	rows, err := twinRequests(ctx, t.client, url.Values{"order": {"desc"}, "limit": {strconv.Itoa(traceMaxLimit)}})
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// noTrace reports a twin that serves no request trace: the postgres twin
// has no /veris/requests, and its 404 is a fact about the twin, not a
// failure to report.
func noTrace(err error) bool {
	var te *twin.Error
	return errors.As(err, &te) && te.Status == http.StatusNotFound
}

// sandboxTrace is the whole verb: resolve the sandbox and its twins, read
// each trace, merge, print; then follow when asked.
func sandboxTrace(ctx *cli.Context, o traceOptions) error {
	// Flags are checked before anything is read: a --tier the twin would
	// refuse is a usage error, not a reason to fetch the sandbox first.
	if err := traceCheckFlags(o); err != nil {
		return err
	}
	s, _, sb, err := openSandboxServices(ctx, o.id)
	if err != nil {
		return err
	}
	twins, err := traceTwins(s, sb, o.service)
	if err != nil {
		return err
	}
	if o.body > 0 {
		return traceBody(s, twins, o.body)
	}
	bg := context.Background()
	rows, newest, readable, failed := traceMerged(bg, s, twins, o.tier, o.limit, o.since)
	if readable == 0 {
		// Nothing answered: every failure is already a ! line (exit 1); a
		// twin asked for by name that keeps no trace is a wrong target
		// (exit 1); a sandbox with no such twin at all has nothing to show.
		switch {
		case failed > 0:
			return printed(1)
		case o.service != "":
			s.ui.Fail("%s keeps no request trace (data plane)", o.service)
			return printed(1)
		case len(twins) > 0:
			s.ui.Warn("no twin in sandbox %s serves a request trace", sb.ID)
		}
		if s.ctx.Globals.JSON {
			return printJSON(s.ctx.Stdout, []traceRow{})
		}
		return nil
	}
	// The watermark --follow starts from is each twin's newest id as READ,
	// not as printed: a twin whose rows the cross-twin cut dropped has still
	// been seen up to that id, and must not have its history re-printed as
	// arrivals on the first poll.
	last := map[string]int{}
	for _, t := range twins {
		last[t.name] = max(o.since, newest[t.name])
	}
	// With --follow the first batch reads tail -f style: the newest rows
	// were selected, and are shown oldest first like every batch after.
	var top traceRow
	if len(rows) > 0 {
		top = rows[0]
		if o.follow {
			sortTraceRows(rows, false)
		}
	}
	switch {
	case o.follow && s.ctx.Globals.JSON:
		if err := traceJSONLines(s, rows); err != nil {
			return err
		}
	case s.ctx.Globals.JSON:
		if rows == nil {
			rows = []traceRow{}
		}
		return printJSON(s.ctx.Stdout, rows)
	case len(rows) == 0:
		s.ui.Info("No requests recorded%s", traceScope(o))
		if !o.follow {
			s.ui.Next("veris run")
		}
	default:
		s.ui.Table([]string{"  Time", "Twin", "Tier", "Method", "Path", "Status", "ms"}, traceTableRows(rows))
		hint := fmt.Sprintf("veris sandbox trace --body %d", top.ID)
		if len(twins) > 1 {
			hint += " --service " + top.Twin
		}
		s.ui.Link(hint + "   (headers and bodies of one entry)")
	}
	if !o.follow {
		return nil
	}
	return traceFollow(s, twins, o, last)
}

// traceCheckFlags refuses the combinations the twin or the merge cannot
// honour before any twin is asked.
func traceCheckFlags(o traceOptions) error {
	switch o.tier {
	case "", twin.TierHandler, twin.TierFault, twin.TierControl, twin.TierDelivery:
	default:
		return fmt.Errorf("--tier must be %s, %s, %s or %s (got '%s')",
			twin.TierHandler, twin.TierFault, twin.TierControl, twin.TierDelivery, o.tier)
	}
	if o.limit < 1 || o.limit > traceMaxLimit {
		return fmt.Errorf("--limit must be between 1 and %d (got %d)", traceMaxLimit, o.limit)
	}
	if o.since < 0 {
		return fmt.Errorf("--since must be a trace id (got %d)", o.since)
	}
	if o.body < 0 {
		return fmt.Errorf("--body must be a trace id (got %d)", o.body)
	}
	if o.body > 0 && o.follow {
		return errors.New("--body prints one entry; it cannot be combined with --follow")
	}
	return nil
}

// traceTwins is every twin with a control URL, or the one --service names.
// A named twin without a control URL is data plane and has nothing to
// trace; that is said rather than answered with an empty table.
func traceTwins(s *session, sb *api.Sandbox, service string) ([]*traceTwin, error) {
	var services []api.ServiceInfo
	if service != "" {
		svc, err := twinNamed(s, sb, service)
		if err != nil {
			return nil, err
		}
		if svc.ControlURL == "" {
			s.ui.Fail("%s keeps no request trace (data plane)", svc.Name)
			return nil, printed(1)
		}
		services = []api.ServiceInfo{*svc}
	} else {
		for _, svc := range sb.Services {
			if svc.ControlURL != "" {
				services = append(services, svc)
			}
		}
	}
	twins := make([]*traceTwin, 0, len(services))
	for _, svc := range services {
		twins = append(twins, &traceTwin{name: svc.Name, client: s.twin(svc.ControlURL)})
	}
	return twins, nil
}

// traceMerged reads every twin and merges the rows newest first by ts, then
// id, cut to limit. newest is each twin's highest id before the cut, the
// mark a follow starts from. readable counts the twins that answered with
// a trace and failed those that could not be read (each a ! line); a twin
// without the route is neither, and is skipped in silence.
func traceMerged(ctx context.Context, s *session, twins []*traceTwin, tier string, limit, since int) (rows []traceRow, newest map[string]int, readable, failed int) {
	newest = map[string]int{}
	for _, t := range twins {
		got, err := t.read(ctx, tier, limit, since)
		if err != nil {
			if !noTrace(err) {
				s.ui.Warn("%s: could not read the trace: %v", t.name, err)
				failed++
			}
			continue
		}
		readable++
		for _, r := range got {
			rows = append(rows, traceRow{Twin: t.name, Request: r})
			newest[t.name] = max(newest[t.name], r.ID)
		}
	}
	sortTraceRows(rows, true)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, newest, readable, failed
}

// sortTraceRows orders rows by ts then id, newest first when desc; the twin
// name breaks the remaining ties so two runs print the same.
func sortTraceRows(rows []traceRow, desc bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.TS != b.TS {
			return (a.TS > b.TS) == desc
		}
		if a.ID != b.ID {
			return (a.ID > b.ID) == desc
		}
		return a.Twin < b.Twin
	})
}

// traceScope names what the empty result was filtered by, for the "No
// requests recorded" line.
func traceScope(o traceOptions) string {
	var parts []string
	if o.service != "" {
		parts = append(parts, "by "+o.service)
	}
	if o.tier != "" {
		parts = append(parts, "of tier "+o.tier)
	}
	if o.since > 0 {
		parts = append(parts, fmt.Sprintf("since id %d", o.since))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

// traceTableRows renders rows for the doc's table: time as HH:MM:SS.mmm on
// the sandbox clock, a hang's missing status as —.
func traceTableRows(rows []traceRow) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{"  " + traceTime(r.TS), r.Twin, r.Tier, r.Method, r.Path,
			traceStatus(r.Status), strconv.Itoa(r.DurationMS)})
	}
	return out
}

// traceTime renders a trace timestamp. The twin records whole seconds of
// its virtual clock; a value too large for seconds is read as milliseconds.
func traceTime(ts int) string {
	sec, nsec := int64(ts), int64(0)
	if ts > 1e12 {
		sec, nsec = int64(ts)/1000, int64(ts)%1000*int64(time.Millisecond)
	}
	return time.Unix(sec, nsec).UTC().Format("15:04:05.000")
}

func traceStatus(status *int) string {
	if status == nil {
		return "—"
	}
	return strconv.Itoa(*status)
}

// traceFollow polls every twin on the interval and prints what arrived
// above the last id seen per twin, oldest first, until the context ends.
// A twin that fails to answer is a ! line once, and again only after it
// has answered in between, so a twin that is down does not fill the screen.
func traceFollow(s *session, twins []*traceTwin, o traceOptions, last map[string]int) error {
	ctx, stop := traceFollowContext()
	defer stop()
	failing := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(traceFollowInterval):
		}
		var fresh []traceRow
		for _, t := range twins {
			got, err := t.read(ctx, o.tier, traceMaxLimit, last[t.name])
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if !noTrace(err) && !failing[t.name] {
					s.ui.Warn("%s: could not read the trace: %v", t.name, err)
				}
				failing[t.name] = true
				continue
			}
			failing[t.name] = false
			for _, r := range got {
				fresh = append(fresh, traceRow{Twin: t.name, Request: r})
				last[t.name] = max(last[t.name], r.ID)
			}
		}
		if len(fresh) == 0 {
			continue
		}
		sortTraceRows(fresh, false)
		if s.ctx.Globals.JSON {
			if err := traceJSONLines(s, fresh); err != nil {
				return err
			}
			continue
		}
		s.ui.Table(nil, traceTableRows(fresh))
	}
}

// traceJSONLines writes one compact JSON object per row to stdout, the
// --follow --json shape: a stream has no closing bracket to wait for.
func traceJSONLines(s *session, rows []traceRow) error {
	enc := json.NewEncoder(s.ctx.Stdout)
	enc.SetEscapeHTML(false)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// traceBody prints one entry's headers and bodies. Ids are per twin, so
// every twin is asked unless --service narrowed it; a hit in several twins
// shows the first by name and says so.
func traceBody(s *session, twins []*traceTwin, id int) error {
	ctx := context.Background()
	var hits []traceRow
	for _, t := range twins {
		r, err := t.find(ctx, id)
		if err != nil {
			if !noTrace(err) {
				s.ui.Warn("%s: could not read the trace: %v", t.name, err)
			}
			continue
		}
		if r != nil {
			hits = append(hits, traceRow{Twin: t.name, Request: *r})
		}
	}
	if len(hits) == 0 {
		s.ui.Fail("No trace entry %d in the twins asked", id)
		s.ui.Next("veris sandbox trace")
		return printed(1)
	}
	if len(hits) > 1 {
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.Twin)
		}
		s.ui.Warn("entry %d exists in %s; showing %s (pass --service to choose)", id, strings.Join(names, " and "), hits[0].Twin)
	}
	hit := hits[0]
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, hit)
	}
	s.ui.Info("%s #%d  %s  %s %s → %s  %d ms  %s", hit.Twin, hit.ID, hit.Tier, hit.Method, hit.Path,
		traceStatus(hit.Status), hit.DurationMS, traceTime(hit.TS))
	sections := []struct {
		title string
		text  *string
		json  bool
	}{
		{"Request headers", hit.RequestHeaders, false},
		{"Request body", hit.RequestBody, true},
		{"Response headers", hit.ResponseHeaders, false},
		{"Response body", hit.ResponseBody, true},
	}
	for _, sec := range sections {
		s.ui.Info("%s", sec.title)
		for _, line := range traceSection(sec.text, sec.json) {
			s.ui.Detail("%s", line)
		}
	}
	return nil
}

// traceSection renders one transcript field as lines. Headers arrive as a
// JSON object of lower-cased names and print one per line; a body that is
// JSON is re-indented, anything else is printed as it was; nothing at all
// (a hang that never answered) is said.
func traceSection(text *string, body bool) []string {
	if text == nil {
		return []string{"(none)"}
	}
	if strings.TrimSpace(*text) == "" {
		return []string{"(empty)"}
	}
	if !body {
		var headers map[string]string
		if json.Unmarshal([]byte(*text), &headers) == nil {
			names := make([]string, 0, len(headers))
			for n := range headers {
				names = append(names, n)
			}
			sort.Strings(names)
			lines := make([]string, 0, len(names))
			for _, n := range names {
				lines = append(lines, n+": "+headers[n])
			}
			return lines
		}
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(*text), "", "  ") == nil {
		return strings.Split(pretty.String(), "\n")
	}
	return strings.Split(strings.TrimRight(*text, "\n"), "\n")
}
