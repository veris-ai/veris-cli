package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/discovery"
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
//	--config  >  --sandbox  >  $VERIS_PROXY_CONFIG  >  $VERIS_SANDBOX_ID
func resolveConfig(src configSources) (*config.Config, string, error) {
	if src.File != "" {
		cfg, err := config.Load(src.File)
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
	cfg, skipped, err := discovery.ToConfig(snapshot)
	if err != nil {
		return nil, "", err
	}
	reportSkipped(skipped)
	return cfg, fmt.Sprintf("sandbox %s (via %s)", sandboxID, via), nil
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
