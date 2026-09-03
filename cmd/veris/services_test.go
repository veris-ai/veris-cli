package main

import (
	"encoding/json"
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

const stripeManual = "# stripe\n\nCredentials  Any sk_test_ key works.\n\n```\ncurl -X POST $STRIPE_API_BASE/v1/customers\n```\n\n## Faults\nA faults row with error.status 402 makes the next matching request fail.\n"

// dataTwins serves /veris/* for the HTTP twins (stripe, and zendesk when a
// test wants a second one, sharing one world) and for a postgres twin
// under /s/<sandbox>/<twin>. Tests script answers through script() and
// read what was asked through the getters; everything is behind the mutex
// because the handlers run on the server's goroutines.
type dataTwins struct {
	srv *httptest.Server
	mu  sync.Mutex

	counts    map[string]int              // the HTTP twins' GET /veris/data counts, singletons included
	version   int                         // their state_version
	health    int                         // the HTTP twins' GET /veris/health status (0 → 200)
	rows      map[string][]map[string]any // rows per table, newest first
	addStatus int                         // POST /veris/data status (0 → 200)
	adds      []string                    // twins that received a POST /veris/data, in order
	queries   []string                    // raw query of every GET /veris/data with an entity_type
	seeds     []string                    // schema_sql of every POST /veris/seed
	pgData404 bool                        // postgres GET /veris/data is FastAPI's 404 rather than the singletons
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
			sbJSON(w, 200, map[string]any{"added": added, "warnings": []string{}})
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

func (f *dataTwins) addedTo() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.adds...)
}

func (f *dataTwins) rowQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
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
			"→ veris sandbox data get stripe TABLE   (rows, newest first)\n",
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

	t.Run("no NAME", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "services", "get")
		if code != 1 || !strings.Contains(stderr, "veris: sandbox services get needs a twin NAME") {
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
