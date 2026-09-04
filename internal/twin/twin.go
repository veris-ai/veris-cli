// Package twin speaks to one twin's /veris/* control routes.
//
// A twin is one mock vendor service inside a sandbox. Every twin serves the
// same small control plane -- health, the world's rows, writes, reset, the
// request trace, its manual -- and the control plane knows nothing about
// which vendor the twin mocks, so one client serves every twin. There is no
// authentication: the control URL the control plane hands out is the
// capability, and whoever holds it may read and rewrite the world.
//
// Two shapes of twin exist. An HTTP twin (veris_core.VerisService) serves the
// full surface. A data-plane twin (postgres) has no HTTP vendor to mock and
// serves an introspected subset: health, schema, reset and seed. Everything
// else answers 404 there, which this package reports as ErrNotSupported so a
// caller can tell "this twin has no such route" from "that entity does not
// exist".
//
// Which 404s carry that meaning depends on the route. Operations and Seed
// are optional by design, so any 404 on them is the route being absent and
// is marked whatever the body says -- an HTTP twin answers a stray
// /veris/seed with its vendor's own 404 shape through the catch-all, not
// FastAPI's. On every other route only FastAPI's own {"detail":"Not Found"}
// (the framework finding no route, as the postgres twin's gaps do) is
// marked; a handler's 404 for a missing entity, or a bare 404 from something
// between here and the twin, stays a plain *Error.
package twin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one twin. ControlURL is the twin's control_url as the
// control plane reports it, like https://…/s/<sandbox>/<twin>, and every
// route hangs under it at /veris/*.
type Client struct {
	ControlURL string
	HTTP       *http.Client
}

// New builds a client for one twin. A trailing slash on the URL is dropped
// so the paths join cleanly.
func New(controlURL string) *Client {
	return &Client{
		ControlURL: strings.TrimSuffix(controlURL, "/"),
		HTTP:       &http.Client{Timeout: 30 * time.Second},
	}
}

// ErrNotSupported is a 404 that means "this twin does not serve that route":
// operations on a twin that publishes none, folder imports on a twin without
// tree ingest, and every HTTP-only route on the postgres twin. It is
// distinct from a 404 that names a missing entity type, which stays a plain
// *Error. Test with errors.Is.
var ErrNotSupported = errors.New("this twin does not serve that route")

// Error is any non-2xx answer from a twin. Detail is the human line. A 422's
// detail arrives as a list of strings (SeedError reasons), a list of
// pydantic {msg, loc, type} objects, or a bare string; Reasons carries the
// list one entry per item and Detail joins them.
type Error struct {
	Status  int
	Method  string
	Path    string
	Detail  string
	Reasons []string

	// notSupported marks a 404 that is the framework's own answer for an
	// unrouted path rather than a handler's answer for a missing thing.
	notSupported bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("[%d] %s", e.Status, e.Detail)
}

// Is lets errors.Is(err, ErrNotSupported) hold for the 404s that mean the
// route is absent, while the *Error keeps its status and path for anyone
// who wants them.
func (e *Error) Is(target error) bool {
	return target == ErrNotSupported && e.notSupported
}

// Health is GET /veris/health. The postgres twin reports only service and
// status; Schema and StateVersion are the HTTP twin's.
type Health struct {
	Status       string `json:"status"`
	Service      string `json:"service"`
	Schema       string `json:"schema,omitempty"`
	StateVersion int    `json:"state_version,omitempty"`
}

// Counts is the bare GET /veris/data: one row count per table, plus the
// sandbox-shared clock and client singletons counted as 1 each.
type Counts struct {
	Counts       map[string]int `json:"counts"`
	StateVersion int            `json:"state_version"`
}

// Rows is one page of GET /veris/data?entity_type=…. The route takes a
// table, a limit and an offset and no sort, so the order is the twin's own
// and nothing here may promise otherwise. A column the twin declares as
// file content arrives as a placeholder rather than the bytes.
type Rows struct {
	EntityType string           `json:"entity_type"`
	Rows       []map[string]any `json:"rows"`
	Total      int              `json:"total"`
	Limit      int              `json:"limit"`
	Offset     int              `json:"offset"`
}

// Write is what the three /veris/data writes return: the per-table counts
// under the key that names the verb, and any warnings the write raised
// (a fault row that will never fire, a clock moved backwards).
type Write struct {
	Added    map[string]int `json:"added,omitempty"`
	Updated  map[string]int `json:"updated,omitempty"`
	Deleted  map[string]int `json:"deleted,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

// ResetRequest chooses what world replaces the current one. At most one of
// Profile and Data may be given; neither restores the boot profile. Data is
// sent whenever it is non-nil: an empty map is a deliberate request for an
// empty world, which the twin distinguishes from no payload at all.
//
// The postgres twin takes no payload: its reset restores the boot world and
// ignores the body. Reset refuses to report that as success when a Profile
// or Data was asked for.
type ResetRequest struct {
	Profile string
	Data    map[string]any
}

// Reset is POST /veris/reset. An HTTP twin answers {reset, seeded}; the
// postgres twin answers {ok}.
type Reset struct {
	Reset  bool           `json:"reset"`
	Seeded map[string]int `json:"seeded,omitempty"`
	OK     bool           `json:"ok"`
}

// Seed is POST /veris/seed on the postgres twin.
type Seed struct {
	OK bool `json:"ok"`
}

// PostgresSchema is the postgres twin's GET /veris/schema: the customer's
// own tables introspected from information_schema, keyed "schema.table".
// An HTTP twin's schema is a JSON Schema object with "properties" instead;
// the "tables" key is what tells them apart.
type PostgresSchema struct {
	Tables map[string]PostgresTable `json:"tables"`
}

// PostgresTable is one introspected table.
type PostgresTable struct {
	Columns []PostgresColumn `json:"columns"`
}

// PostgresColumn is one introspected column.
type PostgresColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

// Operations is GET /veris/operations: what the twin actually implements,
// as opposed to what the vendor's own listing claims. REST and GraphQL
// twins fill Operations; an MCP twin fills MCP; a twin may serve both a
// REST and an MCP surface.
type Operations struct {
	Service    string      `json:"service"`
	Total      int         `json:"total"`
	Operations []Operation `json:"operations,omitempty"`
	MCP        *MCPSurface `json:"mcp,omitempty"`
}

// Operation is one entry of Operations.Operations: {method, path} on a REST
// twin, {type, field} on a GraphQL twin.
type Operation struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Type   string `json:"type,omitempty"`
	Field  string `json:"field,omitempty"`
}

// MCPSurface is the hosted MCP endpoint and the tools it resolves.
type MCPSurface struct {
	Path  *string   `json:"path"`
	Tools []MCPTool `json:"tools"`
}

// MCPTool is one implemented tool.
type MCPTool struct {
	Tool string `json:"tool"`
}

// Tiers a request trace row may carry, as clients see them. The twin folds
// its fallback tiers into "handler" before answering, so those never appear.
const (
	TierHandler  = "handler"
	TierFault    = "fault"
	TierControl  = "control"
	TierDelivery = "delivery"
)

// RequestsQuery narrows GET /veris/requests. Zero values send nothing and
// take the twin's defaults: 50 rows, newest first, every tier.
type RequestsQuery struct {
	Limit int    // 1..1000
	Order string // "desc" (default) or "asc"
	Tier  string // one of the Tier constants
}

// Request is one row of the trace log. Status is nil when the twin sent
// nothing at all (a hang fault). Bodies and headers are the redacted,
// capped transcripts; ResponseHeaders is nil when no response started.
type Request struct {
	ID              int     `json:"id"`
	TS              int     `json:"ts"`
	Method          string  `json:"method"`
	Path            string  `json:"path"`
	Status          *int    `json:"status"`
	Tier            string  `json:"tier"`
	DurationMS      int     `json:"duration_ms"`
	StateVersion    int     `json:"state_version"`
	RequestBody     *string `json:"request_body"`
	ResponseBody    *string `json:"response_body"`
	RequestHeaders  *string `json:"request_headers"`
	ResponseHeaders *string `json:"response_headers"`
}

// Probe is POST /veris/client/probe: the client registration row after a
// synchronous probe of default_base_url. ProbeState is "answered" when an
// HTTP endpoint replied -- any endpoint, so LastProbeResult is for the
// caller to check against their own app -- and "unreachable" otherwise.
type Probe struct {
	ID              int     `json:"id"`
	DefaultBaseURL  *string `json:"default_base_url"`
	BaseURLRevision int     `json:"base_url_revision"`
	ProbeState      string  `json:"probe_state"`
	ProbedRevision  *int    `json:"probed_revision"`
	LastProbeAt     *int    `json:"last_probe_at"`
	LastProbeResult any     `json:"last_probe_result"`
}

// Health is GET /veris/health.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var out Health
	if err := c.do(ctx, http.MethodGet, "/veris/health", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Counts is the bare GET /veris/data.
func (c *Client) Counts(ctx context.Context) (*Counts, error) {
	var out Counts
	if err := c.do(ctx, http.MethodGet, "/veris/data", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Rows reads one page of a table. limit and offset are sent only when
// positive, so zero takes the twin's defaults (100 rows from the start).
func (c *Client) Rows(ctx context.Context, entityType string, limit, offset int) (*Rows, error) {
	if entityType == "" {
		return nil, errors.New("entity type is required; Counts reads the bare table list")
	}
	q := url.Values{"entity_type": {entityType}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	var out Rows
	if err := c.do(ctx, http.MethodGet, "/veris/data", q, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Add is POST /veris/data {data}: additive rows, keyed by table.
func (c *Client) Add(ctx context.Context, data map[string]any) (*Write, error) {
	return c.write(ctx, http.MethodPost, map[string]any{"data": nonNil(data)})
}

// AddProfile is POST /veris/data {profile}: additive rows from one of the
// twin's shipped seed profiles.
func (c *Client) AddProfile(ctx context.Context, profile string) (*Write, error) {
	if profile == "" {
		return nil, errors.New("profile name is required")
	}
	return c.write(ctx, http.MethodPost, map[string]any{"profile": profile})
}

// Patch is PATCH /veris/data {data}: edits to existing rows by primary key,
// and the only way to change the clock and client singletons.
func (c *Client) Patch(ctx context.Context, data map[string]any) (*Write, error) {
	return c.write(ctx, http.MethodPatch, map[string]any{"data": nonNil(data)})
}

// Delete is DELETE /veris/data {data}: rows to remove, by primary key.
func (c *Client) Delete(ctx context.Context, data map[string]any) (*Write, error) {
	return c.write(ctx, http.MethodDelete, map[string]any{"data": nonNil(data)})
}

func (c *Client) write(ctx context.Context, method string, body map[string]any) (*Write, error) {
	var out Write
	if err := c.do(ctx, method, "/veris/data", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reset is POST /veris/reset. The body is {profile}, {data} or {} exactly:
// the twin forbids unknown keys here because the wipe runs regardless, and
// an ignored payload would return 200 over an empty world.
func (c *Client) Reset(ctx context.Context, r ResetRequest) (*Reset, error) {
	if r.Profile != "" && r.Data != nil {
		return nil, errors.New("reset takes a profile or data, not both")
	}
	body := map[string]any{}
	switch {
	case r.Profile != "":
		body["profile"] = r.Profile
	case r.Data != nil:
		body["data"] = r.Data
	}
	var out Reset
	if err := c.do(ctx, http.MethodPost, "/veris/reset", nil, body, &out); err != nil {
		return nil, err
	}
	// The postgres twin answers {ok} and never reads the body, so a reset
	// that asked for a profile or rows got neither: say so rather than hand
	// back a success over the boot world.
	if len(body) > 0 && out.OK && !out.Reset {
		return nil, errors.New("this twin's reset takes no profile or data; it restores the boot world only")
	}
	return &out, nil
}

// Schema is GET /veris/schema, returned raw because the two kinds of twin
// answer different documents: an HTTP twin's per-table JSON Schema, the
// postgres twin's introspected {tables} (see PostgresSchema).
func (c *Client) Schema(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/veris/schema", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Manual is GET /veris/manual with the {"manual": …} envelope removed: the
// twin's own testing notes, as markdown.
func (c *Client) Manual(ctx context.Context) (string, error) {
	var out struct {
		Manual json.RawMessage `json:"manual"`
	}
	if err := c.do(ctx, http.MethodGet, "/veris/manual", nil, nil, &out); err != nil {
		return "", err
	}
	var manual string
	if err := json.Unmarshal(out.Manual, &manual); err != nil {
		return "", fmt.Errorf("the manual is not a string: %w", err)
	}
	return manual, nil
}

// Operations is GET /veris/operations. A twin that publishes no operation
// list has no such route; that 404 is ErrNotSupported.
func (c *Client) Operations(ctx context.Context) (*Operations, error) {
	var out Operations
	if err := optional404(c.do(ctx, http.MethodGet, "/veris/operations", nil, nil, &out)); err != nil {
		return nil, err
	}
	return &out, nil
}

// optional404 marks any 404 from a route a twin may simply not have as
// ErrNotSupported, whatever the body: an HTTP twin's catch-all answers such
// a path in its vendor's error shape, which parseError cannot tell from a
// handler's refusal.
func optional404(err error) error {
	var te *Error
	if errors.As(err, &te) && te.Status == http.StatusNotFound {
		te.notSupported = true
	}
	return err
}

// Requests is GET /veris/requests: the trace log, one row per request the
// twin served.
func (c *Client) Requests(ctx context.Context, q RequestsQuery) ([]Request, error) {
	params := url.Values{}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	switch q.Order {
	case "", "desc", "asc":
		if q.Order != "" {
			params.Set("order", q.Order)
		}
	default:
		return nil, fmt.Errorf("order %q is not asc or desc", q.Order)
	}
	switch q.Tier {
	case "", TierHandler, TierFault, TierControl, TierDelivery:
		if q.Tier != "" {
			params.Set("tier", q.Tier)
		}
	default:
		return nil, fmt.Errorf("tier %q is not one of %s, %s, %s, %s",
			q.Tier, TierHandler, TierFault, TierControl, TierDelivery)
	}
	var out struct {
		Requests []Request `json:"requests"`
	}
	if err := c.do(ctx, http.MethodGet, "/veris/requests", params, nil, &out); err != nil {
		return nil, err
	}
	return out.Requests, nil
}

// Probe is POST /veris/client/probe: re-verify the registered callback base
// URL now, and wait for the verdict.
func (c *Client) Probe(ctx context.Context) (*Probe, error) {
	var out Probe
	if err := c.do(ctx, http.MethodPost, "/veris/client/probe", nil, map[string]any{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Seed is POST /veris/seed {schema_sql} on the postgres twin: DDL applied
// to a staging copy and promoted under the twin's lock. An HTTP twin has no
// such route, and its catch-all answers the path with the vendor's own 404;
// any 404 here is ErrNotSupported.
func (c *Client) Seed(ctx context.Context, schemaSQL string) (*Seed, error) {
	var out Seed
	err := c.do(ctx, http.MethodPost, "/veris/seed", nil, map[string]any{"schema_sql": schemaSQL}, &out)
	if err := optional404(err); err != nil {
		return nil, err
	}
	return &out, nil
}

// maxBody bounds what is read of any answer. A manual or a wide schema is
// tens of kilobytes; a page of rows at limit=1000 can be a few megabytes.
const maxBody = 16 << 20

// defaultHTTP serves a Client whose HTTP field was left nil.
var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

// do sends one request and decodes a 2xx answer into out. Anything else
// becomes *Error, with the 404s that mean "no such route" marked so
// errors.Is(err, ErrNotSupported) holds.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	endpoint := c.ControlURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Read, not assigned: a Client built as a literal may be shared across
	// goroutines, and a write here would race.
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the twin at %s: %w", c.ControlURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	shown := path
	if len(query) > 0 {
		shown += "?" + query.Encode()
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(resp.StatusCode, method, shown, raw)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s answered %d with a body that is not the expected JSON: %w",
			method, shown, resp.StatusCode, err)
	}
	return nil
}

// parseError turns a non-2xx body into *Error. FastAPI wraps every refusal
// as {"detail": …}; the detail is a string for HTTPException, a list of
// strings for a SeedError, and a list of {msg, loc, type} objects when
// pydantic rejected the request body or a query parameter.
func parseError(status int, method, path string, raw []byte) *Error {
	e := &Error{Status: status, Method: method, Path: path}
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Detail) == 0 {
		// No FastAPI envelope: a vendor-shaped body from a twin's catch-all,
		// an HTML page, or nothing at all from something in the path. None
		// of these say the route is absent, so notSupported stays false;
		// the empty-body fallback to "Not Found" below must not be mistaken
		// for FastAPI's own.
		e.Detail = strings.TrimSpace(string(raw))
		if len(e.Detail) > 200 {
			e.Detail = e.Detail[:200]
		}
		if e.Detail == "" {
			e.Detail = http.StatusText(status)
		}
		return e
	}
	var text string
	if json.Unmarshal(envelope.Detail, &text) == nil {
		e.Detail = text
	} else {
		e.Reasons = parseReasons(envelope.Detail)
		e.Detail = strings.Join(e.Reasons, "; ")
	}
	if e.Detail == "" {
		e.Detail = http.StatusText(status)
	}
	// The framework's answer for a path it has no route for is exactly
	// "Not Found"; a handler that refuses a missing entity says what is
	// missing. The router's own stand-in for an absent optional route
	// ("this service does not support folder imports") is the same fact.
	if status == http.StatusNotFound &&
		(e.Detail == "Not Found" || strings.HasPrefix(e.Detail, "this service does not support")) {
		e.notSupported = true
	}
	return e
}

// parseReasons renders a 422 detail list one line per entry: a string as
// itself, a pydantic object as "loc: msg" with the leading "body"/"query"
// segment dropped and integer segments rendered as indexes, so a bad row
// reads "data.customers[0].email: Input should be a valid string".
func parseReasons(raw json.RawMessage) []string {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		// Neither a string nor a list: keep it legible rather than lose it.
		return []string{string(raw)}
	}
	reasons := make([]string, 0, len(items))
	for _, item := range items {
		var s string
		if json.Unmarshal(item, &s) == nil {
			reasons = append(reasons, s)
			continue
		}
		var obj struct {
			Msg string `json:"msg"`
			Loc []any  `json:"loc"`
		}
		if json.Unmarshal(item, &obj) == nil && (obj.Msg != "" || len(obj.Loc) > 0) {
			if loc := renderLoc(obj.Loc); loc != "" {
				reasons = append(reasons, loc+": "+obj.Msg)
			} else {
				reasons = append(reasons, obj.Msg)
			}
			continue
		}
		reasons = append(reasons, string(item))
	}
	return reasons
}

func renderLoc(loc []any) string {
	var b strings.Builder
	for i, part := range loc {
		switch v := part.(type) {
		case float64:
			fmt.Fprintf(&b, "[%d]", int(v))
		case string:
			if i == 0 && (v == "body" || v == "query" || v == "path") {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(v)
		default:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			fmt.Fprint(&b, v)
		}
	}
	return b.String()
}

// nonNil keeps a nil map from marshalling as null, which the twin would
// refuse as "provide exactly one of: data, profile".
func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
