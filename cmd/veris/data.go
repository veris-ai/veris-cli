package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/twin"
)

const (
	// defaultDataLimit is how many rows `data get NAME TABLE` shows.
	defaultDataLimit = 20
	// schemaLineWidth is where a table's column list is cut with an ellipsis.
	schemaLineWidth = 90
	// cellWidth is where one cell of a row listing is cut.
	cellWidth = 40
)

// sandboxDataCommand is `veris sandbox data …`: schemas, rows, and adding,
// editing and removing your own data in a running sandbox.
func sandboxDataCommand() *cli.Command {
	var schemaID, schemaTable, getID, addID, setID, deleteID string
	var limit int
	return &cli.Command{
		Name:    "data",
		Summary: "A sandbox's data: schema, get, add, set, delete",
		Usage:   "veris sandbox data <command> [--id ID] [flags]",
		Help: "schema is the shape of the rows a twin accepts (GET /veris/schema), get reads counts or rows\n" +
			"(GET /veris/data), add posts rows of your own from a file keyed by twin name, exactly as up\n" +
			"does with the environment's data files. set changes a row that already exists (PATCH) and\n" +
			"delete removes one (DELETE), each named field by field on the command line. Each verb acts\n" +
			"on this folder's sandbox unless --id names another.",
		Sub: []*cli.Command{
			{
				Name:    "schema",
				Summary: "The shape of addable rows: tables and columns, for one twin or all",
				Usage:   "veris sandbox data schema [NAME] [--table T] [--id ID] [--json]",
				Help: "An HTTP twin's schema is JSON Schema per table; --table T prints every column with its type,\n" +
					"whether it is required, and its doc. A data-plane twin's is introspected from your own SQL.\n" +
					"--json prints the raw document.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&schemaID, "id", "", "sandbox id (default: this folder's)")
					fs.StringVar(&schemaTable, "table", "", "one table's full column list")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) > 1 {
						return fmt.Errorf("sandbox data schema takes one twin NAME at most (got %q)", strings.Join(args, " "))
					}
					name := ""
					if len(args) == 1 {
						name = args[0]
					}
					if schemaTable != "" && name == "" {
						return errors.New("--table needs the twin NAME whose table it is")
					}
					return dataSchema(ctx, schemaID, name, schemaTable)
				},
			},
			{
				Name:    "get",
				Summary: "Row counts per table, or the rows of one table newest first",
				Usage:   "veris sandbox data get [NAME [TABLE]] [--limit N] [--id ID] [--json]",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&getID, "id", "", "sandbox id (default: this folder's)")
					fs.IntVar(&limit, "limit", defaultDataLimit, "rows to show of a table")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) > 2 {
						return fmt.Errorf("sandbox data get takes NAME and TABLE at most (got %q)", strings.Join(args, " "))
					}
					if limit <= 0 {
						return fmt.Errorf("--limit must be positive (got %d)", limit)
					}
					name, table := "", ""
					if len(args) > 0 {
						name = args[0]
					}
					if len(args) > 1 {
						table = args[1]
					}
					return dataGet(ctx, getID, name, table, limit)
				},
			},
			{
				Name:    "add",
				Summary: "Add rows of your own from files keyed by twin name",
				Usage:   "veris sandbox data add FILE… [--id ID]",
				Help: "Each FILE is {twin: {table: [rows]}}; a postgres twin takes {\"sql\": PATH} instead, with PATH\n" +
					"relative to the project directory as in up's data files. Every twin validates its rows and\n" +
					"answers with what it added; a refusal is printed reason by reason and stops the run, with\n" +
					"nothing applied for that twin. Rows are additive: keep them with veris snapshot create or\n" +
					"veris baseline promote.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&addID, "id", "", "sandbox id (default: this folder's)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if len(args) == 0 {
						return errors.New("sandbox data add needs at least one FILE")
					}
					return dataAdd(ctx, addID, args)
				},
			},
			{
				Name:    "set",
				Summary: "Change a row that already exists, by its key",
				Usage:   "veris sandbox data set NAME TABLE FIELD=VALUE… [--id ID]",
				Help: "The row is the one the fields name, so `data set yente config id=1 dataset_scope=default`\n" +
					"edits the config row whose id is 1 and leaves its other columns alone. Each VALUE is read\n" +
					"as JSON and kept as the literal string when it is not one: id=1 is a number, enabled=true\n" +
					"a boolean, x=null null, mode=permissive a string. One row at a time, and the only way to\n" +
					"change the clock, client and auth singletons; whole files of new rows are add.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&setID, "id", "", "sandbox id (default: this folder's)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					name, table, row, err := dataArgs("set", args)
					if err != nil {
						return err
					}
					return dataSet(ctx, setID, name, table, row)
				},
			},
			{
				Name:    "delete",
				Summary: "Remove a row, by its key",
				Usage:   "veris sandbox data delete NAME TABLE FIELD=VALUE… [--id ID] [--yes]",
				Help: "The row is the one the fields name, as set names it, and the key alone is enough:\n" +
					"`data delete stripe faults id=flt_1` disarms a fault. Each VALUE is read as JSON and kept\n" +
					"as the literal string when it is not one. It asks first, since rows do not come back;\n" +
					"--yes answers. A twin refuses to delete its singletons and says what to do instead.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&deleteID, "id", "", "sandbox id (default: this folder's)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					name, table, row, err := dataArgs("delete", args)
					if err != nil {
						return err
					}
					return dataDelete(ctx, deleteID, name, table, row)
				},
			},
		},
	}
}

// --- schema documents -------------------------------------------------------

// schemaDoc is one twin's GET /veris/schema, parsed as far as the CLI
// prints it. An HTTP twin's document is JSON Schema whose top-level
// properties are its tables, each an array of rows; the postgres twin's is
// the introspected {tables} shape, kept in pg. raw is the document as sent,
// for --json.
type schemaDoc struct {
	raw    json.RawMessage
	tables []schemaTable
	pg     *twin.PostgresSchema
}

// schemaTable is one HTTP-twin table: its columns in the document's order,
// the required ones, its own doc, and whether it is a world singleton the
// user can read and PATCH but never add to.
type schemaTable struct {
	name        string
	description string
	columns     []schemaColumn
	required    []string
	singleton   bool
	items       json.RawMessage
}

// schemaColumn is one column: its name and its JSON Schema.
type schemaColumn struct {
	name string
	raw  json.RawMessage
}

// parseSchema tells the two shapes apart by their top-level key and reads
// each. Column order matters here -- the twin lists a table's columns in
// its model's order, id first -- so properties are walked as tokens rather
// than decoded into a map.
func parseSchema(raw json.RawMessage) (*schemaDoc, error) {
	var head struct {
		Properties json.RawMessage `json:"properties"`
		Tables     json.RawMessage `json:"tables"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("the schema is not a JSON object: %w", err)
	}
	doc := &schemaDoc{raw: raw}
	if len(head.Tables) > 0 && string(head.Tables) != "null" {
		var pg twin.PostgresSchema
		if err := json.Unmarshal(raw, &pg); err != nil {
			return nil, fmt.Errorf("the schema's tables are not the introspected shape: %w", err)
		}
		doc.pg = &pg
		return doc, nil
	}
	names, props, err := orderedObject(head.Properties)
	if err != nil {
		return nil, fmt.Errorf("the schema's properties: %w", err)
	}
	for _, name := range names {
		var t struct {
			Description string `json:"description"`
			Items       struct {
				Properties json.RawMessage `json:"properties"`
				Required   []string        `json:"required"`
			} `json:"items"`
		}
		if err := json.Unmarshal(props[name], &t); err != nil {
			return nil, fmt.Errorf("table %s: %w", name, err)
		}
		cols, colProps, err := orderedObject(t.Items.Properties)
		if err != nil {
			return nil, fmt.Errorf("table %s's columns: %w", name, err)
		}
		table := schemaTable{
			name:        name,
			description: t.Description,
			required:    t.Items.Required,
			singleton:   isSingleton(name) || strings.Contains(strings.ToLower(t.Description), "singleton"),
		}
		var items struct {
			Items json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(props[name], &items)
		table.items = items.Items
		for _, c := range cols {
			table.columns = append(table.columns, schemaColumn{name: c, raw: colProps[c]})
		}
		doc.tables = append(doc.tables, table)
	}
	return doc, nil
}

// orderedObject reads a JSON object's keys in the order the document lists
// them, with each value raw. Nothing (or null) is an empty object.
func orderedObject(raw json.RawMessage) ([]string, map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, map[string]json.RawMessage{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, errors.New("not a JSON object")
	}
	var keys []string
	values := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("key %v is not a string", tok)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
		values[key] = v
	}
	return keys, values, nil
}

// table is the named table of an HTTP twin's document, nil when absent.
func (d *schemaDoc) table(name string) *schemaTable {
	for i := range d.tables {
		if d.tables[i].name == name {
			return &d.tables[i]
		}
	}
	return nil
}

// addable is the tables a user can add rows to: every table that is not a
// singleton, in the document's order.
func (d *schemaDoc) addable() []schemaTable {
	var out []schemaTable
	for _, t := range d.tables {
		if !t.singleton {
			out = append(out, t)
		}
	}
	return out
}

func (d *schemaDoc) singletons() []string {
	var out []string
	for _, t := range d.tables {
		if t.singleton {
			out = append(out, t.name)
		}
	}
	return out
}

// pgTable is the introspected table named by t, matched as "schema.table"
// or by the bare table name; "" when absent.
func (d *schemaDoc) pgTable(t string) (string, *twin.PostgresTable) {
	for _, key := range sortedPGTables(d.pg) {
		if key == t || strings.HasSuffix(key, "."+t) {
			table := d.pg.Tables[key]
			return key, &table
		}
	}
	return "", nil
}

func sortedPGTables(pg *twin.PostgresSchema) []string {
	keys := make([]string, 0, len(pg.Tables))
	for k := range pg.Tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// columnLabel is a column as the one-line table listing shows it: the name,
// with {a,b,c} for an object column with declared properties and [] for an
// array, so the shape reads without opening the full schema.
func columnLabel(c schemaColumn) string {
	var col struct {
		Type       any             `json:"type"`
		Properties json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(c.raw, &col) != nil {
		return c.name
	}
	if keys, _, err := orderedObject(col.Properties); err == nil && len(keys) > 0 {
		return c.name + "{" + strings.Join(keys, ",") + "}"
	}
	if t, ok := col.Type.(string); ok && t == "array" {
		return c.name + "[]"
	}
	return c.name
}

// columnsLine joins the labels and cuts the line at schemaLineWidth on a
// column boundary, with an ellipsis for what was dropped.
func columnsLine(cols []schemaColumn) string {
	labels := make([]string, 0, len(cols))
	for _, c := range cols {
		labels = append(labels, columnLabel(c))
	}
	line := strings.Join(labels, ", ")
	if utf8.RuneCountInString(line) <= schemaLineWidth {
		return line
	}
	for n := len(labels) - 1; n > 0; n-- {
		line = strings.Join(labels[:n], ", ") + ", …"
		if utf8.RuneCountInString(line) <= schemaLineWidth {
			return line
		}
	}
	return "…"
}

// columnType is a column's type as one word: the declared type (a list
// joined with |), the non-null member of a nullable anyOf, "enum" for a
// closed set, with a format in brackets; "any" when the schema says nothing.
func columnType(raw json.RawMessage) string {
	var col struct {
		Type   any               `json:"type"`
		AnyOf  []json.RawMessage `json:"anyOf"`
		Enum   []any             `json:"enum"`
		Format string            `json:"format"`
	}
	if json.Unmarshal(raw, &col) != nil {
		return "any"
	}
	var t string
	switch v := col.Type.(type) {
	case string:
		t = v
	case []any:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			parts = append(parts, fmt.Sprint(p))
		}
		t = strings.Join(parts, "|")
	}
	if t == "" && len(col.AnyOf) > 0 {
		var parts []string
		for _, member := range col.AnyOf {
			if mt := columnType(member); mt != "null" && mt != "any" {
				parts = append(parts, mt)
			}
		}
		t = strings.Join(parts, "|")
	}
	if len(col.Enum) > 0 {
		t = "enum"
	}
	if t == "" {
		t = "any"
	}
	if col.Format != "" {
		t += " (" + col.Format + ")"
	}
	return t
}

func columnDescription(raw json.RawMessage) string {
	var col struct {
		Description string `json:"description"`
		AnyOf       []struct {
			Description string `json:"description"`
		} `json:"anyOf"`
	}
	if json.Unmarshal(raw, &col) != nil {
		return ""
	}
	if col.Description != "" {
		return col.Description
	}
	for _, m := range col.AnyOf {
		if m.Description != "" {
			return m.Description
		}
	}
	return ""
}

// twinSchema reads and parses one twin's schema.
func twinSchema(ctx context.Context, s *session, svc api.ServiceInfo) (*schemaDoc, error) {
	raw, err := s.twin(svc.ControlURL).Schema(ctx)
	if err != nil {
		return nil, err
	}
	return parseSchema(raw)
}

// --- data schema ------------------------------------------------------------

// dataSchema prints the schema of one twin or, without a name, of every
// twin in turn. --table narrows to one table's full column list; --json
// prints the raw document (or, with --table, that table's row schema).
func dataSchema(ctx *cli.Context, idFlag, name, table string) error {
	s, _, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	targets := sb.Services
	if name != "" {
		svc, err := twinNamed(s, sb, name)
		if err != nil {
			return err
		}
		targets = []api.ServiceInfo{*svc}
	}
	if s.ctx.Globals.JSON {
		return dataSchemaJSON(bg, s, targets, name != "", table)
	}
	for i, svc := range targets {
		if i > 0 {
			s.ui.Info("")
		}
		if svc.ControlURL == "" {
			s.ui.Warn("%s has no control URL to read a schema from (data plane; its schema is your SQL)", svc.Name)
			continue
		}
		doc, err := twinSchema(bg, s, svc)
		if err != nil {
			// A named twin that will not answer is the failure; in the tour
			// of every twin it is a ! line and the next twin still prints,
			// as services list does.
			if name != "" {
				return s.fail("read", "schema of "+svc.Name, err)
			}
			s.ui.Warn("%s did not answer: %v", svc.Name, err)
			continue
		}
		if table != "" {
			if err := printTableSchema(s, svc, doc, table); err != nil {
				return err
			}
			continue
		}
		if doc.pg != nil {
			printPGSchema(s, sb, svc, doc)
			continue
		}
		version, known := stateVersion(bg, s, svc)
		printHTTPSchema(s, sb, svc, doc, version, known, name != "")
	}
	return nil
}

// dataSchemaJSON is --json: the raw document of the named twin, or an
// object keyed by twin name (null for a twin with no control URL). With
// --table, the row schema of that table alone.
func dataSchemaJSON(ctx context.Context, s *session, targets []api.ServiceInfo, named bool, table string) error {
	docs := map[string]json.RawMessage{}
	for _, svc := range targets {
		if svc.ControlURL == "" {
			docs[svc.Name] = json.RawMessage("null")
			continue
		}
		doc, err := twinSchema(ctx, s, svc)
		if err != nil {
			return s.fail("read", "schema of "+svc.Name, err)
		}
		body := doc.raw
		if table != "" {
			body, err = tableSchemaJSON(s, svc, doc, table)
			if err != nil {
				return err
			}
		}
		docs[svc.Name] = body
	}
	if named {
		return printJSON(s.ctx.Stdout, docs[targets[0].Name])
	}
	return printJSON(s.ctx.Stdout, docs)
}

// tableSchemaJSON is one table's schema as the twin sent it: the items
// schema of an HTTP twin's table, the {columns} of an introspected one.
func tableSchemaJSON(s *session, svc api.ServiceInfo, doc *schemaDoc, table string) (json.RawMessage, error) {
	if doc.pg != nil {
		_, t := doc.pgTable(table)
		if t == nil {
			return nil, noSuchTable(s, svc, doc, table)
		}
		body, err := json.Marshal(t)
		return body, err
	}
	t := doc.table(table)
	if t == nil {
		return nil, noSuchTable(s, svc, doc, table)
	}
	return t.items, nil
}

// noSuchTable is the printed error for a --table the schema does not have.
func noSuchTable(s *session, svc api.ServiceInfo, doc *schemaDoc, table string) error {
	var have []string
	if doc.pg != nil {
		have = sortedPGTables(doc.pg)
	} else {
		for _, t := range doc.tables {
			have = append(have, t.name)
		}
	}
	s.ui.Fail("No table '%s' in %s's schema (have: %s)", table, svc.Name, strings.Join(have, ", "))
	s.ui.Next("veris sandbox data schema " + svc.Name)
	return printed(1)
}

// stateVersion is the twin's state_version from /veris/health, for the
// schema header; false when the twin did not say.
func stateVersion(ctx context.Context, s *session, svc api.ServiceInfo) (int, bool) {
	pctx, cancel := context.WithTimeout(ctx, twinProbeTimeout)
	defer cancel()
	h, err := s.twin(svc.ControlURL).Health(pctx)
	if err != nil {
		return 0, false
	}
	return h.StateVersion, true
}

// printHTTPSchema is the per-twin summary of an HTTP twin's schema:
//
//	stripe · 14 tables  (sandbox 7hqz4m2n…, state_version 15)
//	  customers          id, email, name, …
//	  auth · clock · client   (singletons; not addable)
//
// known is whether the health probe answered; a version it did not say is
// printed as —, not as 0, which would read as a freshly reset world.
func printHTTPSchema(s *session, sb *api.Sandbox, svc api.ServiceInfo, doc *schemaDoc, version int, known, hints bool) {
	tables := doc.addable()
	noun := "tables"
	if len(tables) == 1 {
		noun = "table"
	}
	versionText := "—"
	if known {
		versionText = strconv.Itoa(version)
	}
	s.ui.Info("%s · %d %s  (sandbox %s, state_version %s)", svc.Name, len(tables), noun, shortID(sb.ID), versionText)
	rows := make([][]string, 0, len(tables))
	for _, t := range tables {
		rows = append(rows, []string{"  " + t.name, columnsLine(t.columns)})
	}
	if len(rows) > 0 {
		s.ui.Table(nil, rows)
	}
	if singles := doc.singletons(); len(singles) > 0 {
		s.ui.Info("  %s   (singletons; not addable)", strings.Join(singles, " · "))
	}
	if hints && len(tables) > 0 {
		s.ui.Link(fmt.Sprintf("veris sandbox data schema %s --table %s   (every column, with its type and doc)", svc.Name, tables[0].name))
		s.ui.Link(fmt.Sprintf("veris sandbox data schema %s --json   (the raw /veris/schema document)", svc.Name))
	}
}

// printPGSchema is the introspected shape of a data-plane twin:
//
//	postgres · 2 tables  (sandbox 7hqz4m2n…; introspected from your SQL)
//	  public.users   (id, email, created_at)
func printPGSchema(s *session, sb *api.Sandbox, svc api.ServiceInfo, doc *schemaDoc) {
	keys := sortedPGTables(doc.pg)
	noun := "tables"
	if len(keys) == 1 {
		noun = "table"
	}
	s.ui.Info("%s · %d %s  (sandbox %s; introspected from your SQL)", svc.Name, len(keys), noun, shortID(sb.ID))
	if len(keys) == 0 {
		s.ui.Detail("(no tables yet; add DDL with veris sandbox data add)")
		return
	}
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		cols := make([]string, 0, len(doc.pg.Tables[k].Columns))
		for _, c := range doc.pg.Tables[k].Columns {
			cols = append(cols, c.Name)
		}
		rows = append(rows, []string{"  " + k, "(" + strings.Join(cols, ", ") + ")"})
	}
	s.ui.Table(nil, rows)
}

// printTableSchema is --table: every column of one table with its type,
// whether a row must carry it, and its doc.
func printTableSchema(s *session, svc api.ServiceInfo, doc *schemaDoc, table string) error {
	if doc.pg != nil {
		key, t := doc.pgTable(table)
		if t == nil {
			return noSuchTable(s, svc, doc, table)
		}
		s.ui.Info("%s.%s", svc.Name, key)
		rows := make([][]string, 0, len(t.Columns))
		for _, c := range t.Columns {
			null := "not null"
			if c.Nullable {
				null = "nullable"
			}
			rows = append(rows, []string{"  " + c.Name, c.Type, null})
		}
		s.ui.Table(nil, rows)
		return nil
	}
	t := doc.table(table)
	if t == nil {
		return noSuchTable(s, svc, doc, table)
	}
	s.ui.Info("%s.%s", svc.Name, t.name)
	if t.description != "" {
		s.ui.Detail("%s", t.description)
	}
	required := map[string]bool{}
	for _, r := range t.required {
		required[r] = true
	}
	rows := make([][]string, 0, len(t.columns))
	for _, c := range t.columns {
		req := ""
		if required[c.name] {
			req = "required"
		}
		rows = append(rows, []string{"  " + c.name, columnType(c.raw), req, columnDescription(c.raw)})
	}
	s.ui.Table(nil, rows)
	return nil
}

// --- data get ---------------------------------------------------------------

// dataGet is counts per table for every twin (or the named one), or with a
// TABLE the newest rows of that table.
func dataGet(ctx *cli.Context, idFlag, name, table string, limit int) error {
	s, _, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	if table != "" {
		svc, err := twinNamed(s, sb, name)
		if err != nil {
			return err
		}
		return dataRows(bg, s, *svc, table, limit)
	}
	targets := sb.Services
	if name != "" {
		svc, err := twinNamed(s, sb, name)
		if err != nil {
			return err
		}
		targets = []api.ServiceInfo{*svc}
	}
	rows := make([]serviceRow, 0, len(targets))
	for _, svc := range targets {
		rows = append(rows, readServiceRow(bg, s, svc))
	}
	if s.ctx.Globals.JSON {
		// A twin that did not answer has no counts to print; null on stdout
		// would read as a data-plane twin, so the failure is said instead.
		for _, r := range rows {
			if r.err != nil {
				return s.fail("read", "tables of "+r.Name, r.err)
			}
		}
		if name != "" {
			return printJSON(s.ctx.Stdout, rows[0].Tables)
		}
		out := map[string]map[string]int{}
		for _, r := range rows {
			out[r.Name] = r.Tables
		}
		return printJSON(s.ctx.Stdout, out)
	}
	for _, r := range rows {
		switch {
		case r.err != nil:
			s.ui.Warn("%s did not answer: %v", r.Name, r.err)
		case r.Tables == nil:
			s.ui.Info("%s  %s", r.Name, dataPlaneNote)
		default:
			s.ui.Info("%s  (state_version %d)", r.Name, r.StateVersion)
			lines := make([][]string, 0, len(r.Tables))
			for _, t := range sortedTables(r.Tables) {
				lines = append(lines, []string{"  " + t, strconv.Itoa(r.Tables[t])})
			}
			s.ui.Table(nil, lines)
		}
	}
	return nil
}

// dataRows prints one page of a table, newest first, as a table whose
// columns are the rows' keys in the schema's order with id first.
func dataRows(ctx context.Context, s *session, svc api.ServiceInfo, table string, limit int) error {
	if svc.ControlURL == "" {
		s.ui.Fail("%s has no control URL to read rows through (data plane; query it with your own client at %s)", svc.Name, svc.URL)
		return printed(1)
	}
	tw := s.twin(svc.ControlURL)
	page, err := tw.Rows(ctx, table, limit, 0)
	if err != nil {
		return s.fail("read", fmt.Sprintf("table %s of %s", table, svc.Name), err)
	}
	if page.Rows == nil {
		page.Rows = []map[string]any{}
	}
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, page.Rows)
	}
	// The schema's column order is a nicety, not a need: a schema that will
	// not read leaves the columns sorted, id first.
	var schemaOrder []string
	if doc, err := twinSchema(ctx, s, svc); err == nil && doc.pg == nil {
		if t := doc.table(table); t != nil {
			for _, c := range t.columns {
				schemaOrder = append(schemaOrder, c.name)
			}
		}
	}
	s.ui.Info("%s.%s · %d of %d rows (newest first)", svc.Name, table, len(page.Rows), page.Total)
	if len(page.Rows) == 0 {
		s.ui.Detail("(no rows)")
		return nil
	}
	cols := rowColumns(schemaOrder, page.Rows)
	header := make([]string, len(cols))
	copy(header, cols)
	header[0] = "  " + header[0]
	lines := make([][]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		line := make([]string, len(cols))
		for i, c := range cols {
			v, ok := row[c]
			if !ok {
				line[i] = ""
			} else {
				line[i] = cellText(v)
			}
			if i == 0 {
				line[i] = "  " + line[i]
			}
		}
		lines = append(lines, line)
	}
	s.ui.Table(header, lines)
	if page.Total > len(page.Rows) {
		s.ui.Link(fmt.Sprintf("veris sandbox data get %s %s --limit %d   (more rows)", svc.Name, table, min(page.Total, 1000)))
	}
	return nil
}

// rowColumns is the keys the rows carry, in schema order first, then any
// the schema did not name sorted, and id at the front whatever the order.
func rowColumns(schemaOrder []string, rows []map[string]any) []string {
	present := map[string]bool{}
	for _, r := range rows {
		for k := range r {
			present[k] = true
		}
	}
	var cols []string
	seen := map[string]bool{}
	for _, c := range schemaOrder {
		if present[c] && !seen[c] {
			cols = append(cols, c)
			seen[c] = true
		}
	}
	var extra []string
	for k := range present {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	cols = append(cols, extra...)
	for i, c := range cols {
		if c == "id" && i > 0 {
			cols = append([]string{"id"}, append(cols[:i:i], cols[i+1:]...)...)
			break
		}
	}
	return cols
}

// cellText is one value as a table cell: scalars as themselves, null as a
// dash, anything nested as compact JSON, everything on one line and cut at
// cellWidth.
func cellText(v any) string {
	var text string
	switch x := v.(type) {
	case nil:
		return "—"
	case string:
		text = x
	case float64:
		text = strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		text = strconv.FormatBool(x)
	default:
		b, err := json.Marshal(x)
		if err != nil {
			text = fmt.Sprint(x)
		} else {
			text = string(b)
		}
	}
	text = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(text)
	if utf8.RuneCountInString(text) > cellWidth {
		r := []rune(text)
		text = string(r[:cellWidth-1]) + "…"
	}
	return text
}

// --- data add ---------------------------------------------------------------

// dataAdd seeds files of the user's own rows, keyed by twin name, exactly
// as up does with the environment config's data files, and prints per twin
// what landed and how the counts moved:
//
//	✓ stripe: added customers 1, payment_methods 1   (state_version 14 → 15; customers now 41, payment_methods 13)
//
// A twin's refusal (422) is printed reason by reason and stops the run:
// nothing was applied for that twin, and the twins after it are untouched.
// FILE is as given, relative to the working directory; a {"sql": PATH}
// inside it resolves as up resolves the same file: against the project
// directory, or the file's own directory when no project is loaded.
func dataAdd(ctx *cli.Context, idFlag string, files []string) error {
	s, _, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return s.fail("read", file, err)
		}
		refDir := filepath.Dir(file)
		if s.res.Project != nil {
			refDir = s.res.Project.Dir()
		}
		names, byTwin, err := parseDataFile(raw)
		if err != nil {
			return s.fail("parse", file, err)
		}
		for _, name := range names {
			svc := findService(sb.Services, name)
			if svc == nil {
				s.ui.Fail("%s: no twin named '%s' in sandbox %s (have: %s)",
					file, name, sb.ID, strings.Join(serviceNames(sb.Services), ", "))
				s.ui.Next("veris sandbox services list")
				return printed(1)
			}
			if svc.ControlURL == "" {
				s.ui.Fail("%s: twin '%s' has no control URL to add data through", file, name)
				return printed(1)
			}
			tw := s.twin(svc.ControlURL)
			data := byTwin[name]
			if sqlRef, ok := data["sql"].(string); ok {
				sql, err := os.ReadFile(projectPath(refDir, sqlRef))
				if err != nil {
					return s.fail("read", sqlRef, err)
				}
				if _, err := tw.Seed(bg, string(sql)); err != nil {
					return s.fail("seed", name+" from "+sqlRef, err)
				}
				s.ui.Success("%s: seeded %s (%d bytes)", name, sqlRef, len(sql))
				continue
			}
			before, _ := tw.Counts(bg)
			w, err := tw.Add(bg, data)
			if err != nil {
				return s.fail("add", "to "+name, err)
			}
			for _, warn := range w.Warnings {
				s.ui.Warn("%s: %s", name, warn)
			}
			after, _ := tw.Counts(bg)
			s.ui.Success("%s: added %s%s", name, countsLine(w.Added), addDelta(w.Added, before, after))
		}
	}
	return nil
}

// addDelta is the parenthetical after a write: the state_version before and
// after, and the new totals of the tables written, from the two count
// reads. "" when either read failed, since a guessed number is worse than
// none. No tables (nil) is the version alone, which is what an edit that
// changed no row count has to report.
func addDelta(added map[string]int, before, after *twin.Counts) string {
	if before == nil || after == nil {
		return ""
	}
	parts := make([]string, 0, len(added))
	for i, t := range sortedTables(added) {
		if i == 0 {
			parts = append(parts, fmt.Sprintf("%s now %d", t, after.Counts[t]))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d", t, after.Counts[t]))
		}
	}
	delta := fmt.Sprintf("state_version %d → %d", before.StateVersion, after.StateVersion)
	if len(parts) > 0 {
		delta += "; " + strings.Join(parts, ", ")
	}
	return "   (" + delta + ")"
}

// --- data set and data delete -----------------------------------------------

// dataRow is one row named on the command line: the fields as they were
// typed, for the confirmation, and their parsed values, for the body.
type dataRow struct {
	spec   string
	fields map[string]any
}

// dataArgs reads NAME TABLE FIELD=VALUE… as set and delete take them. Both
// verbs act on one row, named by including its key among the fields.
func dataArgs(verb string, args []string) (name, table string, row dataRow, err error) {
	if len(args) < 3 {
		return "", "", row, fmt.Errorf("sandbox data %s needs a twin NAME, a TABLE and at least one FIELD=VALUE", verb)
	}
	row = dataRow{spec: strings.Join(args[2:], " "), fields: map[string]any{}}
	for _, arg := range args[2:] {
		field, value, ok := strings.Cut(arg, "=")
		if !ok || field == "" {
			return "", "", row, fmt.Errorf("sandbox data %s takes fields as FIELD=VALUE (got %q)", verb, arg)
		}
		row.fields[field] = fieldValue(value)
	}
	return args[0], args[1], row, nil
}

// fieldValue is one VALUE typed as the twin's schema means it: JSON when it
// parses as JSON, so id=1 is a number, enabled=true a boolean and x=null
// null, and the literal string otherwise, so mode=permissive and an id like
// cus_1 arrive as themselves.
func fieldValue(v string) any {
	var parsed any
	if json.Unmarshal([]byte(v), &parsed) != nil {
		return v
	}
	return parsed
}

// dataTwin is the sandbox and the twin one row write acts on. An unknown
// name is twinNamed's failure; a twin with no control URL has no write
// route at all, and its rows are the user's own SQL.
func dataTwin(ctx *cli.Context, idFlag, name, verb string) (*session, *api.Sandbox, *twin.Client, error) {
	s, _, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return nil, nil, nil, err
	}
	svc, err := twinNamed(s, sb, name)
	if err != nil {
		return nil, nil, nil, err
	}
	if svc.ControlURL == "" {
		s.ui.Fail("%s has no control URL to %s rows through (data plane; write to it with your own client at %s)",
			svc.Name, verb, svc.URL)
		return nil, nil, nil, printed(1)
	}
	return s, sb, s.twin(svc.ControlURL), nil
}

// dataSet edits the row the fields name (PATCH /veris/data), the columns
// given and no others:
//
//	✓ yente: updated config 1   (state_version 3 → 4)
//
// The parenthetical carries the version alone, not the table's new total:
// an edit adds and removes nothing, so a row count would say nothing about
// what happened. A refusal is printed reason by reason, as add's is.
func dataSet(ctx *cli.Context, idFlag, name, table string, row dataRow) error {
	s, _, tw, err := dataTwin(ctx, idFlag, name, "change")
	if err != nil {
		return err
	}
	bg := context.Background()
	before, _ := tw.Counts(bg)
	w, err := tw.Patch(bg, map[string]any{table: []any{row.fields}})
	if err != nil {
		return s.fail("set", table+" of "+name, err)
	}
	for _, warn := range w.Warnings {
		s.ui.Warn("%s: %s", name, warn)
	}
	after, _ := tw.Counts(bg)
	s.ui.Success("%s: updated %s%s", name, countsLine(w.Updated), addDelta(nil, before, after))
	return nil
}

// dataDelete removes the row the fields name (DELETE /veris/data):
//
//	✓ stripe: deleted faults 1   (state_version 4 → 5; faults now 0)
//
// It asks first, since a deleted row does not come back and --yes is the
// way to say so up front. A twin that will not delete a table -- the clock,
// client, auth and delivery_attempts singletons -- answers with what to do
// instead, and that answer is printed as it came.
func dataDelete(ctx *cli.Context, idFlag, name, table string, row dataRow) error {
	s, sb, tw, err := dataTwin(ctx, idFlag, name, "delete")
	if err != nil {
		return err
	}
	if err := confirm(s.ui, fmt.Sprintf("Delete the row %s from %s.%s (sandbox %s)?", row.spec, name, table, sb.ID)); err != nil {
		return err
	}
	bg := context.Background()
	before, _ := tw.Counts(bg)
	w, err := tw.Delete(bg, map[string]any{table: []any{row.fields}})
	if err != nil {
		return s.fail("delete", table+" of "+name, err)
	}
	for _, warn := range w.Warnings {
		s.ui.Warn("%s: %s", name, warn)
	}
	after, _ := tw.Counts(bg)
	s.ui.Success("%s: deleted %s%s", name, countsLine(w.Deleted), addDelta(w.Deleted, before, after))
	return nil
}
