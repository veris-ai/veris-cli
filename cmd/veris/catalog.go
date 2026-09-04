package main

import (
	"context"
	"strings"

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
		rows = append(rows, []string{svc.Name, oneLine(svc.Description, 64), hint, host})
	}
	s.ui.Info("Available on %s (%d twins)", s.res.APIBase, len(catalog))
	s.ui.Table([]string{"Name", "Description", "Env hint", "Hosts"}, rows)
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
