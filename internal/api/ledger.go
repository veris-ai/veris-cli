package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// The sandbox ledger: one time-ordered stream of everything a sandbox saw,
// merged from every twin plus the sandbox's own world events, served by the
// control plane at GET /v1/sandboxes/{id}/ledger. Wire format
// "veris.sandbox-ledger/1"; the types below mirror it field for field, and
// every event carries the bytes it arrived as so --json prints what the
// control plane sent rather than what this package models.

// LedgerFormat is the envelope's format tag, the one version this client
// knows how to read.
const LedgerFormat = "veris.sandbox-ledger/1"

// Source states a member's ledger can be in. Anything but complete means the
// merged stream has a gap the reader should be told about.
const (
	LedgerComplete    = "complete"
	LedgerPartial     = "partial"
	LedgerUnavailable = "unavailable"
	LedgerScrubbed    = "scrubbed"
)

// Ledger is one page of the sandbox-wide stream: the clock it was read
// under, every source that could have contributed (whether it did or not),
// the events themselves, and where the next page starts.
type Ledger struct {
	Format    string         `json:"format"`
	SandboxID string         `json:"sandbox_id"`
	Clock     LedgerClock    `json:"clock"`
	Sources   []LedgerSource `json:"sources"`
	Events    []LedgerEvent  `json:"events"`
	Page      LedgerPage     `json:"page"`
}

// LedgerClock is the sandbox's virtual clock as the page was read.
type LedgerClock struct {
	Mode          string `json:"mode"`
	WorldTime     Time   `json:"world_time"`
	OffsetSeconds int64  `json:"offset_seconds"`
}

// LedgerSource is one contributor to the merge: a twin, or the sandbox
// itself (Kind "sandbox", no name). Ledger is its state, and the reason a
// page can be short: a member of a pre-ledger build answers "unavailable"
// rather than failing the whole read.
type LedgerSource struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Ledger   string `json:"ledger"`
	LastSeq  int64  `json:"last_seq"`
	LagMS    *int64 `json:"lag_ms"`
	Error    string `json:"error"`
}

// LedgerPage is where the read stopped. WatermarkSeq is the after_seq a
// follow may resume from, and is only a safe one once HasMore is false.
type LedgerPage struct {
	Order        string `json:"order"`
	Dir          string `json:"dir"`
	Limit        int    `json:"limit"`
	Count        int    `json:"count"`
	HasMore      bool   `json:"has_more"`
	NextCursor   string `json:"next_cursor"`
	WatermarkSeq int64  `json:"watermark_seq"`
}

// LedgerSourceRef names the source of one event.
type LedgerSourceRef struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// LedgerFault is the injected fault an event was answered by, if any.
type LedgerFault struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// LedgerOutcome is how the event ended. Code is the protocol's own token as
// a string -- an HTTP status, a SQLSTATE -- and is empty when there was
// none: a hang answered nothing, and is Fault.Kind "hang" instead.
type LedgerOutcome struct {
	OK    bool         `json:"ok"`
	Code  string       `json:"code"`
	Fault *LedgerFault `json:"fault"`
	Error string       `json:"error"`
}

// LedgerState is the world version around the event. Before and After are
// null where the source cannot say cheaply; Mutating is what the reader
// filters on either way.
type LedgerState struct {
	Before   *int64 `json:"before"`
	After    *int64 `json:"after"`
	Mutating bool   `json:"mutating"`
}

// LedgerActor is who made the call, as the source resolved it: a credential
// kind and what it resolved to, never the secret itself.
type LedgerActor struct {
	Credential string `json:"credential"`
	Ref        string `json:"ref"`
}

// LedgerOp names the operation a vendor call carries beyond its path: a
// GraphQL or MCP method name, a gRPC procedure. Null for plain REST.
type LedgerOp struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// LedgerHTTPSide is one direction of an http event. Body is whatever JSON
// the source recorded -- a string for a text body, an object for a parsed
// one -- and is absent under detail=summary, where Bytes and Truncated
// still say how much there was.
type LedgerHTTPSide struct {
	Status    *int              `json:"status"`
	Headers   map[string]string `json:"headers"`
	Body      json.RawMessage   `json:"body"`
	Bytes     int64             `json:"bytes"`
	Truncated bool              `json:"truncated"`
	Encoding  string            `json:"encoding"`
}

// LedgerHTTP is the http payload. Query stays raw: its values are the
// source's own JSON and are shown with the rest of the payload rather than
// re-rendered as a query string.
type LedgerHTTP struct {
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Query    json.RawMessage `json:"query"`
	Op       *LedgerOp       `json:"op"`
	Request  LedgerHTTPSide  `json:"request"`
	Response LedgerHTTPSide  `json:"response"`
}

// LedgerSQL is the sql payload, as much of it as a reader summarises on:
// the statement and its command tag. The rest of the payload is in the
// event's own bytes, which --json and the body view print.
type LedgerSQL struct {
	Database    string `json:"database"`
	Application string `json:"application"`
	Statement   string `json:"statement"`
	CommandTag  string `json:"command_tag"`
}

// LedgerConnection is the connection payload: what happened to a data-plane
// connection, and with whom.
type LedgerConnection struct {
	Event  string `json:"event"`
	Peer   string `json:"peer"`
	Reason string `json:"reason"`
}

// LedgerDelivery is the delivery payload: one outbound attempt at a rule's
// destination.
type LedgerDelivery struct {
	DeliveryID  string `json:"delivery_id"`
	RuleID      string `json:"rule_id"`
	Attempt     int    `json:"attempt"`
	Destination string `json:"destination"`
	Method      string `json:"method"`
}

// LedgerWorld is the world payload: something that happened to the sandbox
// rather than in one of its twins, and who asked for it.
type LedgerWorld struct {
	Event  string          `json:"event"`
	Detail json.RawMessage `json:"detail"`
	By     string          `json:"by"`
}

// LedgerEvent is one event of the stream. Type names which payload key is
// set; a type this client does not model still round-trips, because the
// bytes the event arrived as are kept and re-emitted verbatim.
type LedgerEvent struct {
	Seq        int64             `json:"seq"`
	ID         json.Number       `json:"id"`
	Source     LedgerSourceRef   `json:"source"`
	Plane      string            `json:"plane"`
	Type       string            `json:"type"`
	Direction  string            `json:"direction"`
	Tier       string            `json:"tier"`
	At         Time              `json:"at"`
	WorldTime  Time              `json:"world_time"`
	DurationMS *int64            `json:"duration_ms"`
	Session    string            `json:"session"`
	Actor      LedgerActor       `json:"actor"`
	State      LedgerState       `json:"state"`
	Outcome    LedgerOutcome     `json:"outcome"`
	HTTP       *LedgerHTTP       `json:"http,omitempty"`
	SQL        *LedgerSQL        `json:"sql,omitempty"`
	Connection *LedgerConnection `json:"connection,omitempty"`
	Delivery   *LedgerDelivery   `json:"delivery,omitempty"`
	World      *LedgerWorld      `json:"world,omitempty"`

	// raw is the event as the control plane sent it, kept so --json prints
	// the whole record -- origin, correlation, redacted paths, a payload
	// shape newer than this binary -- and not the subset modelled above.
	raw json.RawMessage
}

// ledgerEventFields is LedgerEvent without its methods, so the decoder and
// the encoder can use the struct tags without recursing.
type ledgerEventFields LedgerEvent

// UnmarshalJSON decodes the event and keeps its bytes.
func (e *LedgerEvent) UnmarshalJSON(b []byte) error {
	var fields ledgerEventFields
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	*e = LedgerEvent(fields)
	e.raw = append(json.RawMessage(nil), b...)
	return nil
}

// MarshalJSON writes the bytes the event arrived as, so a page read and
// printed under --json is byte-faithful to what the control plane served.
// An event this process built itself has no such bytes and is encoded from
// its fields.
func (e LedgerEvent) MarshalJSON() ([]byte, error) {
	if len(e.raw) > 0 {
		return e.raw, nil
	}
	return json.Marshal(ledgerEventFields(e))
}

// Payload is the event's payload as it arrived: the value of the key named
// by Type. Nil when the event carries none, or when it was not decoded from
// the wire.
func (e LedgerEvent) Payload() json.RawMessage {
	if len(e.raw) == 0 || e.Type == "" {
		return nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(e.raw, &keys); err != nil {
		return nil
	}
	payload := keys[e.Type]
	if len(bytes.TrimSpace(payload)) == 0 || string(payload) == "null" {
		return nil
	}
	return payload
}

// LedgerQuery is what a read asks for. A zero field is not sent, so the
// control plane's own defaults (order=time, dir=asc, limit=200,
// detail=summary) apply to what is left out.
type LedgerQuery struct {
	AfterSeq int64
	Cursor   string
	// Order is "time" (by the event's own clock) or "arrival" (by seq, the
	// order the ledger committed them in).
	Order string
	Dir   string
	Limit int
	// Detail is "summary" (no headers, bodies or parameters) or "full".
	Detail string
	// Source is a member's registry name, or the literal "sandbox" for the
	// sandbox's own world events.
	Source    string
	Plane     string
	Protocol  string
	Type      string
	Tier      string
	Direction string
	Session   string
	Mutating  *bool
	OK        *bool
	From      string
	To        string
	Path      string
	Statement string
}

// values renders the query. url.Values.Encode sorts the keys, so the query
// string a given read sends is stable.
func (q LedgerQuery) values() url.Values {
	v := url.Values{}
	if q.AfterSeq > 0 {
		v.Set("after_seq", strconv.FormatInt(q.AfterSeq, 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	for name, value := range map[string]string{
		"cursor":    q.Cursor,
		"order":     q.Order,
		"dir":       q.Dir,
		"detail":    q.Detail,
		"source":    q.Source,
		"plane":     q.Plane,
		"protocol":  q.Protocol,
		"type":      q.Type,
		"tier":      q.Tier,
		"direction": q.Direction,
		"session":   q.Session,
		"from":      q.From,
		"to":        q.To,
		"path":      q.Path,
		"statement": q.Statement,
	} {
		if value != "" {
			v.Set(name, value)
		}
	}
	if q.Mutating != nil {
		v.Set("mutating", strconv.FormatBool(*q.Mutating))
	}
	if q.OK != nil {
		v.Set("ok", strconv.FormatBool(*q.OK))
	}
	return v
}

// SandboxLedger reads one page of a sandbox's ledger (GET
// /v1/sandboxes/{id}/ledger). The answer is always JSON: the ndjson framing
// the route also serves is of no use to a client that buffers the body.
//
// A control plane that predates the ledger has no such route and answers
// FastAPI's own 404; NoLedgerRoute tells that apart from a sandbox that does
// not exist.
func (c *Client) SandboxLedger(ctx context.Context, id string, q LedgerQuery) (*Ledger, error) {
	path := "/v1/sandboxes/" + pathEscape(id) + "/ledger"
	if v := q.values(); len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out Ledger
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SandboxLedgerEvent reads one event whole, headers and bodies included
// (GET /v1/sandboxes/{id}/ledger/{seq}). Seq is sandbox-wide, which is why
// this needs no source: one number names one event.
func (c *Client) SandboxLedgerEvent(ctx context.Context, id string, seq int64) (*LedgerEvent, error) {
	path := "/v1/sandboxes/" + pathEscape(id) + "/ledger/" + strconv.FormatInt(seq, 10)
	var out LedgerEvent
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NoLedgerRoute reports the one 404 that means "this control plane serves no
// ledger at all": FastAPI's answer for a path it has no route for is exactly
// {"detail": "Not Found"}, while every 404 the ledger routes themselves send
// names what was missing (the sandbox, the event). A caller that cannot tell
// them apart would degrade a mistyped seq into a silent fallback.
func NoLedgerRoute(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound && e.Detail == "Not Found"
}
