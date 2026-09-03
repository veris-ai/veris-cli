package twin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// received is what the fake twin saw of the last request.
type received struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Body        string
}

// fakeTwin answers every request with one canned body and remembers what
// it was asked, so a test can pin the wire shape of each method.
func fakeTwin(t *testing.T, status int, body string) (*Client, *received) {
	t.Helper()
	got := &received{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*got = received{
			Method:      r.Method,
			Path:        r.URL.Path,
			Query:       r.URL.RawQuery,
			ContentType: r.Header.Get("Content-Type"),
			Body:        string(raw),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	// A trailing slash on the control URL must not double up.
	return New(srv.URL + "/s/sbx_1/stripe/"), got
}

// sameJSON compares two bodies as JSON so key order cannot fail a test.
func sameJSON(a, b string) bool {
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func ptr[T any](v T) *T { return &v }

func TestEveryRouteSendsWhatTheTwinExpects(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		call     func(c *Client) (any, error)
		method   string
		path     string
		query    string
		body     string // "" means no body at all
		answer   string
		want     any
		wantJSON string // for raw answers, compared as JSON
	}{
		{
			name:   "health",
			call:   func(c *Client) (any, error) { return c.Health(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/health",
			answer: `{"status":"ok","service":"stripe","schema":"sb1_stripe","state_version":7}`,
			want:   &Health{Status: "ok", Service: "stripe", Schema: "sb1_stripe", StateVersion: 7},
		},
		{
			name:   "health on the postgres twin has no schema or state version",
			call:   func(c *Client) (any, error) { return c.Health(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/health",
			answer: `{"service":"postgres","status":"ok"}`,
			want:   &Health{Status: "ok", Service: "postgres"},
		},
		{
			name:   "counts is the bare data read",
			call:   func(c *Client) (any, error) { return c.Counts(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/data",
			answer: `{"counts":{"customers":3,"clock":1,"client":1},"state_version":12}`,
			want:   &Counts{Counts: map[string]int{"customers": 3, "clock": 1, "client": 1}, StateVersion: 12},
		},
		{
			name:   "rows with limit and offset",
			call:   func(c *Client) (any, error) { return c.Rows(ctx, "customers", 2, 4) },
			method: "GET", path: "/s/sbx_1/stripe/veris/data",
			query:  "entity_type=customers&limit=2&offset=4",
			answer: `{"entity_type":"customers","rows":[{"id":"cus_9","email":"a@b"},{"id":"cus_8","email":null}],"total":9,"limit":2,"offset":4}`,
			want: &Rows{EntityType: "customers", Total: 9, Limit: 2, Offset: 4, Rows: []map[string]any{
				{"id": "cus_9", "email": "a@b"},
				{"id": "cus_8", "email": nil},
			}},
		},
		{
			name:   "rows with the twin's defaults sends only the entity type",
			call:   func(c *Client) (any, error) { return c.Rows(ctx, "clock", 0, 0) },
			method: "GET", path: "/s/sbx_1/stripe/veris/data",
			query:  "entity_type=clock",
			answer: `{"entity_type":"clock","rows":[{"id":1,"mode":"live","offset_seconds":0,"frozen_time":null}],"total":1,"limit":100,"offset":0}`,
			want: &Rows{EntityType: "clock", Total: 1, Limit: 100, Rows: []map[string]any{
				{"id": float64(1), "mode": "live", "offset_seconds": float64(0), "frozen_time": nil},
			}},
		},
		{
			name: "add",
			call: func(c *Client) (any, error) {
				return c.Add(ctx, map[string]any{"customers": []any{map[string]any{"id": "cus_1"}}})
			},
			method: "POST", path: "/s/sbx_1/stripe/veris/data",
			body:   `{"data":{"customers":[{"id":"cus_1"}]}}`,
			answer: `{"added":{"customers":1},"warnings":["faults[0]: never fires"]}`,
			want:   &Write{Added: map[string]int{"customers": 1}, Warnings: []string{"faults[0]: never fires"}},
		},
		{
			name:   "add with a nil map sends an empty object, not null",
			call:   func(c *Client) (any, error) { return c.Add(ctx, nil) },
			method: "POST", path: "/s/sbx_1/stripe/veris/data",
			body:   `{"data":{}}`,
			answer: `{"added":{}}`,
			want:   &Write{Added: map[string]int{}},
		},
		{
			name:   "add profile",
			call:   func(c *Client) (any, error) { return c.AddProfile(ctx, "busy") },
			method: "POST", path: "/s/sbx_1/stripe/veris/data",
			body:   `{"profile":"busy"}`,
			answer: `{"added":{"customers":40,"charges":120}}`,
			want:   &Write{Added: map[string]int{"customers": 40, "charges": 120}},
		},
		{
			name: "patch",
			call: func(c *Client) (any, error) {
				return c.Patch(ctx, map[string]any{"clock": []any{map[string]any{"mode": "frozen", "frozen_time": 1700000000}}})
			},
			method: "PATCH", path: "/s/sbx_1/stripe/veris/data",
			body:   `{"data":{"clock":[{"mode":"frozen","frozen_time":1700000000}]}}`,
			answer: `{"updated":{"clock":1},"warnings":["clock: moving time backwards"]}`,
			want:   &Write{Updated: map[string]int{"clock": 1}, Warnings: []string{"clock: moving time backwards"}},
		},
		{
			name: "delete",
			call: func(c *Client) (any, error) {
				return c.Delete(ctx, map[string]any{"customers": []any{map[string]any{"id": "cus_1"}}})
			},
			method: "DELETE", path: "/s/sbx_1/stripe/veris/data",
			body:   `{"data":{"customers":[{"id":"cus_1"}]}}`,
			answer: `{"deleted":{"customers":1}}`,
			want:   &Write{Deleted: map[string]int{"customers": 1}},
		},
		{
			name:   "reset with a profile",
			call:   func(c *Client) (any, error) { return c.Reset(ctx, ResetRequest{Profile: "default"}) },
			method: "POST", path: "/s/sbx_1/stripe/veris/reset",
			body:   `{"profile":"default"}`,
			answer: `{"reset":true,"seeded":{"customers":3}}`,
			want:   &Reset{Reset: true, Seeded: map[string]int{"customers": 3}},
		},
		{
			name: "reset with explicit rows",
			call: func(c *Client) (any, error) {
				return c.Reset(ctx, ResetRequest{Data: map[string]any{"customers": []any{}}})
			},
			method: "POST", path: "/s/sbx_1/stripe/veris/reset",
			body:   `{"data":{"customers":[]}}`,
			answer: `{"reset":true,"seeded":{}}`,
			want:   &Reset{Reset: true, Seeded: map[string]int{}},
		},
		{
			name:   "reset to an empty world sends an empty data object",
			call:   func(c *Client) (any, error) { return c.Reset(ctx, ResetRequest{Data: map[string]any{}}) },
			method: "POST", path: "/s/sbx_1/stripe/veris/reset",
			body:   `{"data":{}}`,
			answer: `{"reset":true,"seeded":{}}`,
			want:   &Reset{Reset: true, Seeded: map[string]int{}},
		},
		{
			name:   "reset to the boot profile sends an empty object",
			call:   func(c *Client) (any, error) { return c.Reset(ctx, ResetRequest{}) },
			method: "POST", path: "/s/sbx_1/stripe/veris/reset",
			body:   `{}`,
			answer: `{"reset":true,"seeded":{"customers":3}}`,
			want:   &Reset{Reset: true, Seeded: map[string]int{"customers": 3}},
		},
		{
			name:   "reset on the postgres twin answers ok",
			call:   func(c *Client) (any, error) { return c.Reset(ctx, ResetRequest{}) },
			method: "POST", path: "/s/sbx_1/stripe/veris/reset",
			body:   `{}`,
			answer: `{"ok":true}`,
			want:   &Reset{OK: true},
		},
		{
			name:   "schema is returned raw",
			call:   func(c *Client) (any, error) { return c.Schema(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/schema",
			answer:   `{"type":"object","properties":{"customers":{"type":"array","items":{"type":"object"}}}}`,
			wantJSON: `{"type":"object","properties":{"customers":{"type":"array","items":{"type":"object"}}}}`,
		},
		{
			name:   "manual is unwrapped",
			call:   func(c *Client) (any, error) { return c.Manual(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/manual",
			answer: `{"manual":"# stripe testing notes\n\nTest-side code talks to…"}`,
			want:   "# stripe testing notes\n\nTest-side code talks to…",
		},
		{
			name:   "operations on a REST twin",
			call:   func(c *Client) (any, error) { return c.Operations(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/operations",
			answer: `{"service":"stripe","total":2,"operations":[{"method":"GET","path":"/v1/customers"},{"method":"POST","path":"/v1/customers"}]}`,
			want: &Operations{Service: "stripe", Total: 2, Operations: []Operation{
				{Method: "GET", Path: "/v1/customers"}, {Method: "POST", Path: "/v1/customers"},
			}},
		},
		{
			name:   "operations on a GraphQL twin",
			call:   func(c *Client) (any, error) { return c.Operations(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/operations",
			answer: `{"service":"linear","total":2,"operations":[{"type":"query","field":"issue"},{"type":"mutation","field":"issueCreate"}]}`,
			want: &Operations{Service: "linear", Total: 2, Operations: []Operation{
				{Type: "query", Field: "issue"}, {Type: "mutation", Field: "issueCreate"},
			}},
		},
		{
			name:   "operations on an MCP twin",
			call:   func(c *Client) (any, error) { return c.Operations(ctx) },
			method: "GET", path: "/s/sbx_1/stripe/veris/operations",
			answer: `{"service":"mcp-surface-test","total":2,"mcp":{"path":"/mcp","tools":[{"tool":"get_widget"},{"tool":"list_widgets"}]}}`,
			want: &Operations{Service: "mcp-surface-test", Total: 2, MCP: &MCPSurface{
				Path: ptr("/mcp"), Tools: []MCPTool{{Tool: "get_widget"}, {Tool: "list_widgets"}},
			}},
		},
		{
			name: "requests with every filter",
			call: func(c *Client) (any, error) {
				return c.Requests(ctx, RequestsQuery{Limit: 10, Order: "asc", Tier: TierFault})
			},
			method: "GET", path: "/s/sbx_1/stripe/veris/requests",
			query:  "limit=10&order=asc&tier=fault",
			answer: `{"requests":[{"id":3,"ts":1700000000,"method":"POST","path":"/v1/charges","status":429,"tier":"fault","duration_ms":2,"state_version":5,"request_body":"{\"amount\":100}","response_body":"{\"error\":{}}","request_headers":"{\"authorization\":\"[REDACTED]\"}","response_headers":"{\"retry-after\":\"1\"}"}]}`,
			want: []Request{{
				ID: 3, TS: 1700000000, Method: "POST", Path: "/v1/charges", Status: ptr(429),
				Tier: "fault", DurationMS: 2, StateVersion: 5,
				RequestBody:     ptr(`{"amount":100}`),
				ResponseBody:    ptr(`{"error":{}}`),
				RequestHeaders:  ptr(`{"authorization":"[REDACTED]"}`),
				ResponseHeaders: ptr(`{"retry-after":"1"}`),
			}},
		},
		{
			name:   "requests with defaults sends no query; a hang has no status",
			call:   func(c *Client) (any, error) { return c.Requests(ctx, RequestsQuery{}) },
			method: "GET", path: "/s/sbx_1/stripe/veris/requests",
			answer: `{"requests":[{"id":1,"ts":1,"method":"GET","path":"/v1/x","status":null,"tier":"fault","duration_ms":30000,"state_version":0,"request_body":null,"response_body":null,"request_headers":"{}","response_headers":null}]}`,
			want: []Request{{
				ID: 1, TS: 1, Method: "GET", Path: "/v1/x", Tier: "fault", DurationMS: 30000,
				RequestHeaders: ptr("{}"),
			}},
		},
		{
			name:   "requests answers an empty list as no rows",
			call:   func(c *Client) (any, error) { return c.Requests(ctx, RequestsQuery{}) },
			method: "GET", path: "/s/sbx_1/stripe/veris/requests",
			answer: `{"requests":[]}`,
			want:   []Request{},
		},
		{
			name:   "probe",
			call:   func(c *Client) (any, error) { return c.Probe(ctx) },
			method: "POST", path: "/s/sbx_1/stripe/veris/client/probe",
			body:   `{}`,
			answer: `{"id":1,"default_base_url":"https://app.example","base_url_revision":2,"probe_state":"answered","probed_revision":2,"last_probe_at":1700000001,"last_probe_result":{"outcome":"http_response","status":200}}`,
			want: &Probe{
				ID: 1, DefaultBaseURL: ptr("https://app.example"), BaseURLRevision: 2,
				ProbeState: "answered", ProbedRevision: ptr(2), LastProbeAt: ptr(1700000001),
				LastProbeResult: map[string]any{"outcome": "http_response", "status": float64(200)},
			},
		},
		{
			name:   "probe with nothing registered",
			call:   func(c *Client) (any, error) { return c.Probe(ctx) },
			method: "POST", path: "/s/sbx_1/stripe/veris/client/probe",
			body:   `{}`,
			answer: `{"id":1,"default_base_url":null,"base_url_revision":0,"probe_state":"unknown","probed_revision":null,"last_probe_at":null,"last_probe_result":null}`,
			want:   &Probe{ID: 1, ProbeState: "unknown"},
		},
		{
			name:   "seed",
			call:   func(c *Client) (any, error) { return c.Seed(ctx, "CREATE TABLE t (id int);") },
			method: "POST", path: "/s/sbx_1/stripe/veris/seed",
			body:   `{"schema_sql":"CREATE TABLE t (id int);"}`,
			answer: `{"ok":true}`,
			want:   &Seed{OK: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, got := fakeTwin(t, http.StatusOK, tc.answer)
			out, err := tc.call(c)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.Method != tc.method {
				t.Errorf("method = %s, want %s", got.Method, tc.method)
			}
			if got.Path != tc.path {
				t.Errorf("path = %s, want %s", got.Path, tc.path)
			}
			if got.Query != tc.query {
				t.Errorf("query = %q, want %q", got.Query, tc.query)
			}
			switch {
			case tc.body == "":
				if got.Body != "" {
					t.Errorf("sent a body %q, want none", got.Body)
				}
				if got.ContentType != "" {
					t.Errorf("sent Content-Type %q with no body", got.ContentType)
				}
			default:
				if !sameJSON(got.Body, tc.body) {
					t.Errorf("body = %s, want %s", got.Body, tc.body)
				}
				if got.ContentType != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got.ContentType)
				}
			}
			if tc.wantJSON != "" {
				raw, ok := out.(json.RawMessage)
				if !ok {
					t.Fatalf("answer is %T, want json.RawMessage", out)
				}
				if !sameJSON(string(raw), tc.wantJSON) {
					t.Errorf("raw = %s, want %s", raw, tc.wantJSON)
				}
				return
			}
			if !reflect.DeepEqual(out, tc.want) {
				t.Errorf("answer = %#v\n want %#v", out, tc.want)
			}
		})
	}
}

// The postgres twin's schema is introspected, not modelled: {tables} keyed
// "schema.table", each with typed, nullable-flagged columns.
func TestAPostgresShapedSchemaDecodes(t *testing.T) {
	answer := `{"tables":{"public.users":{"columns":[{"name":"id","type":"integer","nullable":false},{"name":"email","type":"text","nullable":true}]},"app.orders":{"columns":[{"name":"id","type":"bigint","nullable":false}]}}}`
	c, _ := fakeTwin(t, http.StatusOK, answer)
	raw, err := c.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var schema PostgresSchema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := PostgresSchema{Tables: map[string]PostgresTable{
		"public.users": {Columns: []PostgresColumn{
			{Name: "id", Type: "integer", Nullable: false},
			{Name: "email", Type: "text", Nullable: true},
		}},
		"app.orders": {Columns: []PostgresColumn{{Name: "id", Type: "bigint"}}},
	}}
	if !reflect.DeepEqual(schema, want) {
		t.Errorf("schema = %#v\n want %#v", schema, want)
	}

	// An HTTP twin's JSON Schema has no "tables" key, which is how a caller
	// tells the two apart before choosing a renderer.
	c, _ = fakeTwin(t, http.StatusOK, `{"type":"object","properties":{"customers":{}}}`)
	raw, err = c.Schema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var http_ PostgresSchema
	if err := json.Unmarshal(raw, &http_); err != nil {
		t.Fatal(err)
	}
	if http_.Tables != nil {
		t.Errorf("an HTTP twin's schema decoded tables: %#v", http_.Tables)
	}
}

func TestManualMustBeAString(t *testing.T) {
	tests := []struct {
		name    string
		answer  string
		want    string
		wantErr string
	}{
		{name: "string", answer: `{"manual":"# notes"}`, want: "# notes"},
		{name: "empty string", answer: `{"manual":""}`, want: ""},
		{name: "not a string", answer: `{"manual":{"sections":[]}}`, wantErr: "the manual is not a string"},
		{name: "missing key", answer: `{}`, wantErr: "the manual is not a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := fakeTwin(t, http.StatusOK, tc.answer)
			got, err := c.Manual(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("manual = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestErrorShapes(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantDetail   string
		wantReasons  []string
		wantMessage  string
		notSupported bool
	}{
		{
			name:        "422 with a list of strings (SeedError)",
			status:      422,
			body:        `{"detail":["customers[0]: missing id","charges[2].amount: must be an integer"]}`,
			wantDetail:  "customers[0]: missing id; charges[2].amount: must be an integer",
			wantReasons: []string{"customers[0]: missing id", "charges[2].amount: must be an integer"},
			wantMessage: "[422] customers[0]: missing id; charges[2].amount: must be an integer",
		},
		{
			name:   "422 with pydantic objects",
			status: 422,
			body: `{"detail":[
				{"type":"value_error","loc":["body"],"msg":"Value error, provide exactly one of: data, profile","input":{}},
				{"type":"string_type","loc":["body","data","customers",0,"email"],"msg":"Input should be a valid string","input":3},
				{"type":"literal_error","loc":["query","surface"],"msg":"Input should be 'rest', 'graphql' or 'mcp'","input":"grpc"}
			]}`,
			wantDetail: "Value error, provide exactly one of: data, profile; " +
				"data.customers[0].email: Input should be a valid string; " +
				"surface: Input should be 'rest', 'graphql' or 'mcp'",
			wantReasons: []string{
				"Value error, provide exactly one of: data, profile",
				"data.customers[0].email: Input should be a valid string",
				"surface: Input should be 'rest', 'graphql' or 'mcp'",
			},
			wantMessage: "[422] Value error, provide exactly one of: data, profile; " +
				"data.customers[0].email: Input should be a valid string; " +
				"surface: Input should be 'rest', 'graphql' or 'mcp'",
		},
		{
			name:        "422 with a bare string",
			status:      422,
			body:        `{"detail":"environment blob limit exceeded"}`,
			wantDetail:  "environment blob limit exceeded",
			wantMessage: "[422] environment blob limit exceeded",
		},
		{
			name:        "404 naming a missing entity type is a plain error",
			status:      404,
			body:        `{"detail":"unknown entity type 'cutsomers'; valid: ['charges', 'customers']"}`,
			wantDetail:  "unknown entity type 'cutsomers'; valid: ['charges', 'customers']",
			wantMessage: "[404] unknown entity type 'cutsomers'; valid: ['charges', 'customers']",
		},
		{
			name:         "404 from the framework means no such route",
			status:       404,
			body:         `{"detail":"Not Found"}`,
			wantDetail:   "Not Found",
			wantMessage:  "[404] Not Found",
			notSupported: true,
		},
		{
			name:         "404 from the router's stand-in for an absent route",
			status:       404,
			body:         `{"detail":"this service does not support folder imports"}`,
			wantDetail:   "this service does not support folder imports",
			wantMessage:  "[404] this service does not support folder imports",
			notSupported: true,
		},
		{
			name:        "400 with a string",
			status:      400,
			body:        `{"detail":"seed needs schema_sql or sql_file"}`,
			wantDetail:  "seed needs schema_sql or sql_file",
			wantMessage: "[400] seed needs schema_sql or sql_file",
		},
		{
			name:        "non-JSON body is kept, trimmed",
			status:      502,
			body:        "<html><body>bad gateway</body></html>\n",
			wantDetail:  "<html><body>bad gateway</body></html>",
			wantMessage: "[502] <html><body>bad gateway</body></html>",
		},
		{
			name:        "non-JSON body is capped at 200 bytes",
			status:      500,
			body:        strings.Repeat("x", 500),
			wantDetail:  strings.Repeat("x", 200),
			wantMessage: "[500] " + strings.Repeat("x", 200),
		},
		{
			name:        "empty body falls back to the status text",
			status:      503,
			body:        "",
			wantDetail:  "Service Unavailable",
			wantMessage: "[503] Service Unavailable",
		},
		{
			name:        "JSON without a detail key is kept as text",
			status:      409,
			body:        `{"error":"capture fence held"}`,
			wantDetail:  `{"error":"capture fence held"}`,
			wantMessage: `[409] {"error":"capture fence held"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := fakeTwin(t, tc.status, tc.body)
			_, err := c.Counts(context.Background())
			var te *Error
			if !errors.As(err, &te) {
				t.Fatalf("err = %v (%T), want *Error", err, err)
			}
			if te.Status != tc.status {
				t.Errorf("Status = %d, want %d", te.Status, tc.status)
			}
			if te.Method != "GET" || te.Path != "/veris/data" {
				t.Errorf("Method/Path = %s %s, want GET /veris/data", te.Method, te.Path)
			}
			if te.Detail != tc.wantDetail {
				t.Errorf("Detail = %q, want %q", te.Detail, tc.wantDetail)
			}
			if !reflect.DeepEqual(te.Reasons, tc.wantReasons) {
				t.Errorf("Reasons = %q, want %q", te.Reasons, tc.wantReasons)
			}
			if err.Error() != tc.wantMessage {
				t.Errorf("Error() = %q, want %q", err.Error(), tc.wantMessage)
			}
			if errors.Is(err, ErrNotSupported) != tc.notSupported {
				t.Errorf("errors.Is(ErrNotSupported) = %v, want %v", !tc.notSupported, tc.notSupported)
			}
		})
	}
}

// The error's path carries the query, so a refused page read says which
// table it asked for.
func TestErrorPathCarriesTheQuery(t *testing.T) {
	c, _ := fakeTwin(t, http.StatusNotFound, `{"detail":"unknown entity type 'x'; valid: []"}`)
	_, err := c.Rows(context.Background(), "x", 5, 0)
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if te.Path != "/veris/data?entity_type=x&limit=5" {
		t.Errorf("Path = %q", te.Path)
	}
	if errors.Is(err, ErrNotSupported) {
		t.Error("a missing entity type must not read as an unsupported route")
	}
}

// A twin that publishes no operation list has no route at all, so any 404
// there is ErrNotSupported -- whatever the body says.
func TestOperationsMapsEvery404ToNotSupported(t *testing.T) {
	for _, body := range []string{`{"detail":"Not Found"}`, `{"detail":"gone"}`, ``, `not json`} {
		t.Run(body, func(t *testing.T) {
			c, _ := fakeTwin(t, http.StatusNotFound, body)
			_, err := c.Operations(context.Background())
			if !errors.Is(err, ErrNotSupported) {
				t.Fatalf("err = %v, want ErrNotSupported", err)
			}
			var te *Error
			if !errors.As(err, &te) || te.Status != 404 {
				t.Errorf("err = %v, want a *Error keeping status 404", err)
			}
		})
	}
	// Other statuses stay what they are.
	c, _ := fakeTwin(t, http.StatusInternalServerError, `{"detail":"boom"}`)
	_, err := c.Operations(context.Background())
	if errors.Is(err, ErrNotSupported) {
		t.Errorf("a 500 must not read as unsupported: %v", err)
	}
}

// The postgres twin serves health, schema, reset and seed and nothing else.
// Every other method there meets the framework's 404 and reports it as
// ErrNotSupported.
func TestPostgresGapsAreNotSupported(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(c *Client) error{
		"counts":     func(c *Client) error { _, err := c.Counts(ctx); return err },
		"rows":       func(c *Client) error { _, err := c.Rows(ctx, "users", 0, 0); return err },
		"add":        func(c *Client) error { _, err := c.Add(ctx, map[string]any{}); return err },
		"addProfile": func(c *Client) error { _, err := c.AddProfile(ctx, "x"); return err },
		"patch":      func(c *Client) error { _, err := c.Patch(ctx, map[string]any{}); return err },
		"delete":     func(c *Client) error { _, err := c.Delete(ctx, map[string]any{}); return err },
		"manual":     func(c *Client) error { _, err := c.Manual(ctx); return err },
		"requests":   func(c *Client) error { _, err := c.Requests(ctx, RequestsQuery{}); return err },
		"probe":      func(c *Client) error { _, err := c.Probe(ctx); return err },
		"seed":       func(c *Client) error { _, err := c.Seed(ctx, "select 1"); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			c, _ := fakeTwin(t, http.StatusNotFound, `{"detail":"Not Found"}`)
			if err := call(c); !errors.Is(err, ErrNotSupported) {
				t.Errorf("err = %v, want ErrNotSupported", err)
			}
		})
	}
}

// An HTTP twin has no /veris/seed, and its catch-all answers the path in
// the vendor's own 404 shape -- Stripe's error object, the generic
// {"error":{…}}, Adobe's HTML page -- never FastAPI's {"detail":"Not
// Found"}. Seed and Operations must read every one of those as the route
// being absent.
func TestVendorShaped404sAreNotSupportedOnOptionalRoutes(t *testing.T) {
	ctx := context.Background()
	bodies := map[string]string{
		"stripe":  `{"error":{"type":"invalid_request_error","message":"Unrecognized request URL (POST: /veris/seed). If you are trying to list objects, remove the trailing slash."}}`,
		"generic": `{"error":{"message":"Not Found","code":"not_found"}}`,
		"html":    `<!DOCTYPE html><html><head><title>404</title></head><body>Not Found</body></html>`,
		"empty":   ``,
	}
	calls := map[string]func(c *Client) error{
		"seed":       func(c *Client) error { _, err := c.Seed(ctx, "select 1"); return err },
		"operations": func(c *Client) error { _, err := c.Operations(ctx); return err },
	}
	for callName, call := range calls {
		for bodyName, body := range bodies {
			t.Run(callName+"/"+bodyName, func(t *testing.T) {
				c, _ := fakeTwin(t, http.StatusNotFound, body)
				err := call(c)
				if !errors.Is(err, ErrNotSupported) {
					t.Fatalf("err = %v, want ErrNotSupported", err)
				}
				var te *Error
				if !errors.As(err, &te) || te.Status != 404 {
					t.Errorf("err = %v, want a *Error keeping status 404", err)
				}
			})
		}
	}
}

// On a route every twin serves, a 404 without FastAPI's envelope is not the
// route being absent: an empty body is a dead URL somewhere in the path (a
// proxy's bare 404), and a vendor-shaped body is a handler's own refusal.
// Only the framework's {"detail":"Not Found"} means "no such route".
func TestBare404OnACommonRouteIsNotNotSupported(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"empty":  ``,
		"vendor": `{"error":{"message":"Not Found","code":"not_found"}}`,
		"text":   `Not Found`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := fakeTwin(t, http.StatusNotFound, body)
			_, err := c.Counts(ctx)
			if errors.Is(err, ErrNotSupported) {
				t.Errorf("a 404 with body %q read as an unsupported route: %v", body, err)
			}
			var te *Error
			if !errors.As(err, &te) || te.Status != 404 {
				t.Errorf("err = %v, want a plain *Error with status 404", err)
			}
		})
	}
}

// The postgres twin's reset takes no body and answers {ok}; a reset that
// asked for a profile or rows was not honoured and must not read as done.
func TestResetWithPayloadOnPostgresIsAnError(t *testing.T) {
	ctx := context.Background()
	for name, req := range map[string]ResetRequest{
		"profile": {Profile: "busy"},
		"data":    {Data: map[string]any{"users": []any{}}},
		"empty":   {Data: map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := fakeTwin(t, http.StatusOK, `{"ok":true}`)
			_, err := c.Reset(ctx, req)
			if err == nil || !strings.Contains(err.Error(), "takes no profile or data") {
				t.Errorf("err = %v, want the postgres refusal", err)
			}
		})
	}
	// Without a payload {ok} is the postgres twin's success, and an HTTP
	// twin's {reset} with a payload is its own.
	c, _ := fakeTwin(t, http.StatusOK, `{"ok":true}`)
	if r, err := c.Reset(ctx, ResetRequest{}); err != nil || !r.OK {
		t.Errorf("bare reset on postgres = %+v, %v", r, err)
	}
	c, _ = fakeTwin(t, http.StatusOK, `{"reset":true,"seeded":{}}`)
	if r, err := c.Reset(ctx, ResetRequest{Profile: "busy"}); err != nil || !r.Reset {
		t.Errorf("profile reset on an HTTP twin = %+v, %v", r, err)
	}
}

// A Client built as a literal with no HTTP client may be shared; do must
// not write to it.
func TestZeroHTTPClientIsSafeToShare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok","service":"x"}`)
	}))
	t.Cleanup(srv.Close)
	c := &Client{ControlURL: srv.URL}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Health(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if c.HTTP != nil {
		t.Error("do assigned c.HTTP; a shared literal Client would race on that write")
	}
}

func TestClientSideValidation(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(c *Client) error
		want string
	}{
		{
			name: "rows needs an entity type",
			call: func(c *Client) error { _, err := c.Rows(ctx, "", 0, 0); return err },
			want: "entity type is required",
		},
		{
			name: "add profile needs a name",
			call: func(c *Client) error { _, err := c.AddProfile(ctx, ""); return err },
			want: "profile name is required",
		},
		{
			name: "reset refuses profile and data together",
			call: func(c *Client) error {
				_, err := c.Reset(ctx, ResetRequest{Profile: "p", Data: map[string]any{}})
				return err
			},
			want: "not both",
		},
		{
			name: "requests refuses an unknown order",
			call: func(c *Client) error { _, err := c.Requests(ctx, RequestsQuery{Order: "newest"}); return err },
			want: `order "newest" is not asc or desc`,
		},
		{
			name: "requests refuses a tier clients do not know",
			call: func(c *Client) error { _, err := c.Requests(ctx, RequestsQuery{Tier: "fallback-llm"}); return err },
			want: `tier "fallback-llm" is not one of handler, fault, control, delivery`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, got := fakeTwin(t, http.StatusOK, `{}`)
			err := tc.call(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if got.Method != "" {
				t.Errorf("a refused call still reached the twin: %s %s", got.Method, got.Path)
			}
		})
	}
}

func TestEveryTierConstantIsAccepted(t *testing.T) {
	for _, tier := range []string{TierHandler, TierFault, TierControl, TierDelivery} {
		c, got := fakeTwin(t, http.StatusOK, `{"requests":[]}`)
		if _, err := c.Requests(context.Background(), RequestsQuery{Tier: tier}); err != nil {
			t.Errorf("tier %s: %v", tier, err)
		}
		if got.Query != "tier="+tier {
			t.Errorf("tier %s sent query %q", tier, got.Query)
		}
	}
}

func TestAnUnreachableTwinNamesItself(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c := New(srv.URL)
	_, err := c.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot reach the twin at "+srv.URL) {
		t.Errorf("err = %v", err)
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	c, _ := fakeTwin(t, http.StatusOK, `{"status":"ok"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Health(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestAMalformedAnswerIsAnError(t *testing.T) {
	c, _ := fakeTwin(t, http.StatusOK, `{"status": "ok"`)
	_, err := c.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not the expected JSON") {
		t.Errorf("err = %v", err)
	}
}

func TestNewTrimsTheTrailingSlash(t *testing.T) {
	if got := New("https://gw.example/s/sbx/stripe/").ControlURL; got != "https://gw.example/s/sbx/stripe" {
		t.Errorf("ControlURL = %q", got)
	}
	if New("x").HTTP == nil {
		t.Error("New left HTTP nil")
	}
}
