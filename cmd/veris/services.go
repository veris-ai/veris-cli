package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/twin"
)

// dataPlaneNote is what stands in for row counts on a twin that has none:
// the postgres twin's tables are the user's own DDL, and its /veris/data
// reports nothing but the sandbox's clock and client singletons.
const dataPlaneNote = "(data plane; schema from your SQL)"

// sandboxServicesCommand is `veris sandbox services …`: the twins of a
// sandbox, their status, env hints, row counts and manuals.
func sandboxServicesCommand() *cli.Command {
	var listID, getID, manualID string
	var raw bool
	return &cli.Command{
		Name:    "services",
		Summary: "The twins of a sandbox: list, get, manual",
		Usage:   "veris sandbox services <command> [--id ID] [flags]",
		Help: "A twin is one vendor service inside the sandbox. list shows every twin with its row counts,\n" +
			"get adds the URLs and every table, manual prints the twin's own testing notes (GET /veris/manual).\n" +
			"Each verb acts on this folder's sandbox unless --id names another.",
		Sub: []*cli.Command{
			{
				Name:    "list",
				Summary: "Every twin: status, env hint, row counts",
				Usage:   "veris sandbox services list [--id ID] [--json]",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&listID, "id", "", "sandbox id (default: this folder's)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					if err := noPositionals(ctx, args); err != nil {
						return err
					}
					return servicesList(ctx, listID)
				},
			},
			{
				Name:    "get",
				Summary: "One twin: URL, control URL, env hint, status, every table",
				Usage:   "veris sandbox services get NAME [--id ID] [--json]",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&getID, "id", "", "sandbox id (default: this folder's)")
				},
				Run: func(ctx *cli.Context, args []string) error {
					name, err := oneTwinName(ctx, args)
					if err != nil {
						return err
					}
					return servicesGet(ctx, getID, name)
				},
			},
			{
				Name:    "manual",
				Summary: "The twin's testing notes (GET /veris/manual)",
				Usage:   "veris sandbox services manual NAME [--id ID] [--raw | --json]",
				Help: "The manual is the twin's own markdown: which credential it accepts, what the packaged seed\n" +
					"holds, how its faults, callbacks and pagination behave. Without --raw it is rendered lightly\n" +
					"on stderr; --raw prints the markdown itself to stdout; --json prints {service, manual}.",
				Flags: func(fs *flag.FlagSet) {
					fs.StringVar(&manualID, "id", "", "sandbox id (default: this folder's)")
					fs.BoolVar(&raw, "raw", false, "print the markdown itself to stdout")
				},
				Run: func(ctx *cli.Context, args []string) error {
					name, err := oneTwinName(ctx, args)
					if err != nil {
						return err
					}
					return servicesManual(ctx, manualID, name, raw)
				},
			},
		},
	}
}

// oneTwinName is the single NAME a verb takes; none or several is a usage
// error in main's "veris: …" voice.
func oneTwinName(ctx *cli.Context, args []string) (string, error) {
	verb := strings.Join(ctx.Path[1:], " ")
	switch len(args) {
	case 1:
		return args[0], nil
	case 0:
		return "", fmt.Errorf("%s needs a twin NAME (veris sandbox services list shows them)", verb)
	}
	return "", fmt.Errorf("%s takes one twin NAME (got %q)", verb, strings.Join(args, " "))
}

// openSandboxServices is how every services and data verb starts: the
// session, a client for the plane, and the sandbox (--id, VERIS_SANDBOX_ID,
// then this folder's pointer) read from GET /v1/sandboxes/{id} with its
// services from the scoped /services route, the same record the control
// plane serves under both.
func openSandboxServices(ctx *cli.Context, idFlag string) (*session, *api.Client, *api.Sandbox, error) {
	s, err := newSession(ctx, "", idFlag)
	if err != nil {
		return nil, nil, nil, err
	}
	id, err := s.requireSandbox()
	if err != nil {
		return nil, nil, nil, err
	}
	c, err := s.client()
	if err != nil {
		return nil, nil, nil, err
	}
	bg := context.Background()
	sb, err := c.GetSandbox(bg, id)
	if err != nil {
		return nil, nil, nil, s.fail("read", "sandbox "+id, err)
	}
	services, err := c.GetSandboxServices(bg, id)
	if err != nil {
		return nil, nil, nil, s.fail("read", "services of sandbox "+id, err)
	}
	sb.Services = services
	return s, c, sb, nil
}

// twinNamed is the sandbox's service called name, or the printed error
// (exit 1) naming the twins it does have.
func twinNamed(s *session, sb *api.Sandbox, name string) (*api.ServiceInfo, error) {
	if svc := findService(sb.Services, name); svc != nil {
		return svc, nil
	}
	s.ui.Fail("No twin named '%s' in sandbox %s (have: %s)", name, sb.ID, strings.Join(serviceNames(sb.Services), ", "))
	s.ui.Next("veris sandbox services list")
	return nil, printed(1)
}

// isSingleton reports whether a table of the bare GET /veris/data is one of
// the world singletons rather than a table the user can seed: the clock and
// client registration every twin shares, and the auth mode every HTTP twin
// carries (one row per twin; router.py refuses to add to any of the three).
// They are hidden from every count list and every schema listing: a
// data-plane twin reports nothing else, and an HTTP twin's real tables are
// what the user asked for.
func isSingleton(table string) bool {
	return table == "clock" || table == "client" || table == "auth"
}

// twinTables is one twin's row counts from the bare GET /veris/data with the
// singletons hidden, and the state_version the read carried. A twin with no
// control URL, no rows route, or nothing but the singletons is the data
// plane: nil counts and no error. An error is a twin that did not answer.
func twinTables(ctx context.Context, s *session, svc api.ServiceInfo) (map[string]int, int, error) {
	if svc.ControlURL == "" {
		return nil, 0, nil
	}
	pctx, cancel := context.WithTimeout(ctx, twinProbeTimeout)
	defer cancel()
	counts, err := s.twin(svc.ControlURL).Counts(pctx)
	if errors.Is(err, twin.ErrNotSupported) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	tables := map[string]int{}
	for t, n := range counts.Counts {
		if !isSingleton(t) {
			tables[t] = n
		}
	}
	if len(tables) == 0 {
		return nil, counts.StateVersion, nil
	}
	return tables, counts.StateVersion, nil
}

// sortedTables is a count map's keys in order, so two reads print the same.
func sortedTables(counts map[string]int) []string {
	tables := make([]string, 0, len(counts))
	for t := range counts {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	return tables
}

// rowsCell renders counts as "customers 41 · faults 0", the note for a
// data-plane twin, "—" for one that did not answer.
func rowsCell(tables map[string]int, err error) string {
	if err != nil {
		return "—"
	}
	if tables == nil {
		return dataPlaneNote
	}
	parts := make([]string, 0, len(tables))
	for _, t := range sortedTables(tables) {
		parts = append(parts, fmt.Sprintf("%s %d", t, tables[t]))
	}
	return strings.Join(parts, " · ")
}

// serviceRow is one twin as services list and get print it under --json:
// the control plane's record plus what the twin said about its rows.
// Tables is null for a data-plane twin; Error names a twin that did not
// answer.
type serviceRow struct {
	api.ServiceInfo
	Tables       map[string]int `json:"tables"`
	StateVersion int            `json:"state_version,omitempty"`
	Error        string         `json:"error,omitempty"`

	err error
}

func readServiceRow(ctx context.Context, s *session, svc api.ServiceInfo) serviceRow {
	tables, version, err := twinTables(ctx, s, svc)
	row := serviceRow{ServiceInfo: svc, Tables: tables, StateVersion: version, err: err}
	if err != nil {
		row.Error = err.Error()
	}
	return row
}

// expiresIn is the header's "expires in 3h 41m"; a sandbox past its expiry
// or without one says so instead.
func expiresIn(t api.Time) string {
	if t.IsZero() {
		return "expires —"
	}
	left := time.Until(t.Time)
	if left <= 0 {
		return "expired"
	}
	return "expires in " + durationText(left)
}

// --- services list ----------------------------------------------------------

// servicesList prints every twin of the sandbox with its row counts:
//
//	Sandbox 7hqz… · boot baseline wrld-… · expires in 3h 41m
//	  Twin      Status  Env hint         Rows
//	  stripe    ready   STRIPE_API_BASE  customers 41 · faults 0 · payment_methods 13
//	  postgres  ready   DATABASE_URL     (data plane; schema from your SQL)
func servicesList(ctx *cli.Context, idFlag string) error {
	s, c, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return err
	}
	bg := context.Background()
	rows := make([]serviceRow, 0, len(sb.Services))
	for _, svc := range sb.Services {
		rows = append(rows, readServiceRow(bg, s, svc))
	}
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, rows)
	}
	env, envErr := c.GetEnvironment(bg, sb.EnvironmentID)
	s.ui.Info("Sandbox %s · boot %s · %s", sb.ID, bootSource(sb, env), expiresIn(sb.ExpiresAt))
	if envErr != nil {
		s.ui.Warn("could not read environment %s: %v", shortID(sb.EnvironmentID), envErr)
	}
	if len(rows) == 0 {
		s.ui.Info("No twins")
		return nil
	}
	table := make([][]string, 0, len(rows))
	for _, r := range rows {
		hint := r.EnvHint
		if hint == "" {
			hint = "—"
		}
		table = append(table, []string{"  " + r.Name, r.Status, hint, rowsCell(r.Tables, r.err)})
	}
	s.ui.Table([]string{"  Twin", "Status", "Env hint", "Rows"}, table)
	for _, r := range rows {
		if r.err != nil {
			s.ui.Warn("%s did not answer: %v", r.Name, r.err)
		}
	}
	example := "NAME"
	for _, r := range rows {
		if r.Tables != nil {
			example = r.Name
			break
		}
	}
	s.ui.Link(fmt.Sprintf("veris sandbox services get %s   (URL, control URL, every table)", example))
	return nil
}

// --- services get -----------------------------------------------------------

// servicesGet prints one twin: the URLs the control plane hands out, its
// env hint and status, then every table with its count.
func servicesGet(ctx *cli.Context, idFlag, name string) error {
	s, _, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return err
	}
	svc, err := twinNamed(s, sb, name)
	if err != nil {
		return err
	}
	bg := context.Background()
	row := readServiceRow(bg, s, *svc)
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, row)
	}
	dash := func(v string) string {
		if v == "" {
			return "—"
		}
		return v
	}
	s.ui.Info("%s · %s", svc.Name, dash(svc.Status))
	s.ui.Info("  URL:          %s", dash(svc.URL))
	s.ui.Info("  Control URL:  %s", dash(svc.ControlURL))
	s.ui.Info("  Env hint:     %s", dash(svc.EnvHint))
	switch {
	case row.err != nil:
		s.ui.Warn("could not read the tables of %s: %v", svc.Name, row.err)
	case row.Tables == nil:
		s.ui.Info("  Tables:       %s", dataPlaneNote)
	default:
		s.ui.Info("  Tables (state_version %d):", row.StateVersion)
		lines := make([][]string, 0, len(row.Tables))
		for _, t := range sortedTables(row.Tables) {
			lines = append(lines, []string{"    " + t, strconv.Itoa(row.Tables[t])})
		}
		s.ui.Table(nil, lines)
		s.ui.Link(fmt.Sprintf("veris sandbox data get %s TABLE   (a page of rows)", svc.Name))
		s.ui.Link(fmt.Sprintf("veris sandbox services manual %s   (the twin's testing notes)", svc.Name))
	}
	return nil
}

// --- services manual --------------------------------------------------------

// servicesManual prints a twin's manual. A twin without one -- the data
// plane serves no /veris/manual -- is a warning, not a failure: there is
// nothing to read, and the user learned that.
func servicesManual(ctx *cli.Context, idFlag, name string, raw bool) error {
	s, _, sb, err := openSandboxServices(ctx, idFlag)
	if err != nil {
		return err
	}
	svc, err := twinNamed(s, sb, name)
	if err != nil {
		return err
	}
	if svc.ControlURL == "" {
		return noManual(s, svc.Name)
	}
	manual, err := s.twin(svc.ControlURL).Manual(context.Background())
	if errors.Is(err, twin.ErrNotSupported) {
		return noManual(s, svc.Name)
	}
	if err != nil {
		return s.fail("read", "manual of "+svc.Name, err)
	}
	switch {
	case s.ctx.Globals.JSON:
		return printJSON(s.ctx.Stdout, map[string]string{"service": svc.Name, "manual": manual})
	case raw:
		if !strings.HasSuffix(manual, "\n") {
			manual += "\n"
		}
		_, err := io.WriteString(s.ctx.Stdout, manual)
		return err
	}
	s.ui.Info("%s · testing notes", svc.Name)
	for _, line := range renderManual(manual, s.ui.Color) {
		s.ui.Info("%s", line)
	}
	s.ui.Link(fmt.Sprintf("veris sandbox services manual %s --raw   (the markdown itself)", svc.Name))
	return nil
}

// noManual is a data-plane twin, which has no testing notes: a warning,
// and under --json a body whose manual is null, so `--json | jq` still
// reads a document rather than nothing.
func noManual(s *session, name string) error {
	s.ui.Warn("%s has no manual (data plane)", name)
	if s.ctx.Globals.JSON {
		return printJSON(s.ctx.Stdout, map[string]any{"service": name, "manual": nil})
	}
	return nil
}

// renderManual is the light markdown rendering: a heading becomes a bold
// line (bold only when colour is on; the hashes go either way), the lines
// of a code fence are indented and the fences dropped, everything else is
// printed as written.
func renderManual(md string, color bool) []string {
	var out []string
	fenced := false
	for _, line := range strings.Split(strings.TrimRight(md, "\n"), "\n") {
		switch {
		case strings.HasPrefix(strings.TrimSpace(line), "```"):
			fenced = !fenced
		case fenced:
			out = append(out, "    "+line)
		case isHeading(line):
			text := strings.TrimSpace(strings.TrimLeft(line, "#"))
			if color {
				text = "\033[1m" + text + "\033[0m"
			}
			out = append(out, text)
		default:
			out = append(out, line)
		}
	}
	return out
}

// isHeading is an ATX heading: one to six hashes, then a space or nothing.
func isHeading(line string) bool {
	rest := strings.TrimLeft(line, "#")
	hashes := len(line) - len(rest)
	return hashes >= 1 && hashes <= 6 && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}
