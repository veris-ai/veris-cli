package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// envCommand is the env group: create, list, get, use, delete. On the server
// an environment is only a named service set; everything that makes a
// start-up specific (TTL, boot image, data files, test command) lives in the
// named config these verbs keep in .veris/twin.yaml, put there by a flag
// rather than by a question.
func envCommand() *cli.Command {
	return &cli.Command{
		Name:    "env",
		Summary: "Named environments, chosen per folder",
		Help: `A project keeps its environments in .veris/twin.yaml (committed); one is
the default, and a folder can override which is in use with env use, which
writes the gitignored .veris/twin.local.yaml. The server side of each is a
named service set; the TTL, boot source, data files and test command are the
project's, and create records each only when a flag names it.`,
		Sub: []*cli.Command{
			envCreateCommand(),
			{
				Name:    "list",
				Summary: "Configured and available environments",
				Usage:   "veris env list [--json]",
				Run:     runEnvList,
			},
			envGetCommand(),
			envUseCommand(),
			envDeleteCommand(),
		},
	}
}

// initCommand is the hidden alias of `env create --default`: the one-shot
// set-up of a folder that has no project file yet. It takes create's flags.
func initCommand() *cli.Command {
	var f createFlags
	return &cli.Command{
		Name:    "init",
		Summary: "Set this folder up: env create --default",
		Usage:   "veris init [NAME] [--services a,b] [--from ID] [--ttl N] [--boot bundle|baseline|snapshot] [--data FILE] [--command 'cmd'] [--image TAG] [--require-service NAME[:N]] [--require-callback PATH[:N]] [--expose PORT] [--strict]",
		Hidden:  true,
		Flags:   f.bind,
		Run: func(ctx *cli.Context, args []string) error {
			f.def = true
			return runEnvCreate(ctx, args, &f)
		},
	}
}

// --- env create ------------------------------------------------------------

// createFlags are env create's answers as flags. Two of them -- the name and
// the services -- are what an environment IS, and a TTY is asked for either
// left out. The rest describe what a sandbox of it DOES, and are recorded
// only when a flag says so: a question about boot sources, seed files, ports
// and images is a wall in front of the first environment, and every one of
// them has an answer `up` and `run` reach without being told.
type createFlags struct {
	services string
	from     string
	ttl      int
	boot     string
	snapshot string
	data     listFlag
	command  optString
	def      bool
	force    bool

	// The proxy: block, as run's flags spell it.
	image           string
	requireService  listFlag
	requireCallback listFlag
	expose          int
	strict          bool
}

func (f *createFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.services, "services", "", "comma-separated service `names` from the catalog")
	fs.StringVar(&f.from, "from", "", "adopt the existing server environment with this `id` instead of creating one")
	fs.IntVar(&f.ttl, "ttl", 0, "sandbox TTL in `minutes`, recorded in the config (left out: the control plane's own default)")
	fs.StringVar(&f.boot, "boot", "", "the `source` a sandbox boots from: bundle, baseline or snapshot")
	fs.StringVar(&f.snapshot, "snapshot", "", "the snapshot `id` or name a sandbox boots, with --boot snapshot")
	fs.Var(&f.data, "data", "data `file` to add after boot (repeatable or comma-separated; none unless given)")
	fs.Var(&f.command, "command", "the test command run through the proxy, as one shell `string` ('' for none)")
	fs.BoolVar(&f.def, "default", false, "make it the project's default environment")
	fs.BoolVar(&f.force, "force", false, "replace an environment of the same name in .veris/twin.yaml")
	fs.StringVar(&f.image, "image", "", "proxy.image: run the test command in this container `image`, the proxy beside it")
	fs.Var(&f.requireService, "require-service", "proxy.require_service: fail a run unless this service was called, `name[:count]` (repeatable or comma-separated)")
	fs.Var(&f.requireCallback, "require-callback", "proxy.require_callback: fail a run unless the app received a callback on this `path[:count]` (repeatable or comma-separated)")
	fs.IntVar(&f.expose, "expose", 0, "proxy.expose: publish this local `port` at a public URL so the sandbox can deliver callbacks")
	fs.BoolVar(&f.strict, "strict", false, "proxy.strict: block unmapped hosts instead of letting them reach the real internet")
}

func envCreateCommand() *cli.Command {
	var f createFlags
	return &cli.Command{
		Name:    "create",
		Summary: "Define a named environment",
		Usage:   "veris env create [NAME] [--services a,b] [--from ID] [--ttl N] [--boot bundle|baseline|snapshot] [--snapshot ID|NAME] [--data FILE] [--command 'cmd'] [--image TAG] [--require-service NAME[:N]] [--require-callback PATH[:N]] [--expose PORT] [--strict] [--default] [--force] [--json]",
		Help: `The name and service list go to the server (POST /v1/environments), or
--from adopts an existing server environment by id. The named config in
.veris/twin.yaml, created when the folder has none, keeps everything else.

Two questions are asked on a TTY: the name and the services (a searchable
picker). Off a TTY both must be flags: the name, and --services or --from.

--boot, --snapshot, --data and --command are recorded only when given, and
nothing is written for the ones left out -- a sandbox then boots the bundle,
seeds nothing, and veris run takes its command after --. Likewise no --ttl
records no ttl_minutes, so the control plane's own default applies. The
first environment of a project is its default.

The proxy flags land in the config's proxy: block, which fills in whatever a
veris run command line leaves out:

  environments:
    NAME:
      proxy:
        image: app:test                  # --image: run the command in this image
        require_service: [stripe:2]      # --require-service: fail unless it was called
        require_callback: [/hooks/pay]   # --require-callback: fail unless delivered
        expose: 3000                     # --expose: publish this port for callbacks
        strict: true                     # --strict: block unmapped hosts`,
		Flags: f.bind,
		Run: func(ctx *cli.Context, args []string) error {
			return runEnvCreate(ctx, args, &f)
		},
	}
}

// bootChoices are the three boot sources --boot accepts.
var bootChoices = []string{"bundle", "baseline", "snapshot"}

func validBoot(b string) bool {
	for _, c := range bootChoices {
		if b == c {
			return true
		}
	}
	return false
}

// envCreated is env create's --json body.
type envCreated struct {
	Name        string          `json:"name"`
	ID          string          `json:"id"`
	Services    []string        `json:"services"`
	TTLMinutes  int             `json:"ttl_minutes"`
	Boot        string          `json:"boot"`
	Snapshot    string          `json:"snapshot,omitempty"`
	Data        []string        `json:"data"`
	Command     []string        `json:"command"`
	Proxy       cfg.ProxyConfig `json:"proxy"`
	Default     bool            `json:"default"`
	Adopted     bool            `json:"adopted"`
	ProjectFile string          `json:"project_file"`
}

// proxyConfig is the proxy: block the create flags spell, nil-free so the
// --json body prints [] rather than null for a list left out.
func (f *createFlags) proxyConfig() cfg.ProxyConfig {
	return cfg.ProxyConfig{
		RequireService:  append([]string{}, f.requireService.vals...),
		RequireCallback: append([]string{}, f.requireCallback.vals...),
		Expose:          f.expose,
		Image:           f.image,
		Strict:          f.strict,
	}
}

// checkProxyFlags refuses a proxy: block run would refuse, now rather than
// at the first run: an entry is parsed exactly as run parses the flag, and a
// callback requirement needs a port for the callback to arrive on.
func (f *createFlags) checkProxyFlags() error {
	for _, v := range f.requireService.vals {
		if _, err := parseRequirement("service", v); err != nil {
			return &cli.UsageError{Msg: err.Error()}
		}
	}
	for _, v := range f.requireCallback.vals {
		if _, err := parseRequirement("callback", v); err != nil {
			return &cli.UsageError{Msg: err.Error()}
		}
	}
	if f.expose < 0 || f.expose > 65535 {
		return &cli.UsageError{Msg: fmt.Sprintf("--expose must be a port, not %d", f.expose)}
	}
	if len(f.requireCallback.vals) > 0 && f.expose == 0 {
		return &cli.UsageError{Msg: "--require-callback asserts what your app received, and nothing can arrive without --expose PORT"}
	}
	return nil
}

func runEnvCreate(ctx *cli.Context, args []string, f *createFlags) error {
	if len(args) > 1 {
		return &cli.UsageError{Msg: "env create takes at most one NAME"}
	}
	if f.boot != "" && !validBoot(f.boot) {
		return &cli.UsageError{Msg: fmt.Sprintf("--boot must be one of %s, not %q", strings.Join(bootChoices, ", "), f.boot)}
	}
	if f.snapshot != "" && f.boot != "" && f.boot != bootSnapshot {
		return &cli.UsageError{Msg: "--snapshot goes with --boot snapshot"}
	}
	// Recorded without one, every `up` of this environment is refused until a
	// --snapshot is typed on the command line. Refusing here beats writing a
	// config that cannot start.
	if f.boot == bootSnapshot && f.snapshot == "" {
		return &cli.UsageError{Msg: "--boot snapshot needs --snapshot ID|NAME"}
	}
	if f.ttl < 0 {
		return &cli.UsageError{Msg: "--ttl must be a positive number of minutes"}
	}
	// The server has no update route, so a service list beside --from would
	// be dropped on the floor; refusing it beats a sandbox that boots without
	// the twin the user named.
	if f.from != "" && f.services != "" {
		return &cli.UsageError{Msg: "--services cannot change an adopted environment; the server has no update route. Omit --services, or omit --from"}
	}
	if err := f.checkProxyFlags(); err != nil {
		return err
	}
	var command []string
	if f.command.set {
		words, err := splitWords(f.command.val)
		if err != nil {
			return &cli.UsageError{Msg: "--command: " + err.Error()}
		}
		command = words
	}
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	bg := context.Background()

	// The project file is written last but decided first: a name that is
	// already taken must be refused before a server row is minted for it.
	proj := s.res.Project
	if proj == nil {
		proj = &cfg.Project{
			Version: 1,
			Project: filepath.Base(s.cwd),
			Path:    filepath.Join(s.cwd, ".veris", "twin.yaml"),
		}
	}
	if proj.Environments == nil {
		proj.Environments = map[string]cfg.EnvConfig{}
	}
	first := len(proj.Environments) == 0
	projFile := relPath(s.cwd, proj.Path)

	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	if name == "" {
		name, err = s.ui.Input("Environment name", filepath.Base(proj.Dir()), "NAME")
		if err != nil {
			return err
		}
	}
	if _, taken := proj.Environments[name]; taken && !f.force {
		s.ui.Fail("Environment '%s' already exists in %s", name, projFile)
		s.ui.Next(fmt.Sprintf("veris env create %s --force to replace it", name))
		return printed(1)
	}

	c, err := s.client()
	if err != nil {
		return err
	}

	// The server side: an existing row adopted by id, or the services to
	// mint one from, checked against the catalog so a typo is refused with
	// the names that exist rather than with the server's 400.
	var env *api.Environment
	var services []string
	if f.from != "" {
		if env, err = c.GetEnvironment(bg, f.from); err != nil {
			return s.fail("adopt", "environment "+f.from, err)
		}
	} else {
		catalog, err := c.ListServices(bg)
		if err != nil {
			return s.fail("list", "services", err)
		}
		if f.services != "" {
			services = splitList(f.services)
			if unknown := unknownServices(services, catalog); len(unknown) > 0 {
				s.ui.Fail("Unknown service(s): %s", strings.Join(unknown, ", "))
				// Written directly rather than through Detail: the list is
				// the fix, not commentary, and --quiet must not hide it.
				fmt.Fprintf(s.ui.Out, "  Available: %s\n", strings.Join(catalogNames(catalog), ", "))
				return printed(1)
			}
		} else {
			picked, err := s.ui.MultiSelect("Select services:", serviceOptions(catalog), nil, "--services")
			if err != nil {
				return err
			}
			for _, o := range picked {
				services = append(services, o.Value)
			}
		}
		if len(services) == 0 {
			s.ui.Fail("An environment needs at least one service")
			s.ui.Next("veris env create " + name + " --services NAME[,NAME…]")
			return printed(1)
		}
	}

	// Asked for once, on a TTY only, and blank is an answer: nothing is
	// recorded and every sandbox of this environment takes the TTL the
	// control plane hands out. The CLI keeps no default of its own, because
	// the bounds are the server's and it is the server's refusal that names
	// them -- a number suggested here was how a config came to hold 240,
	// which is over the maximum, and every later up was refused.
	ttl := f.ttl
	if ttl == 0 && s.ui.TTY {
		ans, err := s.ui.Input("Sandbox TTL in minutes (blank for the control plane's default)", "", "--ttl")
		if err != nil {
			return err
		}
		if ans = strings.TrimSpace(ans); ans != "" {
			n, perr := strconv.Atoi(ans)
			if perr != nil || n <= 0 {
				s.ui.Warn("'%s' is not a number of minutes; recording none (the control plane's default applies)", ans)
			} else {
				ttl = n
			}
		}
	}

	// Boot, snapshot, data files and the test command are flags, never
	// questions. They describe what a SANDBOX does -- which world it starts
	// from, what is added after it boots, what runs against it -- and each
	// has an answer `up` and `run` reach on their own, so asking here put
	// four questions in front of the first environment that nothing needed
	// answered. Left out, nothing is recorded: `up` boots the bundle, seeds
	// nothing, and `run` takes its command after --.
	boot, snapshot := f.boot, f.snapshot
	data := f.data.vals
	if data == nil {
		data = []string{}
	}
	if command == nil {
		command = []string{}
	}

	// The question is put on a TTY with the answer leaning towards yes for
	// the first environment: a project whose file lists environments but
	// names no default is one nothing can start from, so Enter keeps it.
	// Off a TTY the first one is the default without asking and after that
	// the flag is the only way to say yes -- a bool has no "omitted" a script
	// could be held to.
	def := f.def
	if !def {
		if s.ui.TTY {
			if def, err = s.ui.Confirm(fmt.Sprintf("Make '%s' this project's default environment?", name), first, "--default"); err != nil {
				return err
			}
		} else {
			def = first
		}
	}

	adopted := env != nil
	if env == nil {
		if env, err = c.CreateEnvironment(bg, name, services); err != nil {
			return s.fail("create", "environment '"+name+"'", err)
		}
	}

	// The proxy block as given, nil lists and all: yaml's omitempty then
	// leaves out what the flags left out, so the file reads as if written
	// by hand.
	proj.Environments[name] = cfg.EnvConfig{
		ID:         env.ID,
		TTLMinutes: ttl,
		Boot:       boot,
		Snapshot:   snapshot,
		Data:       data,
		Proxy: cfg.ProxyConfig{
			RequireService: f.requireService.vals, RequireCallback: f.requireCallback.vals,
			Expose: f.expose, Image: f.image, Strict: f.strict,
		},
		Run: cfg.RunConfig{Command: command},
	}
	if def {
		proj.Default = name
	}
	if err := proj.Save(); err != nil {
		// The server row exists now; say so, or the retry mints a second.
		return fmt.Errorf("write %s: %w (environment %s exists on the server; retry with --from %s)", proj.Path, err, env.ID, env.ID)
	}
	ensureIgnored(s, proj)

	svcs := strings.Join(env.Services, ", ")
	if adopted {
		s.ui.Success("Adopted existing environment %s (%s) as '%s'", shortID(env.ID), serverLabel(env), name)
	} else {
		s.ui.Success("Environment created: %s (%s: %s)", env.ID, name, svcs)
	}
	added := fmt.Sprintf("Added '%s' to %s", name, projFile)
	if def {
		added += " as the default"
	}
	s.ui.Success("%s", added)
	studioLink(s.ui, s.consoleURL(), "environments", env.ID)
	s.ui.Next("veris up")
	if s.ctx.Globals.JSON {
		return printJSON(ctx.Stdout, envCreated{
			Name: name, ID: env.ID, Services: env.Services, TTLMinutes: ttl, Boot: boot,
			Snapshot: snapshot, Data: data, Command: command, Proxy: f.proxyConfig(),
			Default: def, Adopted: adopted, ProjectFile: proj.Path,
		})
	}
	return nil
}

// ensureIgnored keeps the local file out of git and says so once it is.
// A failure to edit .gitignore is a warning: the project file is written
// and the local one is only at risk of a commit, which Save also warns about.
func ensureIgnored(s *session, p *cfg.Project) {
	wrote, err := cfg.EnsureIgnored(p.Dir())
	switch {
	case err != nil:
		s.ui.Warn("could not add .veris/twin.local.yaml to .gitignore: %v", err)
	case wrote:
		s.ui.Success("Added .veris/twin.local.yaml to .gitignore (per-machine; holds sandbox ids)")
	}
}

// serviceOptions is the catalog as a picker: the name, then its description
// and the vendor hosts it stands in for, or — for a twin with none, which
// is handed to the app rather than intercepted.
func serviceOptions(catalog []api.CatalogService) []ui.Option {
	opts := make([]ui.Option, 0, len(catalog))
	for _, svc := range catalog {
		var hosts []string
		for _, r := range svc.Routes {
			hosts = append(hosts, r.Host)
		}
		detail := "—"
		if len(hosts) > 0 {
			detail = strings.Join(hosts, ", ")
		}
		if svc.Description != "" {
			detail = svc.Description + "  " + detail
		}
		opts = append(opts, ui.Option{Value: svc.Name, Label: svc.Name, Detail: detail})
	}
	return opts
}

func catalogNames(catalog []api.CatalogService) []string {
	names := make([]string, 0, len(catalog))
	for _, svc := range catalog {
		names = append(names, svc.Name)
	}
	return names
}

// unknownServices is the names the catalog does not have, in the order given.
func unknownServices(names []string, catalog []api.CatalogService) []string {
	known := map[string]bool{}
	for _, svc := range catalog {
		known[svc.Name] = true
	}
	var unknown []string
	for _, n := range names {
		if !known[n] {
			unknown = append(unknown, n)
		}
	}
	return unknown
}

// serverLabel renders a server environment as "name: svc, svc", or the
// services alone for a row the server holds without a name.
func serverLabel(env *api.Environment) string {
	svcs := strings.Join(env.Services, ", ")
	if env.Name == "" {
		return svcs
	}
	return env.Name + ": " + svcs
}

// --- env list --------------------------------------------------------------

// envListed is env list's --json body.
type envListed struct {
	ProjectFile string            `json:"project_file,omitempty"`
	Configured  []configuredEnv   `json:"configured"`
	Available   []availableEnv    `json:"available"`
	APIBase     string            `json:"api_base"`
	Org         string            `json:"org,omitempty"`
	Failed      map[string]string `json:"sandbox_list_errors,omitempty"`
}

type configuredEnv struct {
	Name    string        `json:"name"`
	Default bool          `json:"default"`
	InUse   bool          `json:"in_use"`
	Config  cfg.EnvConfig `json:"config"`
}

type availableEnv struct {
	api.Environment
	Config        string `json:"config,omitempty"`
	LiveSandboxes *int   `json:"live_sandboxes"`
}

func runEnvList(ctx *cli.Context, args []string) error {
	if len(args) > 0 {
		return &cli.UsageError{Msg: "env list takes no arguments"}
	}
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	bg := context.Background()
	envs, err := c.ListEnvironments(bg)
	if err != nil {
		return s.fail("list", "environments", err)
	}
	byID := map[string]*api.Environment{}
	for i := range envs {
		byID[envs[i].ID] = &envs[i]
	}

	// Which config points at each server row; the first name in sorted
	// order wins when two configs share an id.
	pointers := map[string]string{}
	var names []string
	if p := s.res.Project; p != nil {
		for n := range p.Environments {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if id := p.Environments[n].ID; id != "" && pointers[id] == "" {
				pointers[id] = n
			}
		}
	}

	// The org name is decoration on the header; a key that cannot read
	// /v1/me still lists what it can see. A 401 is not decoration.
	org := ""
	if me, err := c.Me(bg); err != nil {
		if api.IsStatus(err, 401) {
			return s.fail("read", "/v1/me", err)
		}
		s.ui.Warn("could not read /v1/me for the organisation name: %v", err)
	} else {
		org = orgName(me)
	}

	// Live sandboxes per row: a failed fetch is one warning and a ? in
	// the column, never a failed listing.
	counts := map[string]int{}
	failed := map[string]string{}
	for _, env := range envs {
		sbs, err := c.ListSandboxes(bg, env.ID)
		if err != nil {
			failed[env.ID] = err.Error()
			s.ui.Warn("could not list the sandboxes of %s: %v", env.ID, err)
			continue
		}
		counts[env.ID] = len(liveSandboxes(sbs))
	}

	if s.ctx.Globals.JSON {
		out := envListed{APIBase: s.res.APIBase, Org: org, Configured: []configuredEnv{}, Available: []availableEnv{}}
		if len(failed) > 0 {
			out.Failed = failed
		}
		if p := s.res.Project; p != nil {
			out.ProjectFile = p.Path
			for _, n := range names {
				out.Configured = append(out.Configured, configuredEnv{
					Name: n, Default: p.Default == n, InUse: inUseHere(s, n), Config: p.Environments[n],
				})
			}
		}
		for _, env := range envs {
			row := availableEnv{Environment: env, Config: pointers[env.ID]}
			if n, ok := counts[env.ID]; ok {
				row.LiveSandboxes = &n
			}
			out.Available = append(out.Available, row)
		}
		return printJSON(ctx.Stdout, out)
	}

	if p := s.res.Project; p != nil {
		s.ui.Info("Configured (%s)", filepath.Join(filepath.Base(p.Dir()), ".veris", "twin.yaml"))
		var rows [][]string
		for _, n := range names {
			e := p.Environments[n]
			mark := " "
			switch {
			case inUseHere(s, n):
				mark = "●"
			case p.Default == n:
				mark = "★"
			}
			id, svcs := "—", "—"
			if e.ID != "" {
				id = shortID(e.ID)
			}
			srv := byID[e.ID]
			if srv != nil {
				svcs = strings.Join(srv.Services, ", ")
			}
			rows = append(rows, []string{"  " + mark + " " + n, id, svcs, bootLabel(e, srv), ttlLabel(e.TTLMinutes), dataLabel(e.Data)})
		}
		if len(rows) == 0 {
			s.ui.Detail("(none)")
		}
		s.ui.Table(nil, rows)
	}

	header := "Available on " + s.res.APIBase
	if org != "" {
		header += " (" + org + ")"
	}
	s.ui.Info("%s", header)
	var rows [][]string
	for _, env := range envs {
		name, ptr, live := "—", "—", "—"
		if env.Name != "" {
			name = env.Name
		}
		if n := pointers[env.ID]; n != "" {
			ptr = "→ " + n
		}
		switch n, ok := counts[env.ID]; {
		case !ok:
			live = "?"
		case n == 1:
			live = "1 live sandbox"
		case n > 1:
			live = fmt.Sprintf("%d live sandboxes", n)
		}
		row := []string{"  " + shortID(env.ID), name, strings.Join(env.Services, ", "), ptr, live}
		if pointers[env.ID] == "" {
			row = append(row, "env create NAME --from "+shortID(env.ID))
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		s.ui.Detail("(none)")
	}
	s.ui.Table(nil, rows)
	return nil
}

// inUseHere is whether name is the environment this folder resolved to by
// something more explicit than being the project's default: a local use:,
// VERIS_ENV, or a flag. The default is marked as the default.
func inUseHere(s *session, name string) bool {
	return s.res.EnvName == name && s.res.EnvSource != "" && s.res.EnvSource != cfg.SourceProject
}

// bootLabel is a config's boot source for the table: the baseline's
// revision when the server has one pinned, the snapshot's name, or bundle.
func bootLabel(e cfg.EnvConfig, srv *api.Environment) string {
	switch e.Boot {
	case "baseline":
		if srv != nil && srv.Baseline != nil {
			return "baseline " + shortID(srv.Baseline.RevisionID)
		}
		return "baseline (none yet)"
	case "snapshot":
		return "snapshot " + e.Snapshot
	}
	return "bundle"
}

func ttlLabel(minutes int) string {
	if minutes == 0 {
		return "ttl —"
	}
	return fmt.Sprintf("ttl %d", minutes)
}

func dataLabel(files []string) string {
	switch len(files) {
	case 0:
		return "—"
	case 1:
		return "data 1 file"
	}
	return fmt.Sprintf("data %d files", len(files))
}

// liveSandboxes is the sandboxes still worth counting: a failed one is
// dead and a terminating one is leaving, and neither is what an env delete
// would orphan or an env list should advertise.
func liveSandboxes(sbs []api.Sandbox) []api.Sandbox {
	var live []api.Sandbox
	for _, sb := range sbs {
		if sb.Status != api.StatusFailed && sb.Status != api.StatusTerminating {
			live = append(live, sb)
		}
	}
	return live
}

// orgName is the name of the organisation the credential acts as, "" when
// /v1/me did not name it.
func orgName(me *api.Me) string {
	for _, o := range me.Organizations {
		if o.ID == me.OrganizationID {
			return o.Name
		}
	}
	if len(me.Organizations) == 1 {
		return me.Organizations[0].Name
	}
	return ""
}

// --- env use ---------------------------------------------------------------

func envUseCommand() *cli.Command {
	var global bool
	return &cli.Command{
		Name:    "use",
		Summary: "Choose the environment this folder uses",
		Usage:   "veris env use [NAME|ID] [--global]",
		Help: `NAME resolves against .veris/twin.yaml first, then against the server's
environments by id or exact name; the shortened id env list prints (the
first characters and an ellipsis, or any prefix only one id begins with) is
accepted at both stages. The choice is written to this folder's
.veris/twin.local.yaml (gitignored), or with --global to the active profile
as the default for folders that have no project file.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&global, "global", false, "set the active profile's default_environment instead")
		},
		Run: func(ctx *cli.Context, args []string) error {
			return runEnvUse(ctx, args, global)
		},
	}
}

func runEnvUse(ctx *cli.Context, args []string, global bool) error {
	if len(args) > 1 {
		return &cli.UsageError{Msg: "env use takes at most one NAME"}
	}
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	// Without a project file there is no local file to write, and --global
	// is the form for that folder; said before any name is resolved, so a
	// missing login or a network round trip never hides the real blocker.
	if !global && s.res.Project == nil {
		s.ui.Fail("No .veris/twin.yaml found (searched up from %s)", s.cwd)
		s.ui.Next("veris env create, or veris env use NAME --global to set the profile's default")
		return printed(1)
	}
	if name == "" {
		if name, err = pickEnvironment(s); err != nil {
			return err
		}
	}
	label, id, err := resolveEnvName(s, name)
	if err != nil {
		return err
	}

	if global {
		g := s.res.Global
		if g == nil {
			g = &cfg.Global{Path: cfg.GlobalPath()}
		}
		if g.Profiles == nil {
			g.Profiles = map[string]cfg.Profile{}
		}
		// The profile's default serves folders with no project file, where a
		// config name means nothing: the id is what is kept.
		prof := g.Profiles[s.res.ProfileName]
		prof.DefaultEnvironment = id
		g.Profiles[s.res.ProfileName] = prof
		if err := g.Save(); err != nil {
			return err
		}
		s.ui.Success("Profile '%s' now defaults to '%s' (%s) — written to %s", s.res.ProfileName, label, shortID(id), g.Path)
		s.ui.Next("veris up")
		return nil
	}

	p, err := s.requireProject()
	if err != nil {
		return err
	}
	// A config name is kept as the name, so the local file reads like the
	// project file; a server-only environment is kept by id, the one form
	// Resolve accepts without an entry to look up.
	use := id
	if e, ok := p.Environments[label]; ok && e.ID == id {
		use = label
	}
	s.res.Local.Use = use
	ensureIgnored(s, p)
	if err := s.saveLocal(); err != nil {
		return err
	}
	s.ui.Success("This folder now uses '%s' (%s) — written to %s", label, shortID(id), relPath(s.cwd, s.res.Local.Path))
	s.ui.Next("veris up")
	return nil
}

// pickEnvironment asks which environment, on a TTY: the project's when it
// has any, else the server's. Off a TTY the answer is the NAME argument.
func pickEnvironment(s *session) (string, error) {
	if !s.ui.TTY {
		return "", &ui.NoTTYError{FlagHint: "NAME"}
	}
	if p := s.res.Project; p != nil && len(p.Environments) > 0 {
		var opts []ui.Option
		for _, n := range s.envNames() {
			opts = append(opts, ui.Option{Value: n, Label: n, Detail: shortID(p.Environments[n].ID)})
		}
		opt, err := s.ui.Select("Select an environment:", opts, "NAME")
		if err != nil {
			return "", err
		}
		return opt.Value, nil
	}
	c, err := s.client()
	if err != nil {
		return "", err
	}
	envs, err := c.ListEnvironments(context.Background())
	if err != nil {
		return "", s.fail("list", "environments", err)
	}
	if len(envs) == 0 {
		s.ui.Fail("No environments on %s", s.res.APIBase)
		s.ui.Next("veris env create")
		return "", printed(1)
	}
	var opts []ui.Option
	for _, env := range envs {
		label := env.Name
		if label == "" {
			label = env.ID
		}
		opts = append(opts, ui.Option{Value: env.ID, Label: label, Detail: shortID(env.ID) + "  " + strings.Join(env.Services, ", ")})
	}
	opt, err := s.ui.Select("Select an environment:", opts, "NAME")
	if err != nil {
		return "", err
	}
	return opt.Value, nil
}

// resolveEnvName turns what the user typed into an environment: the label
// to print and the server id. The project file answers first; then the
// server list, by id or exact name, where more than one match is refused
// with the candidates rather than guessed between. A shortened id -- the
// first characters and an ellipsis, as env list prints it, or a bare prefix
// -- is accepted at both stages when exactly one id begins with it, since
// the table is where most ids are read from and a full one cannot be
// copied out of it.
func resolveEnvName(s *session, name string) (label, id string, err error) {
	prefix := idPrefix(name)
	if p := s.res.Project; p != nil {
		if e, ok := p.Environments[name]; ok {
			if e.ID == "" {
				s.ui.Fail("Environment '%s' in %s has no id", name, p.Path)
				s.ui.Next("veris env create " + name + " --from ID --force")
				return "", "", printed(1)
			}
			return name, e.ID, nil
		}
		// A pasted id of a configured environment is that entry, as Resolve
		// reads it: the config name is the label, so the local file keeps
		// the name and env list marks the right row.
		for _, n := range s.envNames() {
			if e := p.Environments[n]; e.ID != "" && e.ID == name {
				return n, e.ID, nil
			}
		}
		var begun []string
		for _, n := range s.envNames() {
			if e := p.Environments[n]; prefix != "" && e.ID != "" && strings.HasPrefix(e.ID, prefix) {
				begun = append(begun, n)
			}
		}
		switch len(begun) {
		case 1:
			return begun[0], p.Environments[begun[0]].ID, nil
		case 0:
		default:
			var rows []string
			for _, n := range begun {
				rows = append(rows, n+" ("+shortID(p.Environments[n].ID)+")")
			}
			s.ui.Fail("'%s' begins the ids of %d environments in %s: %s", name, len(begun), relPath(s.cwd, p.Path), strings.Join(rows, ", "))
			s.ui.Next("veris env use NAME")
			return "", "", printed(1)
		}
	}
	c, err := s.client()
	if err != nil {
		return "", "", err
	}
	envs, err := c.ListEnvironments(context.Background())
	if err != nil {
		return "", "", s.fail("list", "environments", err)
	}
	var matches []api.Environment
	for _, env := range envs {
		if env.ID == name || (env.Name != "" && env.Name == name) {
			matches = append(matches, env)
		}
	}
	// The prefix is tried only when nothing matched outright: an exact name
	// that happens to begin some id must not turn ambiguous.
	if len(matches) == 0 && prefix != "" {
		for _, env := range envs {
			if strings.HasPrefix(env.ID, prefix) {
				matches = append(matches, env)
			}
		}
	}
	switch len(matches) {
	case 1:
		label = matches[0].Name
		if label == "" {
			label = matches[0].ID
		}
		return label, matches[0].ID, nil
	case 0:
		where := "on " + s.res.APIBase
		if p := s.res.Project; p != nil {
			where = "in " + relPath(s.cwd, p.Path) + " or " + where
		}
		s.ui.Fail("No environment '%s' %s", name, where)
		s.ui.Next("veris env list")
		return "", "", printed(1)
	}
	var ids []string
	for _, m := range matches {
		ids = append(ids, m.ID+" ("+serverLabel(&m)+")")
	}
	s.ui.Fail("'%s' names %d environments on %s: %s", name, len(matches), s.res.APIBase, strings.Join(ids, ", "))
	s.ui.Next("veris env use ID")
	return "", "", printed(1)
}

// --- env get ---------------------------------------------------------------

func envGetCommand() *cli.Command {
	return &cli.Command{
		Name:    "get",
		Summary: "One environment's settings and server record",
		Usage:   "veris env get [NAME|ID] [--json]",
		Help: `Without NAME the environment in use resolves as every command resolves it:
--env, VERIS_ENV, this folder's use:, the project default, the profile's
default_environment. A NAME resolves against .veris/twin.yaml first, then
against the server's environments by id or exact name, as env use does; the
shortened id env list prints is accepted too. Each setting is shown with
where it came from, then the server's record (GET /v1/environments/{id}).`,
		Run: runEnvGet,
	}
}

// envGot is env get's --json body.
type envGot struct {
	Name   string           `json:"name"`
	ID     string           `json:"id"`
	Source string           `json:"source"`
	Config *cfg.EnvConfig   `json:"config"`
	Server *api.Environment `json:"server"`
}

func runEnvGet(ctx *cli.Context, args []string) error {
	if len(args) > 1 {
		return &cli.UsageError{Msg: "env get takes at most one NAME"}
	}
	nameArg := ""
	if len(args) == 1 {
		nameArg = args[0]
	}
	// The argument is the command's --env: it lands where a flag lands and
	// resolves against the project file the way every command's does. One
	// the project file has no entry for is then looked up on the server by
	// id or exact name, as env use and env delete --server look it up, so
	// every NAME the tree accepts is accepted here.
	s, err := newSession(ctx, nameArg, "")
	if err != nil {
		return err
	}
	var name, id string
	var conf *cfg.EnvConfig
	if nameArg != "" && s.res.Env == nil {
		if name, id, err = resolveEnvName(s, nameArg); err != nil {
			return err
		}
		// A shortened id resolved to a configured entry: its settings are
		// that entry's, as the full id's would have been through Resolve.
		if p := s.res.Project; p != nil {
			if e, ok := p.Environments[name]; ok && e.ID == id {
				conf = &e
			}
		}
	} else if name, id, conf, err = s.requireEnv(); err != nil {
		return err
	}
	c, err := s.client()
	if err != nil {
		return err
	}
	srv, err := c.GetEnvironment(context.Background(), id)
	if err != nil {
		return s.fail("get", "environment "+id, err)
	}
	source := envSourceLabel(s)
	if s.ctx.Globals.JSON {
		return printJSON(ctx.Stdout, envGot{Name: name, ID: id, Source: string(s.res.EnvSource), Config: conf, Server: srv})
	}

	projFile := ""
	if p := s.res.Project; p != nil {
		projFile = relPath(s.cwd, p.Path)
	}
	rows := [][]string{
		{"Environment", name + " (" + id + ")", source},
		{"Profile", s.res.ProfileName + " → " + s.res.APIBase, profileSourceLabel(s.res.ProfileSource)},
	}
	if conf != nil {
		from := func(set bool) string {
			if set {
				return projFile
			}
			return "default"
		}
		// A config with no ttl_minutes is a dash, as every other unset
		// setting is: the number that applies is the control plane's, and
		// naming one here would be inventing it.
		ttl := "—"
		if conf.TTLMinutes > 0 {
			ttl = fmt.Sprintf("%d min", conf.TTLMinutes)
		}
		boot := "bundle"
		if conf.Boot != "" {
			boot = conf.Boot
		}
		if conf.Boot == "snapshot" {
			boot += " " + conf.Snapshot
		}
		rows = append(rows,
			[]string{"TTL", ttl, from(conf.TTLMinutes > 0)},
			[]string{"Boot", boot, from(conf.Boot != "")},
			[]string{"Data", dashIfEmpty(strings.Join(conf.Data, ", ")), from(len(conf.Data) > 0)},
			[]string{"Callback", dashIfEmpty(conf.CallbackURL), from(conf.CallbackURL != "")},
			[]string{"Proxy", dashIfEmpty(proxyLabel(conf.Proxy)), from(proxyLabel(conf.Proxy) != "")},
			[]string{"Command", dashIfEmpty(strings.Join(conf.Run.Command, " ")), from(len(conf.Run.Command) > 0)},
		)
	} else {
		rows = append(rows, []string{"Settings", "none: a bare id with no entry in .veris/twin.yaml", ""})
	}
	s.ui.Table(nil, rows)

	s.ui.Info("Server record")
	baseline := "none (boots the bundle)"
	if srv.Baseline != nil {
		baseline = srv.Baseline.RevisionID + " (" + srv.Baseline.Image + ")"
		if !srv.Baseline.PromotedAt.IsZero() {
			baseline += " promoted " + srv.Baseline.PromotedAt.Local().Format("2006-01-02 15:04")
		}
	}
	created := "—"
	if !srv.CreatedAt.IsZero() {
		created = srv.CreatedAt.Local().Format("2006-01-02 15:04")
	}
	s.ui.Table(nil, [][]string{
		{"  Name", dashIfEmpty(srv.Name)},
		{"  Services", strings.Join(srv.Services, ", ")},
		{"  Baseline", baseline},
		{"  Created", created},
	})
	studioLink(s.ui, s.consoleURL(), "environments", id)
	return nil
}

// envSourceLabel names the layer that chose the environment, in the user's
// terms: the argument, a variable, a file, a profile.
func envSourceLabel(s *session) string {
	switch s.res.EnvSource {
	case cfg.SourceFlag:
		return "argument"
	case cfg.SourceEnv:
		if os.Getenv(cfg.EnvEnv) != "" {
			return "$" + cfg.EnvEnv
		}
		return "$" + cfg.EnvEnvironmentID
	case cfg.SourceLocal:
		return "folder (" + relPath(s.cwd, s.res.Local.Path) + " use:)"
	case cfg.SourceProject:
		return "project default (" + relPath(s.cwd, s.res.Project.Path) + ")"
	case cfg.SourceProfile:
		return "profile '" + s.res.ProfileName + "' default_environment"
	}
	return string(s.res.EnvSource)
}

// profileSourceLabel names what chose the profile.
func profileSourceLabel(src cfg.Source) string {
	switch src {
	case cfg.SourceFlag:
		return "--profile"
	case cfg.SourceEnv:
		return "$" + cfg.EnvProfile
	case cfg.SourceProject:
		return "the environment's profile:"
	case cfg.SourceProfile:
		return "active profile"
	}
	return string(src)
}

// proxyLabel renders the proxy defaults on one line, "" when none is set.
func proxyLabel(p cfg.ProxyConfig) string {
	var parts []string
	if len(p.RequireService) > 0 {
		parts = append(parts, "require_service "+strings.Join(p.RequireService, ","))
	}
	if len(p.RequireCallback) > 0 {
		parts = append(parts, "require_callback "+strings.Join(p.RequireCallback, ","))
	}
	if p.Expose != 0 {
		parts = append(parts, fmt.Sprintf("expose %d", p.Expose))
	}
	if p.Image != "" {
		parts = append(parts, "image "+p.Image)
	}
	if p.Strict {
		parts = append(parts, "strict")
	}
	return strings.Join(parts, " · ")
}

func dashIfEmpty(v string) string {
	if v == "" {
		return "—"
	}
	return v
}

// --- env delete ------------------------------------------------------------

type deleteFlags struct {
	server  bool
	cascade bool
}

func envDeleteCommand() *cli.Command {
	var f deleteFlags
	return &cli.Command{
		Name:    "delete",
		Summary: "Remove an environment",
		Usage:   "veris env delete NAME|ID [--server] [--cascade] [--yes]",
		Help: `Drops the named config from .veris/twin.yaml (and the default: or local
use: that named it). With --server the server environment goes too, after a
confirmation; that is refused while it has live sandboxes, because the API
has no cascade and a sandbox of a deleted environment answers to nobody
until its TTL. --cascade deletes them first, one by one, and stops at the
first failure rather than delete the record over survivors.`,
		Flags: func(fs *flag.FlagSet) {
			fs.BoolVar(&f.server, "server", false, "also delete the server environment")
			fs.BoolVar(&f.cascade, "cascade", false, "with --server: delete its live sandboxes first")
		},
		Run: func(ctx *cli.Context, args []string) error {
			return runEnvDelete(ctx, args, &f)
		},
	}
}

func runEnvDelete(ctx *cli.Context, args []string, f *deleteFlags) error {
	if len(args) != 1 {
		return &cli.UsageError{Msg: "env delete needs exactly one NAME"}
	}
	if f.cascade && !f.server {
		return &cli.UsageError{Msg: "--cascade goes with --server"}
	}
	name := args[0]
	s, err := newSession(ctx, "", "")
	if err != nil {
		return err
	}
	bg := context.Background()

	p := s.res.Project
	var id string
	inProject := false
	if p != nil {
		if e, ok := p.Environments[name]; ok {
			inProject, id = true, e.ID
		}
	}
	if !inProject {
		if !f.server {
			if p == nil {
				_, err := s.requireProject()
				return err
			}
			s.ui.Fail("No environment '%s' in %s (have: %s)", name, relPath(s.cwd, p.Path), dashIfEmpty(strings.Join(s.envNames(), ", ")))
			s.ui.Next("veris env list, or --server to delete a server environment by id")
			return printed(1)
		}
		if _, id, err = resolveEnvName(s, name); err != nil {
			return err
		}
	}

	if f.server {
		if id == "" {
			s.ui.Fail("Environment '%s' in %s has no id; nothing to delete on the server", name, relPath(s.cwd, p.Path))
			s.ui.Next("veris env delete " + name + " (without --server)")
			return printed(1)
		}
		if err := deleteServerEnv(s, bg, id, name, f.cascade); err != nil {
			return err
		}
	}

	if inProject {
		delete(p.Environments, name)
		wasDefault := p.Default == name
		if wasDefault {
			p.Default = ""
		}
		if err := p.Save(); err != nil {
			return err
		}
		if l := s.res.Local; l != nil && l.Use == name {
			l.Use = ""
			if err := s.saveLocal(); err != nil {
				return err
			}
		}
		removed := fmt.Sprintf("Removed '%s' from %s", name, relPath(s.cwd, p.Path))
		if wasDefault {
			removed += " (it was the default)"
		}
		s.ui.Success("%s", removed)
	}
	return nil
}

// deleteServerEnv deletes environment id on the server, after its live
// sandboxes when cascade is on and after the user agrees. A failure among
// the sandboxes ends it there: the record is never deleted over survivors.
func deleteServerEnv(s *session, bg context.Context, id, name string, cascade bool) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	srv, err := c.GetEnvironment(bg, id)
	if err != nil {
		if api.IsStatus(err, 404) {
			s.ui.Warn("environment %s is already gone on the server", id)
			return nil
		}
		return s.fail("get", "environment "+id, err)
	}
	sbs, err := c.ListSandboxes(bg, id)
	if err != nil {
		return s.fail("list", "the sandboxes of environment "+id, err)
	}
	live := liveSandboxes(sbs)
	if len(live) > 0 && !cascade {
		var ids []string
		for _, sb := range live {
			ids = append(ids, sb.ID)
		}
		s.ui.Fail("Environment has %d live %s (%s).", len(live), plural(len(live), "sandbox", "sandboxes"), strings.Join(ids, ", "))
		fmt.Fprintln(s.ui.Out, "  Once the record is deleted you cannot see or delete them until their TTL. Pass --cascade to delete them first.")
		return printed(1)
	}
	serverName := srv.Name
	if serverName == "" {
		serverName = name
	}
	question := fmt.Sprintf("Delete environment \"%s\" (%s)?", serverName, id)
	if len(live) > 0 {
		question = fmt.Sprintf("Delete %d %s and environment \"%s\"?", len(live), plural(len(live), "sandbox", "sandboxes"), serverName)
	}
	if err := confirm(s.ui, question); err != nil {
		return err
	}
	for _, sb := range live {
		if err := c.DeleteSandbox(bg, id, sb.ID); err != nil {
			if !api.IsStatus(err, 404) {
				return s.fail("delete", "sandbox "+sb.ID, err)
			}
			s.ui.Warn("Sandbox %s was already gone", sb.ID)
		} else {
			s.ui.Success("Sandbox deleted: %s", sb.ID)
		}
		if l := s.res.Local; l != nil && l.Sandbox != nil && l.Sandbox.ID == sb.ID {
			if err := s.forgetSandbox(); err != nil {
				return err
			}
		}
	}
	if err := c.DeleteEnvironment(bg, id); err != nil {
		if !api.IsStatus(err, 404) {
			return s.fail("delete", "environment "+id, err)
		}
		s.ui.Warn("environment %s was already gone", id)
		return nil
	}
	s.ui.Success("Environment deleted: %s", id)
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// --- flag values and splitting ---------------------------------------------

// optString is a string flag that knows whether it was given, so ” can
// mean "none" where an absent flag means "ask".
type optString struct {
	val string
	set bool
}

func (o *optString) String() string { return o.val }
func (o *optString) Set(v string) error {
	o.val, o.set = v, true
	return nil
}

// listFlag is a repeatable, comma-splittable string list that knows whether
// it was given at all.
type listFlag struct {
	vals []string
	set  bool
}

func (l *listFlag) String() string { return strings.Join(l.vals, ",") }
func (l *listFlag) Set(v string) error {
	l.set = true
	l.vals = append(l.vals, splitList(v)...)
	return nil
}

// splitList splits a comma-separated list, trimming and dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// splitWords splits a command line the way a POSIX shell would for the
// simple cases: whitespace separates words, single quotes are literal,
// double quotes keep everything but let a backslash escape " \ $ `, and a
// backslash outside quotes escapes the next character. There is no
// expansion: the words are stored and later handed to exec as they are.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur []rune
	inWord := false
	var quote rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			if quote == '"' && r == '\\' && i+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[i+1]) {
				i++
				r = rs[i]
			}
			cur = append(cur, r)
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '\\' && i+1 < len(rs):
			i++
			cur, inWord = append(cur, rs[i]), true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words, cur, inWord = append(words, string(cur)), cur[:0], false
			}
		default:
			cur, inWord = append(cur, r), true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if inWord {
		words = append(words, string(cur))
	}
	return words, nil
}
