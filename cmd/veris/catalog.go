package main

import (
	"context"
	"strings"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
)

// servicesCommand is `veris services`: the control plane's catalog of twins
// a project can name in `env create --services`. The picker inside env
// create shows the same list on a terminal; this is the list for a script
// or an agent that has to know the valid names before it asks for them.
func servicesCommand() *cli.Command {
	return &cli.Command{
		Name:    "services",
		Summary: "The catalog: every twin an environment can include",
		Usage:   "veris services [--json]",
		Help: "One line per twin: its name (what --services takes), what it stands in for, the\n" +
			"variable its URL is handed to the app under, and the vendor hostnames the proxy\n" +
			"intercepts for it. A twin with no hostnames is not intercepted: either a data\n" +
			"plane (a DSN, handed to the app) or one this plane serves no hostname for yet.\n" +
			"veris doctor names which, on its vendor-hostnames line.",
		Run: func(ctx *cli.Context, args []string) error {
			if err := noPositionals(ctx, args); err != nil {
				return err
			}
			return servicesCatalog(ctx)
		},
	}
}

func servicesCatalog(ctx *cli.Context) error {
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	catalog, err := c.ListServices(context.Background())
	if err != nil {
		return s.fail("list", "services", err)
	}
	if ctx.Globals.JSON {
		return printJSON(ctx.Stdout, catalog)
	}
	rows := make([][]string, 0, len(catalog))
	for _, svc := range catalog {
		hosts := make([]string, 0, len(svc.Routes))
		for _, r := range svc.Routes {
			hosts = append(hosts, r.Host)
		}
		host := strings.Join(hosts, ", ")
		if host == "" {
			// The catalog carries no URL, so "a DSN by design" and "not
			// measured yet" look the same here. Say only what is known.
			host = "— (not intercepted)"
		}
		hint := svc.EnvHint
		if hint == "" {
			hint = "—"
		}
		// The name carries the dependency rather than a column of its own:
		// a fifth column costs every row its width, and this is empty for
		// all but the family services.
		name := svc.Name
		if len(svc.Requires) > 0 {
			name += " (+" + strings.Join(svc.Requires, ",") + ")"
		}
		rows = append(rows, []string{name, oneLine(svc.Description, 56), hint, host})
	}
	s.ui.Info("Available on %s (%d twins)", s.res.APIBase, len(catalog))
	s.ui.Table([]string{"Name", "Description", "Env hint", "Hosts"}, rows)
	s.ui.Detail("(+name) is added automatically with the service beside it: its sign-in issuer.")
	s.ui.Next("veris env create --services name,name")
	return nil
}

// oneLine is a description cut to fit a table column: the catalog's texts
// run to a paragraph, and the full text is a --json away. The cut lands on
// a word boundary and ends with an ellipsis.
func oneLine(text string, width int) string {
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) <= width {
		return string(r)
	}
	cut := string(r[:width])
	if i := strings.LastIndexByte(cut, ' '); i > width/2 {
		cut = cut[:i]
	}
	return cut + "…"
}

// --- family dependencies ----------------------------------------------------

// A service that signs in through a family issuer (google-calendar through
// google-identity) is not usable without it, so the control plane adds the
// issuer to every sandbox holding one -- the client's auth base URL resolves
// without anyone having to know the relationship. The catalog names it, and
// these read it back so the CLI can say so before the twin turns up
// unexplained.
//
// A plane too old to serve the fields returns none, which reads as "nothing
// is added" -- the behaviour the CLI had before it could ask.

// autoAdded is the services a sandbox of these will hold that were never
// named: each requested service's issuer, once, in the order the requests
// introduce them.
func autoAdded(requested []string, catalog []api.CatalogService) []string {
	byName := make(map[string]api.CatalogService, len(catalog))
	for _, svc := range catalog {
		byName[svc.Name] = svc
	}
	present := make(map[string]bool, len(requested))
	for _, n := range requested {
		present[n] = true
	}
	var added []string
	for _, n := range requested {
		for _, dep := range byName[n].Requires {
			if !present[dep] {
				present[dep] = true
				added = append(added, dep)
			}
		}
	}
	return added
}

// addedFor is the requested services that brought issuer along, so a line
// about it can say whose sign-in it serves.
func addedFor(issuer string, requested []string, catalog []api.CatalogService) []string {
	byName := make(map[string]api.CatalogService, len(catalog))
	for _, svc := range catalog {
		byName[svc.Name] = svc
	}
	var whose []string
	for _, n := range requested {
		for _, dep := range byName[n].Requires {
			if dep == issuer {
				whose = append(whose, n)
				break
			}
		}
	}
	return whose
}

// withAdded is a service list written the way it will exist: the services
// asked for, then the issuers that come with them.
func withAdded(requested []string, catalog []api.CatalogService) string {
	list := strings.Join(requested, ", ")
	if added := autoAdded(requested, catalog); len(added) > 0 {
		list += " + " + strings.Join(added, ", ")
	}
	return list
}

// announceAdded says which issuers a selection brings along and whose
// sign-in each one serves. Silent when nothing is added, so the ordinary
// single-service environment gains no line.
func announceAdded(s *session, requested []string, catalog []api.CatalogService) {
	for _, issuer := range autoAdded(requested, catalog) {
		s.ui.Detail("%s is added automatically (%s signs in through it)",
			issuer, strings.Join(addedFor(issuer, requested, catalog), ", "))
	}
}

// addedNote is why a twin is in the sandbox though the environment never
// named it: the platform put it there for a service that signs in through
// it. "" for a twin the environment asked for, and for every twin when the
// environment could not be read -- an unexplained line is better than a
// wrong explanation.
//
// requested is the environment's own service list; catalog may be nil, which
// costs the note its reason but not its existence.
func addedNote(name string, requested []string, catalog []api.CatalogService) string {
	if len(requested) == 0 {
		return ""
	}
	for _, r := range requested {
		if r == name {
			return ""
		}
	}
	if whose := addedFor(name, requested, catalog); len(whose) > 0 {
		return "added automatically: " + strings.Join(whose, ", ") + " signs in through it"
	}
	return "added automatically; the environment does not name it"
}
