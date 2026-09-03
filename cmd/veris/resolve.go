package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/discovery"
	"github.com/veris-ai/veris-proxy/internal/routes"
)

// configSources are the ways a command can be told what to route.
type configSources struct {
	// File is --config: an explicit path.
	File string
	// Sandbox is --sandbox: an id the config is derived from.
	Sandbox string
	// Refresh re-reads the sandbox from the control plane rather than using
	// the cached description.
	Refresh bool

	APIBase string
	APIKey  string

	// Local is the sandbox this folder's .veris/twin.local.yaml points at,
	// set by run only when every explicit source above is silent. It is the
	// last resort, not a merge: a --sandbox, a --config, or either of their
	// environment variables hides it entirely, and run says on stderr when
	// the pointer is what routes.
	Local string

	// Overrides is --route, accumulated per service. For a service it names,
	// the entries REPLACE whatever the control plane or the embedded table
	// would have derived: an override that merged could never say "only this
	// host", which is the whole point of overriding.
	Overrides map[string][]routes.Entry
}

// bindConfigFlags gives a command the standard ways of naming its target.
func bindConfigFlags(fs *flag.FlagSet, src *configSources) {
	fs.StringVar(&src.File, "config", "",
		"path to a proxy config file")
	// No backquotes in these strings: Go's flag package reads backquoted text
	// as the argument NAME, so it rendered "-sandbox veris-proxy use".
	fs.StringVar(&src.Sandbox, "sandbox", "",
		"sandbox `id` to route at (also read from $"+discovery.EnvSandboxID+")")
	fs.BoolVar(&src.Refresh, "refresh", false,
		"re-read the sandbox from the control plane instead of the cached copy")
	fs.StringVar(&src.APIBase, "api-base", "",
		"control plane base URL (defaults to $"+discovery.EnvAPIBase+")")
	fs.StringVar(&src.APIKey, "api-key", "",
		"control plane API key (defaults to $"+discovery.EnvAPIKey+"; never written to disk)")
	fs.Func("route",
		"route a service at a hostname for this run: service=host[/prefix], "+
			"repeatable; replaces that service's derived routes",
		func(value string) error {
			service, entry, err := parseRouteFlag(value)
			if err != nil {
				return err
			}
			if src.Overrides == nil {
				src.Overrides = make(map[string][]routes.Entry)
			}
			src.Overrides[service] = append(src.Overrides[service], entry)
			return nil
		})
}

// parseRouteFlag reads one --route value: service=host[/prefix]. The prefix is
// optional and is everything from the first slash on; the host may carry a
// single leading "*." wildcard, exactly like a config file entry.
func parseRouteFlag(value string) (service string, entry routes.Entry, err error) {
	service, target, found := strings.Cut(value, "=")
	service = strings.TrimSpace(service)
	target = strings.TrimSpace(target)
	if !found || service == "" || target == "" {
		return "", routes.Entry{}, fmt.Errorf(
			"--route %q is not service=host[/prefix] (e.g. --route stripe=api.stripe.com)", value)
	}
	host, path, hasPath := strings.Cut(target, "/")
	if host == "" {
		return "", routes.Entry{}, fmt.Errorf("--route %q names no host before the path", value)
	}
	entry = routes.Entry{Host: host}
	if hasPath && path != "" {
		entry.Paths = []string{"/" + path}
	}
	return service, entry, nil
}

// resolveConfig is where every command gets its configuration.
//
// There is no stored selection: a command routes at what its own flags and
// environment say, and nothing else. An earlier version remembered a sandbox in
// ~/.veris/state.json, which meant two suites could not run against two
// sandboxes at once and a CI job's behaviour depended on a file in someone's
// home directory.
//
// The layers never merge. An explicit --config means exactly that file, because
// a config that silently absorbed state from elsewhere could not be reasoned
// about from its own contents. Precedence runs most-explicit first:
//
//	--config  >  --sandbox  >  $VERIS_PROXY_CONFIG  >  $VERIS_SANDBOX_ID  >  the folder's pointer
//
// The pointer (src.Local) is the one exception to "no stored selection", and
// it is a different kind of state: `veris up` wrote it into this checkout's
// .veris/twin.local.yaml, not into a home directory, so two checkouts route
// at two sandboxes and a CI job that names its sandbox is never touched by
// it. It is consulted only when src.Local is set, which run does after
// checking every explicit source itself.
func resolveConfig(src configSources) (*config.Config, string, error) {
	if src.File != "" {
		cfg, err := config.Load(src.File)
		if err == nil && len(src.Overrides) > 0 {
			err = applyOverridesToFileConfig(cfg, src.Overrides)
		}
		return cfg, src.File, err
	}
	if src.Sandbox != "" {
		return configFromSandbox(src, src.Sandbox, "--sandbox")
	}
	if fromEnv := os.Getenv(discovery.EnvConfig); fromEnv != "" {
		cfg, err := config.Load(fromEnv)
		return cfg, fromEnv, err
	}
	// Exported into the environment of anything `run` launches, so a nested run
	// inherits the same sandbox rather than silently picking a different one,
	// and so a CI job can set it once for a whole pipeline.
	if fromEnv := os.Getenv(discovery.EnvSandboxID); fromEnv != "" {
		return configFromSandbox(src, fromEnv, "$"+discovery.EnvSandboxID)
	}
	if src.Local != "" {
		return configFromSandbox(src, src.Local, "this folder's .veris/twin.local.yaml")
	}

	return nil, "", fmt.Errorf(
		"nothing to route: pass --sandbox <id> (or set $%s), or --config <file>",
		discovery.EnvSandboxID)
}

func configFromSandbox(src configSources, sandboxID, via string) (*config.Config, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	snapshot, err := discovery.SnapshotFor(ctx, sandboxID, src.APIBase, src.APIKey, src.Refresh)
	if err != nil {
		return nil, "", fmt.Errorf("sandbox %s (via %s): %w", sandboxID, via, err)
	}
	if snapshot.Status != "" && snapshot.Status != "ready" {
		fmt.Fprintf(os.Stderr,
			"warning: sandbox %s was %q when last read. Requests may fail until it is ready.\n",
			sandboxID, snapshot.Status)
	}
	cfg, skipped, err := discovery.ToConfig(snapshot, src.Overrides)
	if err != nil {
		return nil, "", err
	}
	reportSkipped(skipped)
	return cfg, fmt.Sprintf("sandbox %s (via %s)", sandboxID, via), nil
}

// applyOverridesToFileConfig rewrites a file config's routes for the services
// --route names. The file's own entries for that service are the only place
// its upstream lives, so a service the file does not mention cannot be
// overridden -- and saying so beats silently intercepting nothing.
func applyOverridesToFileConfig(cfg *config.Config, overrides map[string][]routes.Entry) error {
	upstreams := make(map[string]string)
	for _, svc := range cfg.Services {
		if _, seen := upstreams[svc.Name]; !seen {
			upstreams[svc.Name] = svc.Upstream
		}
	}
	kept := cfg.Services[:0]
	for _, svc := range cfg.Services {
		if _, replaced := overrides[svc.Name]; !replaced {
			kept = append(kept, svc)
		}
	}
	cfg.Services = kept
	for name, entries := range overrides {
		upstream, ok := upstreams[name]
		if !ok {
			return fmt.Errorf(
				"--route names %s, which this config file does not define, so "+
					"there is no upstream to route it at", name)
		}
		for _, entry := range entries {
			cfg.Services = append(cfg.Services, config.Service{
				Name: name, Hosts: []string{entry.Host}, Paths: entry.Paths,
				Upstream: upstream,
			})
		}
	}
	return cfg.Validate()
}

// reportSkipped names every service the sandbox runs that will NOT be
// intercepted. Silence here would let a client believe a dependency was
// covered when its traffic goes straight to the real vendor.
func reportSkipped(skipped []discovery.Unroutable) {
	for _, s := range skipped {
		fmt.Fprintf(os.Stderr, "note: %s is not intercepted -- %s\n", s.Service, s.Reason)
	}
}

// printRoutes shows what a resolved config will intercept, which is the only
// way to see the derived routing without starting anything.
func printRoutes(w *os.File, cfg *config.Config) {
	fmt.Fprintf(w, "sandbox %s\n", cfg.SandboxID)
	for _, svc := range cfg.Services {
		host := svc.Hosts[0]
		if len(svc.Paths) == 0 {
			fmt.Fprintf(w, "  %-22s %s\n", svc.Name, host)
			continue
		}
		for _, p := range svc.Paths {
			fmt.Fprintf(w, "  %-22s %s%s\n", svc.Name, host, p)
		}
	}
}
