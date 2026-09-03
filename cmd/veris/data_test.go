package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
)

func TestSandboxDataSchema(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	dataBench(t, plane, twins.services())

	t.Run("an HTTP twin: tables in schema order, columns cut at 90, singletons apart", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "stripe")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"stripe · 5 tables  (sandbox "+shortID(sbID)+", state_version 3)\n",
			"  customers", "id, email, name, created, metadata, balance\n",
			"  payment_methods", "id, customer, type, card{brand,last4}\n",
			"  webhook_endpoints", "id, url, enabled_events[]\n",
			"  wide", "column_number_01, column_number_02, column_number_03, column_number_04, …\n",
			"  faults", "id, path\n",
			"  auth · clock · client   (singletons; not addable)\n",
			"→ veris sandbox data schema stripe --table customers   (every column, with its type and doc)\n",
			"→ veris sandbox data schema stripe --json   (the raw /veris/schema document)\n")
		// auth is a singleton by name alone: its table description does not
		// say so, and only router.py's refusal of an add would.
		if strings.Contains(stderr, "  auth   ") || strings.Contains(stderr, "id, mode") {
			t.Errorf("auth must not be listed as addable:\n%s", stderr)
		}
	})

	t.Run("a state_version the twin did not say is a dash", func(t *testing.T) {
		twins.script(func(f *dataTwins) { f.health = 503 })
		defer twins.script(func(f *dataTwins) { f.health = 0 })
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "stripe")
		if code != 0 || !strings.Contains(stderr, "stripe · 5 tables  (sandbox "+shortID(sbID)+", state_version —)\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("--table is the full column list", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "stripe", "--table", "customers")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"stripe.customers\n",
			"  Customers of the account\n",
			"  id", "string", "required", "Stripe customer id, cus_…\n",
			"  email", "string", "Customer email\n",
			"  name", "string",
			"  created", "integer", "Unix seconds on the sandbox clock\n",
			"  metadata", "any",
			"  balance", "integer")
		if strings.Count(stderr, "required") != 1 {
			t.Errorf("only id is required:\n%s", stderr)
		}
		code, _, stderr = runSandboxCLI(t, "sandbox", "data", "schema", "stripe", "--table", "payment_methods")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "  type", "enum", "  card", "object")
	})

	t.Run("--table the schema does not have", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "stripe", "--table", "customer")
		if code != 1 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"✗ No table 'customer' in stripe's schema (have: customers, payment_methods, webhook_endpoints, wide, faults, auth, clock, client)\n",
			"→ Next: veris sandbox data schema stripe\n")
	})

	t.Run("--table without a NAME", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "--table", "customers")
		if code != 1 || !strings.Contains(stderr, "veris: --table needs the twin NAME") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("--json is the raw document", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "stripe", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		var doc struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil || len(doc.Properties) != 8 {
			t.Errorf("stdout %q: %v", stdout, err)
		}
		code, stdout, stderr = runSandboxCLI(t, "sandbox", "data", "schema", "stripe", "--table", "customers", "--json")
		var items struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if code != 0 || json.Unmarshal([]byte(stdout), &items) != nil || len(items.Properties) != 6 || len(items.Required) != 1 {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
	})

	t.Run("a data-plane twin: the introspected shape", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "postgres")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"postgres · 1 table  (sandbox "+shortID(sbID)+"; introspected from your SQL)\n",
			"  public.users  (id, email)\n")
		code, _, stderr = runSandboxCLI(t, "sandbox", "data", "schema", "postgres", "--table", "users")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "postgres.public.users\n", "  id     integer  not null\n", "  email  text     nullable\n")
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "postgres", "--json")
		var doc struct {
			Tables map[string]json.RawMessage `json:"tables"`
		}
		if code != 0 || json.Unmarshal([]byte(stdout), &doc) != nil || len(doc.Tables) != 1 {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
	})

	t.Run("no NAME is every twin in turn", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "schema")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr, "stripe · 5 tables", "  auth · clock · client", "postgres · 1 table", "  public.users")
		if strings.Contains(stderr, "→ veris sandbox data schema") {
			t.Errorf("the per-twin hints are for one twin:\n%s", stderr)
		}
		code, stdout, stderr = runSandboxCLI(t, "sandbox", "data", "schema", "--json")
		var docs map[string]json.RawMessage
		if code != 0 || json.Unmarshal([]byte(stdout), &docs) != nil || len(docs) != 2 || len(docs["stripe"]) == 0 {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
	})

	t.Run("a twin that does not answer", func(t *testing.T) {
		services := twins.services()
		services[0].ControlURL = "http://127.0.0.1:1"
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(services, time.Now().Add(time.Hour)) }
		})
		defer plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(), time.Now().Add(time.Hour)) }
		})
		// In the tour of every twin it is a ! line and the next twin still
		// prints, as services list does.
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "schema")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr, "! stripe did not answer: ", "postgres · 1 table", "  public.users")
		// Asked for by name it is the failure.
		code, _, stderr = runSandboxCLI(t, "sandbox", "data", "schema", "stripe")
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read schema of stripe: ") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("a twin the sandbox does not have", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "schema", "shopify")
		if code != 1 || !strings.Contains(stderr, "✗ No twin named 'shopify' in sandbox "+sbID) {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}

func TestSandboxDataGet(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	dataBench(t, plane, twins.services())

	t.Run("counts of every twin", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "get")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"stripe  (state_version 3)\n",
			"  customers        41\n",
			"  faults           0\n",
			"  payment_methods  13\n",
			"postgres  (data plane; schema from your SQL)\n")
		if strings.Contains(stderr, "clock") || strings.Contains(stderr, "auth") {
			t.Errorf("the singletons must be hidden:\n%s", stderr)
		}
		code, stdout, stderr = runSandboxCLI(t, "sandbox", "data", "get", "--json")
		var counts map[string]map[string]int
		if code != 0 || json.Unmarshal([]byte(stdout), &counts) != nil || counts["stripe"]["customers"] != 41 || counts["postgres"] != nil {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		if _, ok := counts["stripe"]["auth"]; ok || len(counts["stripe"]) != 3 {
			t.Errorf("the singletons must be hidden under --json too: %v", counts["stripe"])
		}
	})

	t.Run("a twin that does not answer is not null under --json", func(t *testing.T) {
		services := twins.services()
		services[0].ControlURL = "http://127.0.0.1:1"
		plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(services, time.Now().Add(time.Hour)) }
		})
		defer plane.script(func(p *sandboxPlane) {
			p.answer = func(int) *api.Sandbox { return readySandbox(twins.services(), time.Now().Add(time.Hour)) }
		})
		for _, args := range [][]string{{"stripe", "--json"}, {"--json"}} {
			code, stdout, stderr := runSandboxCLI(t, append([]string{"sandbox", "data", "get"}, args...)...)
			if code != 1 || stdout != "" || !strings.Contains(stderr, "✗ Failed to read tables of stripe: ") {
				t.Errorf("%v: exit %d, stdout %q:\n%s", args, code, stdout, stderr)
			}
		}
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "get")
		if code != 0 || !strings.Contains(stderr, "! stripe did not answer: ") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("counts of one twin", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "get", "stripe")
		if code != 0 || !strings.Contains(stderr, "stripe  (state_version 3)\n") || strings.Contains(stderr, "postgres") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "get", "stripe", "--json")
		var counts map[string]int
		if code != 0 || json.Unmarshal([]byte(stdout), &counts) != nil || counts["faults"] != 0 || len(counts) != 3 {
			t.Errorf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
	})

	t.Run("rows of one table, newest first, columns in schema order", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "get", "stripe", "customers", "--limit", "5")
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		sbInOrder(t, stderr,
			"stripe.customers · 2 of 41 rows (newest first)\n",
			"  id", "email", "name", "created", "metadata", "balance\n",
			"  cus_2", "bob@example.com", "—", "1700000000", `{"tier":"gold"}`, "0\n",
			"  cus_1", "ada@example.com", "Ada", "1699999999", "{}", "-5\n",
			"→ veris sandbox data get stripe customers --limit 41   (more rows)\n")
		if q := twins.rowQueries(); len(q) != 1 || q[0] != "entity_type=customers&limit=5" {
			t.Errorf("queries = %q, want entity_type=customers&limit=5", q)
		}
	})

	t.Run("the default limit is 20 and --json prints the rows", func(t *testing.T) {
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "get", "stripe", "customers", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil || len(rows) != 2 || rows[0]["id"] != "cus_2" {
			t.Errorf("stdout %q: %v", stdout, err)
		}
		q := twins.rowQueries()
		if len(q) == 0 || q[len(q)-1] != "entity_type=customers&limit=20" {
			t.Errorf("queries = %q, want the last with limit=20", q)
		}
	})

	t.Run("an empty table", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "get", "stripe", "faults")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "stripe.faults · 0 of 39 rows (newest first)\n", "  (no rows)\n")
	})

	t.Run("a table the twin does not have", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "get", "stripe", "nope")
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read table nope of stripe: [404] unknown entity type 'nope'; valid: ['customers', 'faults']\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("--limit must be positive", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "get", "stripe", "customers", "--limit", "0")
		if code != 1 || !strings.Contains(stderr, "veris: --limit must be positive (got 0)") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("row cells", func(t *testing.T) {
		for in, want := range map[any]string{
			nil:                     "—",
			"a\nb\tc":               "a b c",
			float64(12):             "12",
			float64(1.5):            "1.5",
			true:                    "true",
			strings.Repeat("x", 50): strings.Repeat("x", 39) + "…",
		} {
			if got := cellText(in); got != want {
				t.Errorf("cellText(%v) = %q, want %q", in, got, want)
			}
		}
		if got := cellText([]any{"a", 1.0}); got != `["a",1]` {
			t.Errorf("cellText(list) = %q", got)
		}
		cols := rowColumns([]string{"id", "email", "name"}, []map[string]any{{"zzz": 1, "email": 2}, {"id": 3, "aaa": 4}})
		if strings.Join(cols, ",") != "id,email,aaa,zzz" {
			t.Errorf("rowColumns = %v, want id,email,aaa,zzz", cols)
		}
		cols = rowColumns(nil, []map[string]any{{"b": 1, "id": 2, "a": 3}})
		if strings.Join(cols, ",") != "id,a,b" {
			t.Errorf("rowColumns without a schema = %v, want id,a,b", cols)
		}
	})
}

func TestSandboxDataAdd(t *testing.T) {
	plane := newSandboxPlane(t)
	twins := newDataTwins(t)
	b, _ := dataBench(t, plane, twins.services("zendesk"))
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(b.project, "data", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return "data/" + name
	}

	t.Run("rows land, and the delta is read back", func(t *testing.T) {
		file := write("dev-customers.json", `{"stripe": {
			"customers": [{"id": "cus_dev_ada", "email": "ada@example.com"}],
			"payment_methods": [{"id": "pm_dev_visa", "customer": "cus_dev_ada"}]}}`)
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "data", "add", file)
		if code != 0 || stdout != "" {
			t.Fatalf("exit %d, stdout %q:\n%s", code, stdout, stderr)
		}
		if want := "✓ stripe: added customers 1, payment_methods 1   (state_version 3 → 4; customers now 42, payment_methods 14)\n"; !strings.Contains(stderr, want) {
			t.Errorf("want %q in:\n%s", want, stderr)
		}
		if got := twins.addedTo(); len(got) != 1 || got[0] != "stripe" {
			t.Errorf("adds = %v, want one to stripe", got)
		}
	})

	t.Run("a refusal is printed reason by reason and stops the run", func(t *testing.T) {
		twins.script(func(f *dataTwins) { f.addStatus = 422; f.adds = nil })
		defer twins.script(func(f *dataTwins) { f.addStatus = 0 })
		file := write("bad.json", `{"stripe": {"customers": [{"id": 1}]}, "zendesk": {"tickets": [{"id": "t_1"}]}}`)
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "add", file)
		if code != 1 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"✗ Failed to add to stripe: [422]\n",
			"  customers[0].email: must be a string\n",
			"  unknown table 'customer'\n")
		if strings.Contains(stderr, "✓") {
			t.Errorf("nothing succeeded:\n%s", stderr)
		}
		if got := twins.addedTo(); len(got) != 1 || got[0] != "stripe" {
			t.Errorf("adds = %v, want stripe alone: zendesk comes after it and must be untouched", got)
		}
	})

	t.Run("a twin the sandbox does not have", func(t *testing.T) {
		twins.script(func(f *dataTwins) { f.adds = nil })
		file := write("shopify.json", `{"shopify": {"orders": []}, "stripe": {"customers": []}}`)
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "add", file)
		if code != 1 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"✗ data/shopify.json: no twin named 'shopify' in sandbox "+sbID+" (have: stripe, postgres, zendesk)\n",
			"→ Next: veris sandbox services list\n")
		if got := twins.addedTo(); len(got) != 0 {
			t.Errorf("adds = %v, want none", got)
		}
	})

	t.Run("a postgres twin takes SQL under its key", func(t *testing.T) {
		write("schema.sql", "create table users (id serial primary key);\n")
		file := write("pg.json", `{"postgres": {"sql": "data/schema.sql"}}`)
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "add", file)
		if code != 0 || !strings.Contains(stderr, "✓ postgres: seeded data/schema.sql (44 bytes)\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		if got := twins.seeded(); len(got) != 1 || !strings.HasPrefix(got[0], "create table users") {
			t.Errorf("seeds = %q", got)
		}
	})

	t.Run("the SQL path resolves against the project, as up's does", func(t *testing.T) {
		twins.script(func(f *dataTwins) { f.seeds = nil })
		write("schema.sql", "create table users (id serial primary key);\n")
		write("pg.json", `{"postgres": {"sql": "data/schema.sql"}}`)
		sub := filepath.Join(b.project, "src")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "add", "../data/pg.json")
		if code != 0 || !strings.Contains(stderr, "✓ postgres: seeded data/schema.sql (44 bytes)\n") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		if got := twins.seeded(); len(got) != 1 {
			t.Errorf("seeds = %q", got)
		}
	})

	t.Run("several files, in order", func(t *testing.T) {
		twins.script(func(f *dataTwins) { f.adds = nil })
		one := write("one.json", `{"stripe": {"customers": [{"id": "cus_a"}]}}`)
		two := write("two.json", `{"zendesk": {"tickets": [{"id": "t_a"}]}}`)
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "add", one, two)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "✓ stripe: added customers 1", "✓ zendesk: added tickets 1")
		if got := twins.addedTo(); strings.Join(got, ",") != "stripe,zendesk" {
			t.Errorf("adds = %v", got)
		}
	})

	t.Run("a file that will not read or parse", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "add", "data/nope.json")
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read data/nope.json: ") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		file := write("list.json", `[1, 2]`)
		code, _, stderr = runSandboxCLI(t, "sandbox", "data", "add", file)
		if code != 1 || !strings.Contains(stderr, "✗ Failed to parse data/list.json: not a JSON object keyed by twin name") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
		file = write("block.json", `{"stripe": [1]}`)
		code, _, stderr = runSandboxCLI(t, "sandbox", "data", "add", file)
		if code != 1 || !strings.Contains(stderr, "✗ Failed to parse data/block.json: 'stripe' must be an object of tables to rows") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})

	t.Run("no FILE", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "data", "add")
		if code != 1 || !strings.Contains(stderr, "veris: sandbox data add needs at least one FILE") {
			t.Errorf("exit %d:\n%s", code, stderr)
		}
	})
}
