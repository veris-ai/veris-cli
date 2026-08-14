// Command veris-proxy intercepts outbound HTTP(S) from code under test and
// routes it to simulated services in a Veris dependency sandbox.
//
// It is normally started by the Veris CLI rather than invoked directly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/veris-ai/veris-proxy/internal/ca"
	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/discovery"
	"github.com/veris-ai/veris-proxy/internal/proxy"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `veris-proxy - route code under test at a Veris dependency sandbox

Usage:
  veris-proxy preflight [--environment <id>] [--image <image>]
  veris-proxy serve     [--sandbox <id>] [--transparent] [--print-routes] [--listen <addr>]
  veris-proxy run       [--environment <id>] [--image <image>] [--require-service <n>] -- <cmd>
  veris-proxy promote   --sandbox <id> [--environment <id>]
  veris-proxy check     [--expect-canary <token>] [--any-run] [--proxy <url>]
  veris-proxy version

What to route (run and serve both accept these, most explicit first):
  --sandbox <id>    derive the routing from this sandbox
  --config <file>   or an explicit config file, used exactly as written
  read from $VERIS_SANDBOX_ID and $VERIS_PROXY_CONFIG when neither is given

  Which hostname belongs to each service comes from the control plane when
  it serves routes, else from the table embedded at this binary's release.
  --route <service>=<host>[/prefix]   replace one service's derived routes
                                      for this run (repeatable)

Commands:
  preflight
          Assert every precondition a run has -- credential, control plane,
          docker, the environment, optionally the test image -- and exit 2 if
          one is missing. Cheap enough to run before every run, and it also
          reports whether the environment has a promoted world, which is the
          only way to learn that a run is rebuilding state somebody already
          built.
  serve   Run the proxy. This is what the container image runs, and what a
          long-lived local session runs. Add --transparent for the kernel
          redirect, which is the only tier that covers every runtime.
  run     Run a command against a sandbox and report what it sent.
          --image runs it in a container, with the proxy in its own container
          beside it -- the image needs no capability, no iptables and no
          change, and no docker commands are yours to write.
          Without --image the command runs LOCALLY with proxy and CA
          environment variables set, which covers only libraries that honour
          them; it builds the JVM truststore itself when a JDK is present.

  promote Make a sandbox's current state the environment's default world, so
          every later sandbox starts from it instead of the boot profile.
          The capture is a boundary: that sandbox is left frozen and
          scrubbed, so promote LAST, when the run is finished with it.
          "run --promote-on-success" is the same move for a one-shot run.

  check   Assert that a live proxy belongs to THIS run, and exit 2 if not.
          A proxy left running from an earlier run against a different
          sandbox would otherwise let a suite pass against the wrong data.

Exit codes:
  0  success
  1  usage or configuration error
  2  an assertion failed: preflight found a missing precondition, or check
     found no proxy, not ours, or a different run
  3  the run did not call a service it was required to call
  4  the run's outcome is indeterminate
  n  otherwise, whatever the command under "run" exited with
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		// A command that ran and failed carries its own status out; printing
		// anything here would talk over its output.
		var code exitCode
		if errors.As(err, &code) {
			os.Exit(int(code))
		}
		var ce checkFailure
		if errors.As(err, &ce) {
			fmt.Fprintf(os.Stderr, "veris-proxy: %v\n", err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "veris-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command given")
	}

	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "serve":
		return cmdServe(args[1:])
	case "preflight":
		return cmdPreflight(args[1:])
	case "promote":
		return cmdPromote(args[1:])
	case "check":
		return cmdCheck(args[1:])
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var sources configSources
	bindConfigFlags(fs, &sources)
	listen := fs.String("listen", "", "override the listen address from the config")
	logLevel := fs.String("log-level", "info", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	transparent := fs.Bool("transparent", false,
		"also serve kernel-redirected connections (for iptables REDIRECT inside a container)")
	tHTTP := fs.String("transparent-http", "0.0.0.0:8081", "listen address for redirected plaintext traffic")
	tHTTPS := fs.String("transparent-https", "0.0.0.0:8443", "listen address for redirected TLS traffic")
	caDir := fs.String("ca-dir", "", "directory holding the CA (overrides the config's ca_dir)")
	strict := fs.Bool("strict", false,
		"block unmapped hosts instead of letting them reach their real destination")
	printRoutesOnly := fs.Bool("print-routes", false,
		"print what would be intercepted, then exit without listening")
	environment := fs.String("environment", os.Getenv("VERIS_ENVIRONMENT_ID"),
		"deploy a fresh sandbox from this environment `id` and delete it "+
			"afterwards, instead of attaching to an existing --sandbox")
	ttlMinutes := fs.Int("ttl-minutes", 60,
		"how long a sandbox created by --environment may live if teardown never "+
			"runs, so a crashed run cannot leak one forever")
	expose := fs.Int("expose", 0,
		"publish this local `port` at a public https URL so the sandbox can "+
			"deliver callbacks to it")
	exposeHost := fs.String("expose-host", proxy.DefaultOriginHost,
		"`host` the exposed port is on; loopback is right when the workload shares "+
			"this network namespace, host.docker.internal when your app is on the host")
	exposeToken := fs.String("expose-token", "",
		"cloudflared named-tunnel `token` (defaults to $VERIS_TUNNEL_TOKEN); "+
			"without it a quick tunnel is used, which needs no account")
	exposeHostname := fs.String("expose-hostname", "",
		"public `hostname` a named tunnel serves; required with --expose-token, "+
			"which announces nothing to read")
	var requireCallback []requirement
	fs.Func("require-callback",
		"fail unless your app received a callback on this path[:count] (* for any path)",
		func(v string) error {
			r, err := parseRequirement("callback", v)
			if err != nil {
				return err
			}
			requireCallback = append(requireCallback, r)
			return nil
		})
	redirectExternal := fs.Bool("redirect-external", false,
		"the kernel redirect is installed by something else; do not install or demand it")
	proxyUID := fs.Int("proxy-uid", defaultProxyUID,
		"uid to drop to, and the one the kernel redirect exempts; must differ "+
			"from whatever the code under test runs as")
	readyFile := fs.String("ready-file", "",
		"write this `file` once every listener is bound (an edge-triggered startup signal)")
	writeEnv := fs.String("write-env", "",
		"write the command's interception environment to this `file`")
	caPublicPath := fs.String("ca-public-path", "",
		"the CA certificate `path` as the command under test will see it, when "+
			"that differs from ours (the CA directory also holds the private key)")
	envTrustOnly := fs.Bool("env-trust-only", false,
		"emit only the CA variables, for a tier where the kernel already does the routing")
	envFormat := fs.String("env-format", "posix",
		"posix (sourceable exports) or docker (for `docker run --env-file`)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger(*logLevel, *logFormat)

	// Checked here, before selfSetup installs iptables rules and drops
	// privileges. Refusing afterwards would leave a namespace redirecting 80
	// and 443 at listeners that are about to close.
	if len(requireCallback) > 0 && *expose <= 0 {
		return errors.New(
			"--require-callback asserts what your app received, and nothing can " +
				"arrive without --expose <port>. Add it, or drop the requirement")
	}
	if *environment != "" && sources.Sandbox != "" {
		return errors.New(
			"--environment deploys a sandbox of its own and --sandbox attaches to " +
				"one that exists. Pick one")
	}
	// --config wins in resolveConfig, so this combination would route egress at
	// the file's sandbox while callbacks were registered on the one just
	// created -- two different sandboxes, with nothing said.
	if *environment != "" && sources.File != "" {
		return errors.New(
			"--environment deploys a sandbox and --config routes at whatever the " +
				"file names, so callbacks and traffic would go to different " +
				"sandboxes. Pick one")
	}
	// Before the tunnel opens, not after the listeners bind. Otherwise a
	// reserved port is caught only once a public URL is already forwarding at
	// the proxy itself, whose status endpoint is unauthenticated.
	if *expose > 0 {
		if err := refuseConfiguredPort(*expose, *exposeHost,
			cfg0Listen(*listen), *tHTTP, *tHTTPS, *transparent); err != nil {
			return err
		}
	}

	// Installed before anything is provisioned. Create and WaitReady can take
	// minutes, and a signal arriving in that window would exit before the
	// deferred teardown ran -- leaving a public tunnel alive in its own process
	// group and a sandbox billing until its TTL.
	// Only when there is something to provision. Registering a second SIGTERM
	// handler unconditionally adds a window, as each registration is torn down
	// on return, in which a signal meant for the serve handler finds none
	// installed and takes the default action -- which killed the test binary
	// on CI while passing locally.
	provisionCtx := context.Background()
	if *environment != "" || *expose > 0 {
		ctx, cancel := signal.NotifyContext(
			context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		provisionCtx = ctx
	}

	// With --environment the order inverts. The tunnel needs only a local port,
	// so it opens FIRST and its URL is handed to the sandbox at creation --
	// which is strictly better than PATCHing afterwards: the sandbox is never
	// alive without knowing where to deliver, and there is no window in which a
	// callback could be produced with nowhere to go.
	var (
		pending    *pendingIngress
		lifetime   *sandboxLifetime
		tunnelDied atomic.Bool
		err        error
	)
	if *environment != "" {
		if *expose > 0 {
			pending, err = openIngress(provisionCtx, log, ingressOptions{
				RunAsUID: *proxyUID,
				Port:     *expose,
				Host:     *exposeHost,
				Token:    firstNonEmpty(*exposeToken, os.Getenv("VERIS_TUNNEL_TOKEN")),
				Hostname: *exposeHostname,
			})
			if err != nil {
				return fmt.Errorf("open the callback path: %w", err)
			}
			defer pending.CloseOnFailure()
		}
		lifetime, err = deploySandbox(
			provisionCtx, log, sources, *environment, *ttlMinutes,
			pending.URL())
		if err != nil {
			return err
		}
		defer lifetime.Delete(log)
		sources.Sandbox = lifetime.SandboxID
		sources.Refresh = true
	}

	cfg, source, cfgErr := resolveConfig(sources)
	if cfgErr != nil {
		return cfgErr
	}
	if *listen != "" {
		cfg.Listen = *listen
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	if *caDir != "" {
		cfg.CADir = *caDir
	}
	if *strict {
		cfg.Mode = config.ModeStrict
	}
	if *printRoutesOnly {
		printRoutes(os.Stdout, cfg)
		return nil
	}

	log.Info("configuration resolved", "from", source)

	opts := proxy.ListenOptions{}
	if *transparent {
		opts.TransparentHTTP, opts.TransparentHTTPS = *tHTTP, *tHTTPS
	}

	// The CA is minted BEFORE the trust store is installed, because installing
	// it reads the file. Getting this backwards produced a container that came
	// up with the redirect live, the CA absent from the trust store, a warning
	// in the log, and every intercepted handshake failing.
	authority, err := ca.Load(expand(cfg.CADir))
	if err != nil {
		return err
	}

	// Then the redirect and the privilege drop, both before a listener is
	// bound. Nothing may become reachable before the traffic is being
	// redirected to it, or a workload racing startup reaches the real vendor.
	if *transparent && runtime.GOOS == "linux" {
		if err := selfSetup(log, setupOptions{
			UID:              *proxyUID,
			TransparentHTTP:  *tHTTP,
			TransparentHTTPS: *tHTTPS,
			Writable:         writableDirs(cfg, *writeEnv, *readyFile),
			CAPath:           authority.CertPath(),
			RedirectExternal: *redirectExternal,
		}); err != nil {
			return err
		}
	}

	srv := proxy.New(cfg, authority, log, version)
	if *transparent && runtime.GOOS != "linux" {
		// The listeners bind on any OS, and on any OS but Linux nothing can
		// ever route to them: the redirect is iptables, which is netfilter,
		// which is Linux. Left unsaid, this looks exactly like it is working.
		log.Warn("--transparent has no effect on this OS: the kernel redirect "+
			"is iptables, which is Linux-only. The listeners will bind and "+
			"receive nothing. Use the explicit proxy, or run in a container.",
			"os", runtime.GOOS)
	}
	if *transparent {
		// Transparent mode is what makes the container tier work: nothing in
		// the process under test has to honour HTTP_PROXY, which is the only
		// way to cover Java, static Go binaries and Apache HttpClient.
		opts.TransparentHTTP, opts.TransparentHTTPS = *tHTTP, *tHTTPS
	}

	running, err := srv.Start(opts)
	if err != nil {
		return err
	}
	announce(log, running, cfg, authority)

	// The shutdown handler arms BEFORE anything announces readiness. The ready
	// marker's contract is "everything is armed": a supervisor (or docker stop)
	// that signals the moment the marker appears must find this handler, not
	// the default action -- which would kill the process with listeners bound
	// and the deferred cleanup (tunnel close, sandbox delete) never run.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		<-stop
		log.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = running.Shutdown(ctx)
	}()

	// Order matters and is the contract: the environment is complete before
	// the readiness marker exists, so a supervisor that sees the marker can
	// source the environment without a second wait.
	var ing *ingress
	if *expose > 0 {
		ing, err = startIngress(provisionCtx, log, ingressOptions{
			RunAsUID: *proxyUID,
			Port:     *expose,
			Host:     *exposeHost,
			Token:    firstNonEmpty(*exposeToken, os.Getenv("VERIS_TUNNEL_TOKEN")),
			Hostname: *exposeHostname,
		}, cfg, running, pending)
		if err != nil {
			return fmt.Errorf("open the callback path: %w", err)
		}
		running.AttachIngress(ing.Inbound)
		// The workload starts after we report ready, so the registration probe
		// ran against a port nothing was on yet. Confirm once it is up.
		ing.confirmWhenReady(context.Background(), ing.originAddr())
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			_ = ing.Stop(ctx)
		}()
	}

	material, err := publishTrust(*caPublicPath, authority)
	if err != nil {
		return fmt.Errorf("publish the trust material: %w", err)
	}
	if *writeEnv != "" {
		if err := writeEnvFile(*writeEnv, *envFormat, material, *envTrustOnly, running, cfg, publicURL(ing)); err != nil {
			return fmt.Errorf("write env file: %w", err)
		}
	}
	if *readyFile != "" {
		if err := writeReadyFile(*readyFile, running); err != nil {
			return fmt.Errorf("write ready file: %w", err)
		}
	}

	if ing != nil {
		// A tunnel that dies mid-run leaves the proxy healthy and registered at
		// a hostname nothing answers, so callbacks vanish with nothing said.
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-ing.tunnel.Done():
				log.Error("the callback tunnel exited; callbacks can no longer arrive")
				// Recorded, not just acted on. Shutting down cleanly would make
				// Wait return nil and the run exit 0 -- a webhook suite passing
				// because ingress vanished is exactly the silent success this
				// direction was built to refuse.
				tunnelDied.Store(true)
				ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
				defer cancel()
				_ = running.Shutdown(ctx)
			case <-watchDone:
			}
		}()
	}

	err = running.Wait()
	if err == nil && tunnelDied.Load() {
		err = errors.New(
			"the callback tunnel exited during the run, so callbacks stopped " +
				"arriving; what the app received is incomplete")
	}
	if ing != nil {
		receipt := ing.Receipt()
		printInbound(os.Stderr, receipt)
		unmet := unmetCallbacks(requireCallback, receipt)
		for _, u := range unmet {
			fmt.Fprintf(os.Stderr, "veris-proxy: %s\n", u)
		}
		if err == nil && len(unmet) > 0 {
			return exitCode(exitRequirementUnmet)
		}
	}
	return err
}

// publicURL is the callback URL, or "" when nothing was exposed.
func publicURL(in *ingress) string {
	if in == nil {
		return ""
	}
	return in.URL
}

// writableDirs is everywhere the proxy still writes after dropping privileges.
// Missing one is not a warning: the CA, the sandbox cache and the handoff
// files are each fatal to lose, and each failed at least once by being
// forgotten here.
func writableDirs(cfg *config.Config, files ...string) []string {
	dirs := []string{expand(cfg.CADir), discovery.Dir()}
	for _, f := range files {
		if f != "" {
			dirs = append(dirs, filepath.Dir(f))
		}
	}
	return dirs
}

// shutdownGrace is how long in-flight requests get to finish before the
// listeners are force-closed.
const shutdownGrace = 5 * time.Second

func announce(log *slog.Logger, running *proxy.Running, cfg *config.Config, authority *ca.CA) {
	log.Info("veris-proxy listening",
		"addr", running.Addr("proxy"),
		"mode", string(cfg.Mode),
		"sandbox_id", cfg.SandboxID,
		"services", len(cfg.Services),
		"ca", authority.CertPath(),
		"ca_fingerprint", authority.Fingerprint(),
	)
	for _, kind := range []string{"transparent-http", "transparent-https"} {
		if addr := running.Addr(kind); addr != "" {
			log.Info("listening", "kind", kind, "addr", addr)
		}
	}
	if cfg.Mode == config.ModePassthrough {
		log.Info("mode=passthrough: only the listed services are rerouted; " +
			"every other host reaches its real destination. Use --strict to block them.")
	}
	for _, p := range cfg.PassEnv {
		// Said out loud so nobody hunts for why the database "isn't proxied":
		// it is handed over instead, under the name the code already reads.
		log.Info("not proxied; handed to the command as an environment variable",
			"service", p.Service, "var", p.Name)
	}
}

// checkFailure marks an interception probe failure, which exits 2 rather than 1
// so a test harness can tell it apart from a usage error.
type checkFailure struct{ error }

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	proxyURL := fs.String("proxy", "", "proxy URL (defaults to $VERIS_PROXY_URL)")
	expect := fs.String("expect-canary", "", "canary token that must match (defaults to $VERIS_CANARY)")
	anyRun := fs.Bool("any-run", false,
		"accept any live Veris proxy, without checking which run it belongs to")
	timeout := fs.Duration("timeout", 5*time.Second, "probe timeout")
	quiet := fs.Bool("quiet", false, "print nothing on success")
	if err := fs.Parse(args); err != nil {
		return err
	}

	url := *proxyURL
	if url == "" {
		url = os.Getenv("VERIS_PROXY_URL")
	}
	if url == "" {
		return checkFailure{errors.New("no proxy URL: pass --proxy or set VERIS_PROXY_URL. " +
			"If you expected the environment to be set, interception is not active")}
	}

	want := *expect
	if want == "" {
		want = os.Getenv("VERIS_CANARY")
	}
	// An assertion must not quietly weaken into a liveness probe. Without a
	// canary this cannot tell the proxy for THIS run from one left running by
	// an earlier one against a different sandbox -- which is the whole failure
	// it exists to catch -- so accepting that has to be said out loud.
	if want == "" && !*anyRun {
		return checkFailure{errors.New(
			"no canary to check against: set --expect-canary or $VERIS_CANARY. " +
				"Pass --any-run to accept any live proxy, which cannot detect one " +
				"left over from an earlier run")}
	}
	if want != "" && *anyRun {
		return errors.New("--any-run and an expected canary are contradictory")
	}

	client := &http.Client{Timeout: *timeout}
	resp, err := client.Get(url + proxy.StatusPath)
	if err != nil {
		return checkFailure{fmt.Errorf("cannot reach the proxy at %s: %w", url, err)}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return checkFailure{fmt.Errorf("read probe response: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		return checkFailure{fmt.Errorf("proxy returned %d: %s", resp.StatusCode, string(body))}
	}

	var state struct {
		VerisProxy  bool   `json:"veris_proxy"`
		SandboxID   string `json:"sandbox_id"`
		Mode        string `json:"mode"`
		CanaryToken string `json:"canary_token"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return checkFailure{fmt.Errorf("response from %s is not a Veris proxy: %w", url, err)}
	}
	if !state.VerisProxy {
		return checkFailure{fmt.Errorf("something is listening at %s but it is not a Veris proxy", url)}
	}
	if want != "" && state.CanaryToken != want {
		// This is the case worth having: a proxy left running from an earlier
		// run, pointing at a different sandbox. Without the token check the
		// tests would pass against the wrong simulated data.
		return checkFailure{fmt.Errorf(
			"canary mismatch: the proxy at %s belongs to a different run (sandbox %s). "+
				"Stop it and restart with the current config", url, state.SandboxID)}
	}
	if !*quiet {
		fmt.Printf("interception live: sandbox %s, mode %s\n", state.SandboxID, state.Mode)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// defaultCADir is under the user's home, except where there is no usable home
// to put it in -- a service account in a container has none, and silently
// choosing an unwritable path fails at first TLS handshake rather than at
// startup.
func defaultCADir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".veris/ca"
	}
	if info, statErr := os.Stat(home); statErr != nil || !info.IsDir() {
		return ".veris/ca"
	}
	return filepath.Join(home, ".veris", "ca")
}

func expand(path string) string {
	if path == "" {
		return defaultCADir()
	}
	if path == "~" || len(path) > 1 && path[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	// Logs go to stderr so stdout stays free for machine-readable output.
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
