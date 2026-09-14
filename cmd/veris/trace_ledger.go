package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
)

// The ledger side of `veris sandbox trace`: one read of GET
// /v1/sandboxes/{id}/ledger, which is the whole sandbox's stream -- every
// twin's requests, every data plane's statements, the sandbox's own world
// events -- already merged and sequenced by the control plane. A control
// plane that has no such route hands the command back to the per-twin merge
// in trace.go, which is what every sandbox had before the ledger.

// ledgerHeader is the table's columns. Seq is sandbox-wide, so it is what
// --body takes; At is wall clock in the reader's own zone, because a ledger
// spans twins whose virtual clocks may not agree.
var ledgerHeader = []string{"  Seq", "At", "Source", "Type", "Plane", "Event", "Outcome", "ms"}

// ledgerSummaryWidth bounds a statement or a world detail in the Event
// column: enough to recognise, never enough to wrap the table.
const ledgerSummaryWidth = 60

// traceLedger reads and prints the sandbox's ledger. It reports done=false
// only when the control plane has no ledger route at all, which is the
// caller's signal to fall back to the per-twin merge; every other outcome,
// success or failure, is done.
func traceLedger(s *session, c *api.Client, sb *api.Sandbox, o traceOptions) (bool, error) {
	if o.body > 0 {
		return traceLedgerBody(s, c, sb.ID, int64(o.body))
	}
	bg := context.Background()
	l, err := c.SandboxLedger(bg, sb.ID, ledgerQuery(o, int64(o.since), o.limit, "time", "desc"))
	if api.NoLedgerRoute(err) {
		return false, nil
	}
	if err != nil {
		return true, s.fail("read", "the ledger of sandbox "+sb.ID, err)
	}
	if readable := traceLedgerSources(s, l.Sources); readable == 0 && len(l.Sources) > 0 && len(l.Events) == 0 {
		// Nothing in the sandbox could be read and nothing was returned:
		// every source is already a ! line, and an empty table would read
		// as "nothing happened" rather than "nothing was legible".
		return true, printed(1)
	}
	events := l.Events
	// The watermark --follow starts from is what was SEEN, not what was
	// printed: events the filters or the cut dropped have still been read
	// past. page.watermark_seq covers the ones the filters hid, but it is
	// only a safe mark once the page says there is no more to drain.
	last := max(int64(o.since), maxLedgerSeq(events))
	if !l.Page.HasMore && l.Page.WatermarkSeq > last {
		last = l.Page.WatermarkSeq
	}
	// With --follow the first batch reads tail -f style: the newest events
	// were selected, and are shown oldest first like every batch after.
	var top api.LedgerEvent
	if len(events) > 0 {
		top = events[0]
		if o.follow {
			sortLedgerEvents(events)
		}
	}
	switch {
	case o.follow && s.ctx.Globals.JSON:
		if err := jsonLines(s, events); err != nil {
			return true, err
		}
	case s.ctx.Globals.JSON:
		if events == nil {
			events = []api.LedgerEvent{}
		}
		return true, printJSON(s.ctx.Stdout, events)
	case len(events) == 0:
		s.ui.Info("No events recorded%s", traceLedgerScope(o))
		if !o.follow {
			s.ui.Next("veris run")
		}
	default:
		s.ui.Table(ledgerHeader, ledgerTableRows(events))
		s.ui.Link(fmt.Sprintf("veris sandbox trace --body %d   (request and response of one event)", top.Seq))
	}
	if !o.follow {
		return true, nil
	}
	return true, traceLedgerFollow(s, c, sb.ID, o, last)
}

// ledgerQuery is one read's parameters. The list reads are always
// detail=summary: a page of a thousand events with every header and body
// would be cut by the client's own body limit, and --body fetches the one
// event a reader actually wants whole.
func ledgerQuery(o traceOptions, afterSeq int64, limit int, order, dir string) api.LedgerQuery {
	q := api.LedgerQuery{
		AfterSeq: afterSeq,
		Order:    order,
		Dir:      dir,
		Limit:    limit,
		Detail:   "summary",
		Source:   o.source,
		Plane:    o.plane,
		Type:     o.typ,
		Tier:     o.tier,
		Session:  o.session,
	}
	if q.Source == "" {
		// --service names a twin on either path; on this one it is the
		// source filter, so the flag keeps working against a ledger.
		q.Source = o.service
	}
	if o.mutating {
		mutating := true
		q.Mutating = &mutating
	}
	return q
}

// traceLedgerSources reports every source whose ledger is not whole -- a
// member too old to keep one, one still catching up, one scrubbed by a
// capture -- and returns how many could be read at all, so a sandbox that
// answered nothing legible is not printed as an empty one.
func traceLedgerSources(s *session, sources []api.LedgerSource) (readable int) {
	for _, src := range sources {
		name := ledgerSourceName(src.Kind, src.Name)
		reason := ""
		if src.Error != "" {
			reason = ": " + src.Error
		}
		switch src.Ledger {
		case api.LedgerUnavailable:
			s.ui.Warn("%s: no ledger%s", name, reason)
		case api.LedgerComplete:
			readable++
		default:
			readable++
			s.ui.Warn("%s: ledger %s%s", name, src.Ledger, reason)
		}
	}
	return readable
}

// ledgerSourceName is what a source is called in a line of output: its
// registry name, or "sandbox" for the world's own events, which have none.
func ledgerSourceName(kind, name string) string {
	if name != "" {
		return name
	}
	if kind != "" {
		return kind
	}
	return "sandbox"
}

// maxLedgerSeq is the highest seq in a page, 0 for an empty one.
func maxLedgerSeq(events []api.LedgerEvent) int64 {
	var top int64
	for _, e := range events {
		top = max(top, e.Seq)
	}
	return top
}

// sortLedgerEvents orders events oldest first. seq is sandbox-wide and
// monotonic at the ledger's own write, so it orders the stream on its own.
func sortLedgerEvents(events []api.LedgerEvent) {
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })
}

// traceLedgerScope names what an empty result was filtered by, for the "No
// events recorded" line.
func traceLedgerScope(o traceOptions) string {
	var parts []string
	if source := ledgerQuery(o, 0, 0, "", "").Source; source != "" {
		parts = append(parts, "from "+source)
	}
	if o.typ != "" {
		parts = append(parts, "of type "+o.typ)
	}
	if o.plane != "" {
		parts = append(parts, "on the "+o.plane+" plane")
	}
	if o.tier != "" {
		parts = append(parts, "of tier "+o.tier)
	}
	if o.session != "" {
		parts = append(parts, "in session "+o.session)
	}
	if o.mutating {
		parts = append(parts, "that changed the world")
	}
	if o.since > 0 {
		parts = append(parts, fmt.Sprintf("since seq %d", o.since))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

// ledgerTableRows renders events for the table.
func ledgerTableRows(events []api.LedgerEvent) [][]string {
	out := make([][]string, 0, len(events))
	for _, e := range events {
		out = append(out, []string{
			"  " + strconv.FormatInt(e.Seq, 10),
			ledgerTime(e.At),
			ledgerSourceName(e.Source.Kind, e.Source.Name),
			e.Type,
			e.Plane,
			ledgerEventSummary(e),
			ledgerOutcome(e.Outcome),
			ledgerDuration(e.DurationMS),
		})
	}
	return out
}

// ledgerTime renders an event's wall clock in the reader's own zone, to the
// millisecond. The sandbox's virtual clock is on the event too (--json
// carries world_time), but it is per source and can be frozen, so it is not
// what a stream is read along.
func ledgerTime(t api.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("15:04:05.000")
}

// ledgerDuration renders how long the event took; a world event and a hang
// that never finished have none.
func ledgerDuration(ms *int64) string {
	if ms == nil {
		return "—"
	}
	return strconv.FormatInt(*ms, 10)
}

// ledgerEventSummary is the one-column account of what the event was: the
// call, the statement, the connection's turn, the delivery's target or the
// world's own change. A type this binary does not model is summarised by
// its payload, so a newer control plane still prints something true.
func ledgerEventSummary(e api.LedgerEvent) string {
	switch {
	case e.HTTP != nil:
		return strings.TrimSpace(e.HTTP.Method + " " + e.HTTP.Path)
	case e.SQL != nil:
		return strings.TrimSpace(e.SQL.CommandTag + " " + oneLine(e.SQL.Statement, ledgerSummaryWidth))
	case e.Connection != nil:
		return strings.TrimSpace(e.Connection.Event + " " + e.Connection.Peer)
	case e.Delivery != nil:
		return strings.TrimSpace(e.Delivery.Method + " " + e.Delivery.Destination)
	case e.World != nil:
		return strings.TrimSpace(e.World.Event + " " + oneLine(compactJSON(e.World.Detail), ledgerSummaryWidth))
	default:
		return oneLine(compactJSON(e.Payload()), ledgerSummaryWidth)
	}
}

// ledgerOutcome renders how the event ended: the protocol's own token where
// there is one (an HTTP status, a SQLSTATE), the fault that answered instead
// where there is not, and — for a hang that answered nothing at all.
func ledgerOutcome(o api.LedgerOutcome) string {
	switch {
	case o.Code != "":
		return o.Code
	case o.Fault != nil && o.Fault.Kind != "":
		return o.Fault.Kind
	case o.OK:
		return "ok"
	default:
		return "—"
	}
}

// traceLedgerFollow polls the ledger on the interval and prints what arrived
// above the watermark, oldest first, until the context ends. Each tick
// drains: a page that says there is more is read on at once, so a burst is
// printed in one batch rather than trickling in over the next minute. A
// control plane that fails to answer is one ! line until it answers again,
// so a plane that is down does not fill the screen.
func traceLedgerFollow(s *session, c *api.Client, id string, o traceOptions, last int64) error {
	ctx, stop := traceFollowContext()
	defer stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(traceFollowInterval):
		}
		var fresh []api.LedgerEvent
		for {
			// order=arrival, so the drain follows the ledger's own commit
			// order and after_seq cannot step over an event that landed
			// out of wall-clock order.
			l, err := c.SandboxLedger(ctx, id, ledgerQuery(o, last, traceMaxLimit, "arrival", "asc"))
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if !failing {
					s.ui.Warn("could not read the ledger: %v", err)
				}
				failing = true
				break
			}
			failing = false
			fresh = append(fresh, l.Events...)
			last = max(last, maxLedgerSeq(l.Events))
			if !l.Page.HasMore {
				// Drained: the page's watermark is now a safe resume point,
				// and covers events the filters kept out of this batch.
				if l.Page.WatermarkSeq > last {
					last = l.Page.WatermarkSeq
				}
				break
			}
			if len(l.Events) == 0 {
				// has_more with nothing in hand would loop on one page
				// forever; wait for the next tick instead.
				break
			}
		}
		if len(fresh) == 0 {
			continue
		}
		sortLedgerEvents(fresh)
		if s.ctx.Globals.JSON {
			if err := jsonLines(s, fresh); err != nil {
				return err
			}
			continue
		}
		s.ui.Table(nil, ledgerTableRows(fresh))
	}
}

// traceLedgerBody prints one event whole, by its sandbox-wide seq. Like
// traceLedger it reports done=false only when there is no ledger route: a
// 404 that names the event is a wrong seq and is said, not degraded into a
// scan of every twin.
func traceLedgerBody(s *session, c *api.Client, id string, seq int64) (bool, error) {
	ev, err := c.SandboxLedgerEvent(context.Background(), id, seq)
	if api.NoLedgerRoute(err) {
		return false, nil
	}
	if api.IsStatus(err, 404) {
		s.ui.Fail("No ledger event %d in sandbox %s", seq, id)
		s.ui.Next("veris sandbox trace")
		return true, printed(1)
	}
	if err != nil {
		return true, s.fail("read", fmt.Sprintf("ledger event %d of sandbox %s", seq, id), err)
	}
	if s.ctx.Globals.JSON {
		return true, printJSON(s.ctx.Stdout, ev)
	}
	s.ui.Info("%s #%d  %s %s  %s → %s  %s ms  %s",
		ledgerSourceName(ev.Source.Kind, ev.Source.Name), ev.Seq, ev.Plane, ev.Tier,
		ledgerEventSummary(*ev), ledgerOutcome(ev.Outcome), ledgerDuration(ev.DurationMS), ledgerTime(ev.At))
	for _, line := range ledgerFacts(*ev) {
		s.ui.Detail("%s", line)
	}
	for _, sec := range ledgerSections(*ev) {
		s.ui.Info("%s", sec.title)
		for _, line := range traceSection(sec.text, sec.body) {
			s.ui.Detail("%s", line)
		}
	}
	return true, nil
}

// ledgerFacts are the event's own facts, under its header line: who made the
// call, what it moved the world to, what it was answered with, and which
// paths of it were masked before it was ever stored.
func ledgerFacts(e api.LedgerEvent) []string {
	var out []string
	if e.Actor.Credential != "" {
		actor := e.Actor.Credential
		if e.Actor.Ref != "" {
			actor += " " + e.Actor.Ref
		}
		out = append(out, "actor: "+actor)
	}
	if e.Session != "" {
		out = append(out, "session: "+e.Session)
	}
	if e.State.Before != nil || e.State.After != nil {
		out = append(out, fmt.Sprintf("state: %s → %s (%s)",
			ledgerVersion(e.State.Before), ledgerVersion(e.State.After), mutatingWord(e.State.Mutating)))
	}
	if e.Outcome.Error != "" {
		out = append(out, "error: "+e.Outcome.Error)
	}
	if e.Outcome.Fault != nil && e.Outcome.Fault.Kind != "" {
		fault := e.Outcome.Fault.Kind
		if e.Outcome.Fault.ID != "" {
			fault += " " + e.Outcome.Fault.ID
		}
		out = append(out, "fault: "+fault)
	}
	return out
}

func ledgerVersion(v *int64) string {
	if v == nil {
		return "—"
	}
	return strconv.FormatInt(*v, 10)
}

func mutatingWord(mutating bool) string {
	if mutating {
		return "mutating"
	}
	return "read"
}

// ledgerSection is one titled block of the body view.
type ledgerSection struct {
	title string
	text  *string
	body  bool
}

// ledgerSections are the blocks the body view prints: an http event's four,
// the way the per-twin trace has always shown one, and for every other type
// the payload as the ledger holds it.
func ledgerSections(e api.LedgerEvent) []ledgerSection {
	if e.HTTP != nil {
		return []ledgerSection{
			{"Request headers", headersText(e.HTTP.Request.Headers), false},
			{"Request body", bodyText(e.HTTP.Request.Body), true},
			{"Response headers", headersText(e.HTTP.Response.Headers), false},
			{"Response body", bodyText(e.HTTP.Response.Body), true},
		}
	}
	return []ledgerSection{{"Payload", rawText(e.Payload()), true}}
}

// headersText renders headers the way traceSection reads them: a JSON object
// of names to values. Nil (none recorded, or a summary read) stays nil, and
// prints as "(none)".
func headersText(headers map[string]string) *string {
	if headers == nil {
		return nil
	}
	encoded, err := json.Marshal(headers)
	if err != nil {
		return nil
	}
	text := string(encoded)
	return &text
}

// bodyText renders a recorded body: a JSON string as the text it holds, any
// other JSON as itself, so traceSection can re-indent it.
func bodyText(body json.RawMessage) *string {
	if text := jsonString(body); text != nil {
		return text
	}
	return rawText(body)
}

// rawText is a raw JSON value as text, nil when there is none.
func rawText(raw json.RawMessage) *string {
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return nil
	}
	text := string(raw)
	return &text
}

// jsonString is the value of a JSON string, or nil when raw is anything else.
func jsonString(raw json.RawMessage) *string {
	if len(raw) == 0 || raw[0] != '"' {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil
	}
	return &text
}

// compactJSON is a raw JSON value on one line, "" when there is none.
func compactJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 || string(raw) == "null" {
		return ""
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	return out.String()
}
