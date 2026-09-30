package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
)

// stripeSchemaJSON is an HTTP twin's GET /veris/schema as the router
// builds it: one array property per table in model order, columns in
// column order, the world singletons documented beside them. It is a
// literal so the order the tests assert is the order the twin sends.
const stripeSchemaJSON = `{
  "type": "object",
  "properties": {
    "customers": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "description": "Stripe customer id, cus_…"},
          "email": {"anyOf": [{"type": "string"}, {"type": "null"}], "description": "Customer email"},
          "name": {"anyOf": [{"type": "string"}, {"type": "null"}]},
          "created": {"type": "integer", "description": "Unix seconds on the sandbox clock"},
          "metadata": {},
          "balance": {"type": "integer"}
        },
        "required": ["id"],
        "additionalProperties": false
      },
      "description": "Customers of the account"
    },
    "payment_methods": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "customer": {"type": "string"},
          "type": {"type": "string", "enum": ["card", "us_bank_account"]},
          "card": {"type": "object", "properties": {"brand": {"type": "string"}, "last4": {"type": "string"}}}
        },
        "required": ["id", "customer"],
        "additionalProperties": false
      }
    },
    "webhook_endpoints": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "url": {"type": "string"},
          "enabled_events": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["id", "url"],
        "additionalProperties": false
      }
    },
    "wide": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "column_number_01": {"type": "string"}, "column_number_02": {"type": "string"},
          "column_number_03": {"type": "string"}, "column_number_04": {"type": "string"},
          "column_number_05": {"type": "string"}, "column_number_06": {"type": "string"},
          "column_number_07": {"type": "string"}, "column_number_08": {"type": "string"}
        },
        "required": [],
        "additionalProperties": false
      }
    },
    "faults": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"id": {"type": "integer"}, "path": {"type": "string"}},
        "required": ["path"],
        "additionalProperties": false
      }
    },
    "auth": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"id": {"type": "integer", "description": "Singleton: always 1."}, "mode": {"type": "string"}},
        "required": [],
        "additionalProperties": false
      },
      "description": "Whether this service checks credential VALUES."
    },
    "clock": {
      "type": "array",
      "items": {"type": "object", "properties": {"mode": {"type": "string"}}, "required": [], "additionalProperties": false},
      "description": "Sandbox-level singleton (shared by all services); read and PATCH only — cannot be added or deleted."
    },
    "client": {
      "type": "array",
      "items": {"type": "object", "properties": {"default_base_url": {"type": "string"}}, "required": [], "additionalProperties": false},
      "description": "Sandbox-level singleton (shared by all services); read and PATCH only — cannot be added or deleted."
    }
  },
  "additionalProperties": false,
  "description": "Seed data keyed by entity type (table name)."
}`

// pgSchemaJSON is the postgres twin's introspected schema.
const pgSchemaJSON = `{"tables": {"public.users": {"columns": [
  {"name": "id", "type": "integer", "nullable": false},
  {"name": "email", "type": "text", "nullable": true}
]}}}`

// undeletable is router.py's remove_data refusals, word for word: the
// tables a twin will not delete, each naming what to do instead.
var undeletable = map[string]string{
	"clock":             "clock: the clock is a singleton and cannot be deleted; reset it with PATCH (mode=live, offset_seconds=0)",
	"client":            "client: the client registration is a singleton and cannot be deleted; PATCH default_base_url to null to unregister",
	"auth":              "auth: the auth mode is a singleton and cannot be deleted; PATCH mode to 'permissive' to stop checking credential values",
	"delivery_attempts": "delivery_attempts: the attempt log is append-only and cannot be deleted",
}

const stripeManual = "# stripe\n\nCredentials  Any sk_test_ key works.\n\n```\ncurl -X POST $STRIPE_API_BASE/v1/customers\n```\n\n## Faults\nA faults row with error.status 402 makes the next matching request fail.\n"

// dataTwins serves /veris/* for the HTTP twins (stripe, and zendesk when a
// test wants a second one, sharing one world) and for a postgres twin
// under /s/<sandbox>/<twin>. Tests script answers through script() and
// read what was asked through the getters; everything is behind the mutex
// because the handlers run on the server's goroutines.
type dataTwins struct {
	srv *httptest.Server
	mu  sync.Mutex

	counts     map[string]int                                    // the HTTP twins' GET /veris/data counts, singletons included
	countReads int                                               // how many bare GET /veris/data the HTTP twins answered
	countDelay time.Duration                                     // how long each bare GET /veris/data takes to answer
	rowsDelay  time.Duration                                     // how long each GET /veris/data?entity_type=… takes to answer
	addDelay   time.Duration                                     // how long each POST /veris/data takes to answer
	version    int                                               // their state_version
	health     int                                               // the HTTP twins' GET /veris/health status (0 → 200)
	rows       map[string][]map[string]any                       // rows per table, newest first
	addStatus  int                                               // POST /veris/data status (0 → 200)
	adds       []string                                          // twins that received a POST /veris/data, in order
	edits      []dataEdit                                        // every PATCH and DELETE /veris/data, in order
	queries    []string                                          // raw query of every GET /veris/data with an entity_type
	seeds      []string                                          // schema_sql of every POST /veris/seed
	pgData404  bool                                              // postgres GET /veris/data is FastAPI's 404 rather than the singletons
	resets     []twinCall                                        // every POST /veris/reset, in order
	resetReply func(twin string, body map[string]any) (int, any) // nil → a plain success
	opsCalls   []twinCall                                        // every GET /veris/operations, in order
}

// twinCall is one control request as the twin received it: which twin, the
// X-API-Key it carried, its raw query and its decoded body.
type twinCall struct {
	twin  string
	key   string
	query string
	body  map[string]any
	raw   string // the body exactly as sent
}

// dataEdit is one PATCH or DELETE of /veris/data as the twin received it.
type dataEdit struct {
	twin   string
	method string
	data   map[string]any
}

func newDataTwins(t *testing.T) *dataTwins {
	t.Helper()
	f := &dataTwins{
		counts:  map[string]int{"customers": 41, "payment_methods": 13, "faults": 0, "auth": 1, "clock": 1, "client": 1},
		version: 3,
		rows: map[string][]map[string]any{
			"customers": {
				{"id": "cus_2", "email": "bob@example.com", "name": nil, "created": 1700000000, "metadata": map[string]any{"tier": "gold"}, "balance": 0},
				{"id": "cus_1", "email": "ada@example.com", "name": "Ada", "created": 1699999999, "metadata": map[string]any{}, "balance": -5},
			},
			"faults": {},
		},
	}
	mux := http.NewServeMux()
	prefix := "/s/" + sbID + "/"
	mux.HandleFunc(prefix+"{twin}/veris/health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PathValue("twin") == "postgres" {
			sbJSON(w, 200, map[string]any{"service": "postgres", "status": "ok"})
			return
		}
		if f.health != 0 {
			sbJSON(w, f.health, map[string]string{"detail": "the world is rebuilding"})
			return
		}
		sbJSON(w, 200, map[string]any{"status": "ok", "service": r.PathValue("twin"), "state_version": f.version})
	})
	mux.HandleFunc(prefix+"{twin}/veris/schema", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("twin") == "postgres" {
			_, _ = w.Write([]byte(pgSchemaJSON))
			return
		}
		_, _ = w.Write([]byte(stripeSchemaJSON))
	})
	mux.HandleFunc(prefix+"{twin}/veris/manual", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("twin") == "postgres" {
			sbJSON(w, 404, map[string]string{"detail": "Not Found"})
			return
		}
		sbJSON(w, 200, map[string]any{"manual": stripeManual})
	})
	mux.HandleFunc(prefix+"{twin}/veris/data", func(w http.ResponseWriter, r *http.Request) {
		// A slow count or add sleeps before taking the lock, so the other
		// requests around it are answered meanwhile, as a twin would.
		f.mu.Lock()
		delay := time.Duration(0)
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("entity_type") == "":
			delay = f.countDelay
		case r.Method == http.MethodGet:
			delay = f.rowsDelay
		case r.Method == http.MethodPost:
			delay = f.addDelay
		}
		f.mu.Unlock()
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.PathValue("twin") == "postgres" {
			if f.pgData404 || r.Method != http.MethodGet {
				sbJSON(w, 404, map[string]string{"detail": "Not Found"})
				return
			}
			sbJSON(w, 200, map[string]any{"counts": map[string]int{"clock": 1, "client": 1}, "state_version": 0})
			return
		}
		switch r.Method {
		case http.MethodGet:
			entity := r.URL.Query().Get("entity_type")
			if entity == "" {
				f.countReads++
				sbJSON(w, 200, map[string]any{"counts": f.counts, "state_version": f.version})
				return
			}
			f.queries = append(f.queries, r.URL.RawQuery)
			rows, ok := f.rows[entity]
			if !ok {
				sbJSON(w, 404, map[string]string{"detail": "unknown entity type '" + entity + "'; valid: ['customers', 'faults']"})
				return
			}
			sbJSON(w, 200, map[string]any{"entity_type": entity, "rows": rows, "total": len(rows) + 39, "limit": 20, "offset": 0})
		case http.MethodPost:
			f.adds = append(f.adds, r.PathValue("twin"))
			var body struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if f.addStatus == 422 {
				sbJSON(w, 422, map[string]any{"detail": []string{"customers[0].email: must be a string", "unknown table 'customer'"}})
				return
			}
			added := map[string]int{}
			for table, rows := range body.Data {
				if list, ok := rows.([]any); ok {
					added[table] = len(list)
					f.counts[table] += len(list)
				}
			}
			f.version++
			sbJSON(w, 200, map[string]any{"added": added, "warnings": []string{}, "state_version": f.version})
		case http.MethodPatch, http.MethodDelete:
			var body struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.edits = append(f.edits, dataEdit{twin: r.PathValue("twin"), method: r.Method, data: body.Data})
			// router.py refuses to delete the singletons, each with the
			// message that says what to do instead; the CLI must carry
			// those through untouched.
			if r.Method == http.MethodDelete {
				for table, refusal := range undeletable {
					if _, ok := body.Data[table]; ok {
						sbJSON(w, 422, map[string]any{"detail": []string{refusal}})
						return
					}
				}
			}
			counts := map[string]int{}
			for table, rows := range body.Data {
				list, ok := rows.([]any)
				if !ok {
					continue
				}
				counts[table] = len(list)
				if r.Method == http.MethodDelete {
					f.counts[table] -= len(list)
				}
			}
			f.version++
			key := "updated"
			if r.Method == http.MethodDelete {
				key = "deleted"
			}
			sbJSON(w, 200, map[string]any{key: counts})
		default:
			sbJSON(w, 405, map[string]string{"detail": "Method Not Allowed"})
		}
	})
	mux.HandleFunc("POST "+prefix+"{twin}/veris/seed", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body struct {
			SchemaSQL string `json:"schema_sql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.seeds = append(f.seeds, body.SchemaSQL)
		sbJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST "+prefix+"{twin}/veris/reset", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		name := r.PathValue("twin")
		f.resets = append(f.resets, twinCall{twin: name, key: r.Header.Get("X-API-Key"), body: body, raw: string(raw)})
		if f.resetReply != nil {
			status, answer := f.resetReply(name, body)
			sbJSON(w, status, answer)
			return
		}
		if name == "postgres" {
			sbJSON(w, 200, map[string]any{"ok": true})
			return
		}
		seeded := map[string]int{"customers": 3, "prices": 12}
		if data, ok := body["data"].(map[string]any); ok {
			seeded = map[string]int{}
			for table, rows := range data {
				if list, ok := rows.([]any); ok {
					seeded[table] = len(list)
				}
			}
		}
		sbJSON(w, 200, map[string]any{"reset": true, "seeded": seeded})
	})
	mux.HandleFunc("GET "+prefix+"{twin}/veris/operations", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("twin")
		f.opsCalls = append(f.opsCalls, twinCall{twin: name, key: r.Header.Get("X-API-Key"), query: r.URL.RawQuery})
		surface := r.URL.Query().Get("surface")
		switch name {
		case "postgres":
			sbJSON(w, 404, map[string]string{"detail": "Not Found"})
		case "linear":
			sbJSON(w, 200, map[string]any{"service": "linear", "total": 2, "operations": []map[string]string{
				{"type": "query", "field": "issue"}, {"type": "mutation", "field": "issueCreate"}}})
		default:
			body := map[string]any{"service": name}
			total := 0
			if surface == "" || surface == "rest" {
				body["operations"] = []map[string]string{
					{"method": "GET", "path": "/v1/customers"},
					{"method": "POST", "path": "/v1/customers"},
					{"method": "GET", "path": "/v1/customers/{customer}"}}
				total += 3
			}
			if surface == "" || surface == "mcp" {
				body["mcp"] = map[string]any{"path": "/mcp", "auth": "bearer", "sign_in": "oauth",
					"tools": []map[string]string{{"tool": "get_widget"}, {"tool": "list_widgets"}}}
				total += 2
			}
			body["total"] = total
			sbJSON(w, 200, body)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		sbJSON(w, 404, map[string]string{"detail": "Not Found"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *dataTwins) script(fn func(f *dataTwins)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *dataTwins) countsRead() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.countReads
}

func (f *dataTwins) addedTo() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.adds...)
}

func (f *dataTwins) edited() []dataEdit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dataEdit(nil), f.edits...)
}

func (f *dataTwins) rowQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func (f *dataTwins) resetCalls() []twinCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]twinCall(nil), f.resets...)
}

func (f *dataTwins) operationsCalls() []twinCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]twinCall(nil), f.opsCalls...)
}

func (f *dataTwins) seeded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seeds...)
}

func (f *dataTwins) control(twin string) string {
	return f.srv.URL + "/s/" + sbID + "/" + twin
}

// services is the sandbox's service list: stripe proxied through the
// gateway, postgres a data-plane DSN with a control URL of its own, and
// with extra, the named HTTP twins after them.
func (f *dataTwins) services(extra ...string) []api.ServiceInfo {
	out := []api.ServiceInfo{
		{Name: "stripe", Status: "ready", URL: f.control("stripe"), ControlURL: f.control("stripe"), EnvHint: "STRIPE_API_BASE"},
		{Name: "postgres", Status: "ready", URL: "postgresql://app:app@10.0.0.5:5432/sb?sslmode=require", ControlURL: f.control("postgres"), EnvHint: "DATABASE_URL"},
	}
	for _, name := range extra {
		out = append(out, api.ServiceInfo{Name: name, Status: "ready", URL: f.control(name), ControlURL: f.control(name), EnvHint: strings.ToUpper(name) + "_API_BASE"})
	}
	return out
}

// dataBench is a logged-in bench whose folder points at sbID, ready in ci
// with the twins' services, expiring in three hours.
func dataBench(t *testing.T, plane *sandboxPlane, services []api.ServiceInfo) (*bench, time.Time) {
	t.Helper()
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	expires := time.Now().Add(3*time.Hour + 20*time.Second)
	plane.script(func(p *sandboxPlane) {
		p.answer = func(int) *api.Sandbox { return readySandbox(services, expires) }
	})
	return b, expires
}

func TestSandboxServicesList(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	dataBench(t, plane, twins.services())

	t.Run("the table, singletons hidden, data plane named", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "list")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"Sandbox "+sbID+" · boot bundle · expires in 3h 0m\n",
			"  Twin      Status  Env hint         Rows\n",
			"  stripe    ready   STRIPE_API_BASE  customers 41 · faults 0 · payment_methods 13\n",
			"  postgres  ready   DATABASE_URL     (data plane; schema from your SQL)\n",
			"→ veris sandbox services get stripe   (URL, control URL, every table)\n")
		if strings.Contains(stderr, "clock") || strings.Contains(stderr, "client") || strings.Contains(stderr, "auth") {
			t.Errorf("the singletons must be hidden:\n%s", stderr)
		}
	})

	t.Run("a data-plane twin with no rows route", func(t *testing.T) {
		twins.script(func(f *dataTwins) { f.pgData404 = true })
		defer twins.script(func(f *dataTwins) { f.pgData404 = false })
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "list")
		if code != 0 || !strings.Contains(stderr, "  postgres  ready   DATABASE_URL     (data plane; schema from your SQL)\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("--json carries the counts without the singletons", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "list", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		var rows []struct {
			Name         string         `json:"name"`
			ControlURL   string         `json:"control_url"`
			Tables       map[string]int `json:"tables"`
			StateVersion int            `json:"state_version"`
		}
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil || len(rows) != 2 {
			t.Fatalf("stdout %q: %v", stdout, err)
		}
		if rows[0].Name != "stripe" || rows[0].Tables["customers"] != 41 || rows[0].StateVersion != 3 {
			t.Errorf("stripe row = %+v", rows[0])
		}
		if _, ok := rows[0].Tables["clock"]; ok {
			t.Errorf("clock must be hidden under --json too: %v", rows[0].Tables)
		}
		if _, ok := rows[0].Tables["auth"]; ok || len(rows[0].Tables) != 3 {
			t.Errorf("auth must be hidden under --json too: %v", rows[0].Tables)
		}
		if rows[1].Name != "postgres" || rows[1].Tables != nil {
			t.Errorf("postgres row = %+v, want null tables", rows[1])
		}
		if strings.Contains(stderr, "Sandbox "+sbID) {
			t.Errorf("--json must not print the header:\n%s", stderr)
		}
	})

	t.Run("a twin that does not answer is a dash and a warning", func(t *testing.T) {
		services := twins.services()
		services[0].ControlURL = "http://127.0.0.1:1"
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(services, time.Now().Add(time.Hour)) }
		})
		defer plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(), time.Now().Add(time.Hour)) }
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "list")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "  stripe    ready   STRIPE_API_BASE  —\n", "! stripe did not answer: ")
	})

	t.Run("--id names another sandbox", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "list", "--id", otherSbID)
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read sandbox "+otherSbID+": [404]") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestSandboxServicesGet(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	dataBench(t, plane, twins.services())
	stripe := twins.control("stripe")

	t.Run("an HTTP twin", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "get", "stripe")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"stripe · ready\n",
			"  URL:          "+stripe+"\n",
			"  Control URL:  "+stripe+"\n",
			"  Env hint:     STRIPE_API_BASE\n",
			"  Tables (state_version 3):\n",
			"    customers        41\n",
			"    faults           0\n",
			"    payment_methods  13\n",
			"→ veris sandbox data get stripe TABLE   (a page of rows)\n",
			"→ veris sandbox services manual stripe   (the twin's testing notes)\n")
		if strings.Contains(stderr, "clock") || strings.Contains(stderr, "auth") {
			t.Errorf("the singletons must be hidden:\n%s", stderr)
		}
	})

	t.Run("a data-plane twin", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get", "postgres")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"postgres · ready\n",
			"  URL:          postgresql://app:app@10.0.0.5:5432/sb?sslmode=require\n",
			"  Control URL:  "+twins.control("postgres")+"\n",
			"  Env hint:     DATABASE_URL\n",
			"  Tables:       (data plane; schema from your SQL)\n")
	})

	t.Run("--json", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "get", "stripe", "--json")
		var row struct {
			Name   string         `json:"name"`
			Tables map[string]int `json:"tables"`
		}
		if code != 0 || json.Unmarshal([]byte(stdout), &row) != nil || row.Name != "stripe" || row.Tables["payment_methods"] != 13 {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
	})

	t.Run("a twin the sandbox does not have", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get", "shopify")
		if code != 1 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"✗ No twin named 'shopify' in sandbox "+sbID+" (have: stripe, postgres)\n",
			"→ Next: veris sandbox services list\n")
	})

	// Off a terminal, a verb with no NAME is told which twins the sandbox
	// has -- as the commands themselves, so the fix can be run rather than
	// read and retyped.
	t.Run("no NAME lists the twins as commands", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get")
		if code != 1 {
			t.Errorf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"\u2717 sandbox services get needs the twin's name. Sandbox "+sbID+" has 2:\n",
			"\u2192 Next: veris sandbox services get stripe\n",
			"\u2192 Next: veris sandbox services get postgres\n")
	})

	// The --id that named the sandbox rides along, so a printed line means
	// the same sandbox when it is run.
	t.Run("no NAME keeps the --id it was given", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get", "--id", sbID)
		if code != 1 {
			t.Errorf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "\u2192 Next: veris sandbox services get stripe --id "+sbID+"\n")
	})

	// A near miss is one line away from right, so the corrected command is
	// what the refusal offers.
	t.Run("a near miss is corrected", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get", "STRIPE")
		if code != 1 {
			t.Errorf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "\u2192 Next: veris sandbox services get stripe\n")
	})

	// A sandbox with one twin has already answered "which twin?", so it is
	// not asked -- but the answer is said out loud, since the reader never
	// typed it.
	t.Run("a sandbox with one twin needs no NAME", func(t *testing.T) {
		only := newSandboxPlane(t)
		twins := newDataTwins(t)
		dataBench(t, only, twins.services()[:1])
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "sandbox services get of stripe, the sandbox's only twin\n")
	})

	t.Run("a terminal is asked which twin", func(t *testing.T) {
		code, _, stderr := runSandboxCLITTY(t, "2\n", "sandbox", "services", "get")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "? Which twin?", "1) stripe", "2) postgres")
	})

	t.Run("two NAMEs is a usage error", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get", "stripe", "postgres")
		if code != 1 || !strings.Contains(stderr, `sandbox services get takes one twin name (got "stripe postgres")`) {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestSandboxServicesManual(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	dataBench(t, plane, twins.services())

	t.Run("rendered lightly on stderr", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "manual", "stripe")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"stripe · testing notes\n",
			"stripe\n",
			"Credentials  Any sk_test_ key works.\n",
			"    curl -X POST $STRIPE_API_BASE/v1/customers\n",
			"Faults\n",
			"A faults row with error.status 402 makes the next matching request fail.\n",
			"→ veris sandbox services manual stripe --raw   (the markdown itself)\n")
		if strings.Contains(stderr, "```") || strings.Contains(stderr, "# stripe") || strings.Contains(stderr, "## Faults") {
			t.Errorf("fences and hashes must not print:\n%s", stderr)
		}
	})

	t.Run("--raw is the markdown itself on stdout", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "manual", "stripe", "--raw")
		if code != 0 || stdout != stripeManual {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		if strings.Contains(stderr, "testing notes") {
			t.Errorf("--raw prints nothing but the markdown:\n%s", stderr)
		}
	})

	t.Run("a twin without a manual", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "manual", "postgres")
		if code != 0 || stdout != "" || !strings.Contains(stderr, "! postgres has no manual (data plane)\n") {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
	})

	t.Run("headings are bold when colour is on", func(t *testing.T) {
		got := renderManual("# Title\ntext\n#hashtag is not a heading\n", true)
		want := []string{"\033[1mTitle\033[0m", "text", "#hashtag is not a heading"}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("renderManual = %q, want %q", got, want)
		}
	})
}

func TestSandboxServicesOperations(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	dataBench(t, plane, twins.services("linear"))

	t.Run("every surface, with the profile's key", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "operations", "stripe")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"stripe · 5 operations\n",
			"GET", "/v1/customers\n",
			"POST", "/v1/customers\n",
			"GET", "/v1/customers/{customer}\n",
			"  MCP at /mcp (auth bearer) · 2 tools\n",
			"    get_widget\n",
			"    list_widgets\n")
		calls := twins.operationsCalls()
		last := calls[len(calls)-1]
		if last.twin != "stripe" || last.key != sbTestKey || last.query != "" {
			t.Errorf("GET /veris/operations = %+v; want stripe, the profile's key, no query", last)
		}
	})

	t.Run("--surface is sent as the query", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "operations", "stripe", "--surface", "mcp")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		calls := twins.operationsCalls()
		if last := calls[len(calls)-1]; last.query != "surface=mcp" {
			t.Errorf("query %q, want surface=mcp", last.query)
		}
		sbInOrder(t, stderr, "stripe · 2 operations (mcp)\n", "  MCP at /mcp (auth bearer) · 2 tools\n")
		if strings.Contains(stderr, "/v1/customers") {
			t.Errorf("--surface mcp printed REST routes:\n%s", stderr)
		}
	})

	t.Run("a GraphQL twin lists type and field", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "operations", "linear")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "linear · 2 operations\n", "query", "issue\n", "mutation", "issueCreate\n")
	})

	t.Run("--json is the twin's document as sent", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "operations", "stripe", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
		}
		mcp, _ := doc["mcp"].(map[string]any)
		if doc["service"] != "stripe" || doc["total"] != float64(5) || mcp["sign_in"] != "oauth" {
			t.Errorf("document lost fields:\n%s", stdout)
		}
	})

	t.Run("a twin with no operations route", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "services", "operations", "postgres")
		if code != 0 || stdout != "" || !strings.Contains(stderr, "! postgres publishes no operation list (GET /veris/operations is not served)\n") {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		code, stdout, _ = runSandboxCLI(t, "sandbox", "services", "operations", "postgres", "--json")
		if code != 0 || stdout != "{\n  \"operations\": null,\n  \"service\": \"postgres\"\n}\n" {
			t.Errorf("--json: exit %d, stdout %q", code, stdout)
		}
	})

	t.Run("an unknown surface is refused before any request", func(t *testing.T) {
		before := len(twins.operationsCalls())
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "operations", "stripe", "--surface", "soap")
		if code != 1 || !strings.Contains(stderr, `--surface must be rest, graphql or mcp (got "soap")`) || len(twins.operationsCalls()) != before {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

// A split sandbox's /c/ control URL is served only to the profile's key; a
// refusal there names the login to redo rather than a missing route.
func TestTwinVerbsCarryTheKeyToAControlProxy(t *testing.T) {
	plane := newSandboxPlane(t)
	var mu sync.Mutex
	var keys []string
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		keys = append(keys, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-API-Key"))
		if refuse {
			sbJSON(w, 401, map[string]string{"detail": "invalid or missing API key"})
			return
		}
		switch r.URL.Path {
		case "/c/" + sbID + "/stripe/veris/reset":
			sbJSON(w, 200, map[string]any{"reset": true, "seeded": map[string]int{"customers": 3}})
		case "/c/" + sbID + "/stripe/veris/operations":
			sbJSON(w, 200, map[string]any{"service": "stripe", "total": 0, "operations": []any{}})
		default:
			sbJSON(w, 404, map[string]string{"detail": "Not Found"})
		}
	}))
	t.Cleanup(srv.Close)
	control := srv.URL + "/c/" + sbID + "/stripe"
	dataBench(t, plane, []api.ServiceInfo{{Name: "stripe", Status: "ready", URL: srv.URL + "/s/" + sbID + "/stripe", ControlURL: control}})

	for _, argv := range [][]string{
		{"sandbox", "reset", "stripe", "--yes"},
		{"sandbox", "services", "operations", "stripe"},
	} {
		if code, _, stderr := runSandboxCLI(t, argv...); code != 0 {
			t.Errorf("%v: exit %d:\n%s", argv, code, stderr)
		}
	}
	mu.Lock()
	want := []string{
		"POST /c/" + sbID + "/stripe/veris/reset " + sbTestKey,
		"GET /c/" + sbID + "/stripe/veris/operations " + sbTestKey,
	}
	if strings.Join(keys, "|") != strings.Join(want, "|") {
		t.Errorf("requests %q, want %q", keys, want)
	}
	refuse = true
	mu.Unlock()

	for _, argv := range [][]string{
		{"sandbox", "reset", "stripe", "--yes"},
		{"sandbox", "services", "operations", "stripe"},
	} {
		code, _, stderr := runSandboxCLI(t, argv...)
		if code != 1 || !strings.Contains(stderr, "[401] control plane rejected the Veris credential; run veris login (invalid or missing API key)\n") ||
			!strings.Contains(stderr, "→ Next: veris login --profile default\n") {
			t.Errorf("%v: exit %d:\n%s", argv, code, stderr)
		}
	}
}
