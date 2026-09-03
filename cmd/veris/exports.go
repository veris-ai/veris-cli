package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cli"
)

// sandbox exports is the sandbox's env hints as shell exports:
//
//	export STRIPE_API_BASE='https://gw.veris.ai/s/7hqz…/stripe'
//	export DATABASE_URL='postgresql://app:app@10.0.0.5:5432/sb?sslmode=require'
//
// so `eval "$(veris sandbox exports)"` points an app at the sandbox with no
// proxy in between: the variables the code under test already reads, valued
// at the twins. Data-plane twins are included, since the proxy hands those
// over the same way. It is the one command whose stdout is meant for eval,
// so stdout carries the lines and nothing else; a twin with no env hint is
// a ! line on stderr, not a blank one on stdout.

// exportFormats are --format's values: env (sourceable exports, the
// default), dotenv (KEY=value, for a .env file) and json (one object).
var exportFormats = []string{"env", "dotenv", "json"}

// sandboxExportsCommand is the exports leaf under sandbox; sandbox get
// --exports reaches the same code.
func sandboxExportsCommand() *cli.Command {
	var id, format string
	return &cli.Command{
		Name:    "exports",
		Summary: "The twins' env hints as shell exports, for eval",
		Usage:   "veris sandbox exports [--id ID] [--format env|dotenv|json]",
		Help: `Prints one line per twin, valued at the sandbox, to STDOUT and nothing
else there, so eval "$(veris sandbox exports)" points an app at the
sandbox without the proxy. --format env (the default) prints sourceable
export lines, dotenv prints KEY=value for a .env file, json prints one
object; --json is --format json. Twins are listed by the control plane;
one with no env hint is skipped with a ! line on stderr.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "sandbox id (default: this folder's)")
			fs.StringVar(&format, "format", "", "env, dotenv or json (default env)")
		},
		Run: func(ctx *cli.Context, args []string) error {
			if err := noPositionals(ctx, args); err != nil {
				return err
			}
			return sandboxExports(ctx, id, format)
		},
	}
}

// sandboxExports reads the sandbox's services and prints their hints in
// format to stdout. idFlag is --id, "" for this folder's sandbox; format ""
// is env, or json under --json.
func sandboxExports(ctx *cli.Context, idFlag, format string) error {
	format, err := exportFormat(ctx, format)
	if err != nil {
		return err
	}
	s, err := newSession(ctx, "", idFlag)
	if err != nil {
		return err
	}
	id, err := s.requireSandbox()
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	services, err := c.GetSandboxServices(context.Background(), id)
	if err != nil {
		return s.fail("read", "services of sandbox "+id, err)
	}
	for _, svc := range services {
		if svc.EnvHint == "" {
			s.ui.Warn("%s has no env hint, so nothing is exported for it", svc.Name)
		}
	}
	return renderExports(ctx.Stdout, format, services)
}

// exportFormat resolves --format: "" is env, or json when --json was given,
// and anything not in exportFormats is a usage error. --json beside a
// --format that is not json is refused rather than quietly outvoted either
// way: under --json stdout carries a JSON body and nothing else, and a
// wrapper that adds --json to every call must not be handed export lines.
func exportFormat(ctx *cli.Context, format string) (string, error) {
	asJSON := ctx.Globals != nil && ctx.Globals.JSON
	if format == "" {
		if asJSON {
			return "json", nil
		}
		return "env", nil
	}
	for _, f := range exportFormats {
		if format == f {
			if asJSON && f != "json" {
				return "", &cli.UsageError{Msg: fmt.Sprintf("--json is --format json; drop --format %s or --json", f)}
			}
			return f, nil
		}
	}
	return "", &cli.UsageError{Msg: fmt.Sprintf("--format %q is not one of %s", format, strings.Join(exportFormats, ", "))}
}

// renderExports writes the services that carry an env hint, in the order the
// control plane lists them. json is one object, so its keys come out sorted.
func renderExports(w io.Writer, format string, services []api.ServiceInfo) error {
	if format == "json" {
		body := map[string]string{}
		for _, svc := range services {
			if svc.EnvHint != "" {
				body[svc.EnvHint] = svc.URL
			}
		}
		return printJSON(w, body)
	}
	for _, svc := range services {
		if svc.EnvHint == "" {
			continue
		}
		var line string
		switch format {
		case "dotenv":
			line = svc.EnvHint + "=" + dotenvValue(svc.URL)
		default:
			line = "export " + svc.EnvHint + "=" + shellQuote(svc.URL)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// shellQuote wraps s in single quotes, the one quoting every POSIX shell
// reads literally; a single quote inside is closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// dotenvValue is s as a .env file carries it: bare when it is one token, in
// double quotes when whitespace or a # would otherwise end it early.
func dotenvValue(s string) string {
	if strings.ContainsAny(s, " \t\"#'\\") {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return `"` + s + `"`
	}
	return s
}
