package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/veris-ai/veris-proxy/internal/ca"
	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/discovery"
	"github.com/veris-ai/veris-proxy/internal/procgroup"
	"github.com/veris-ai/veris-proxy/internal/proxy"
	"github.com/veris-ai/veris-proxy/internal/trust"
)

// run is the whole product in one command: start the proxy, hand the child the
// environment that routes it here, run it, and report what actually reached the
// sandbox.
//
// It exists for work that is not containerised. Doing the same thing by hand --
// start a proxy, eval an environment, run the tests -- leaves three failure
// modes open: a stale proxy from an earlier run, an eval that silently did
// nothing, and a suite that stopped calling its dependency and passed anyway.
// This closes all three.

// Exit codes above the child's own. Distinct so a harness can tell "your tests
// failed" from "your tests never touched the sandbox".
const (
	exitRequirementUnmet = 3
	exitIndeterminate    = 4
)

type requirement struct {
	kind  string // "service" or "host"
	name  string
	count int64
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var sources configSources
	bindConfigFlags(fs, &sources)
	listen := fs.String("listen", "", "override the listen address (use :0 to pick a free port)")
	logLevel := fs.String("log-level", "warn", "debug, info, warn or error")
	logFormat := fs.String("log-format", "text", "text or json")
	strict := fs.Bool("strict", false, "block unmapped hosts instead of letting them reach the real internet")
	quiet := fs.Bool("quiet", false, "do not print the receipt summary")
	javaStore := fs.String("java-truststore", "", "path to a JKS truststore containing the Veris CA")
	javaPass := fs.String("java-truststore-pass", "changeit", "password for the JKS truststore")
	caDir := fs.String("ca-dir", "", "directory holding the CA (overrides the config's ca_dir)")
	image := fs.String("image", "",
		"run the command in this container `image` instead of locally, with the "+
			"proxy in its own container beside it")
	proxyImage := fs.String("proxy-image", defaultProxyImage,
		"the proxy's own `image`, for --image")
	workdir := fs.String("w", "", "working `directory` inside the image (with --image)")
	var callbackReqs []requirement
	expose := fs.Int("expose", 0,
		"publish this local `port` at a public https URL so the sandbox can "+
			"deliver callbacks to it (with --image, the port your image listens on)")
	exposeHost := fs.String("expose-host", "",
		"`host` the exposed port is on; defaults to loopback, which is right when "+
			"your image shares the proxy's network namespace")
	exposeToken := fs.String("expose-token", "",
		"cloudflared named-tunnel `token` (defaults to $VERIS_TUNNEL_TOKEN)")
	exposeHostname := fs.String("expose-hostname", "",
		"public `hostname` a named tunnel serves; required with --expose-token")
	environment := fs.String("environment", "",
		"deploy a fresh sandbox from this environment `id` and delete it after, "+
			"instead of attaching to an existing --sandbox")
	ttlMinutes := fs.Int("ttl-minutes", 0,
		"how long a sandbox created by --environment may live if teardown never runs")
	fs.Func("require-callback",
		"fail unless your app received a callback on this path[:count] (* for any path)",
		func(v string) error {
			r, err := parseRequirement("callback", v)
			if err != nil {
				return err
			}
			callbackReqs = append(callbackReqs, r)
			return nil
		})
	proxyUID := fs.Int("proxy-uid", defaultProxyUID,
		"uid the proxy drops to, and the one the redirect exempts; your image "+
			"must not run as it")
	keepProxy := fs.Bool("keep-proxy", false,
		"leave the proxy container running afterwards, to inspect it")
	var volumes, envVars []string
	fs.Func("v", "bind mount, repeatable (with --image)", func(v string) error {
		volumes = append(volumes, v)
		return nil
	})
	fs.Func("e", "environment variable, repeatable (with --image)", func(v string) error {
		envVars = append(envVars, v)
		return nil
	})

	var reqs []requirement
	fs.Func("require-service", "fail unless this service was called (name[:count])", func(v string) error {
		r, err := parseRequirement("service", v)
		if err != nil {
			return err
		}
		reqs = append(reqs, r)
		return nil
	})
	fs.Func("require-host", "fail unless this hostname was intercepted (host[:count])", func(v string) error {
		r, err := parseRequirement("host", v)
		if err != nil {
			return err
		}
		reqs = append(reqs, r)
		return nil
	})

	if err := fs.Parse(args); err != nil {
		return err
	}
	// An empty argv is only a mistake WITHOUT --image. With one, it is the
	// ordinary case: docker runs the image's own ENTRYPOINT and CMD, which is
	// what an application image is built to do and what its author tested. A
	// command supplied here overrides them, exactly as `docker run` does.
	argv := fs.Args()
	if len(argv) == 0 && *image == "" {
		return errors.New("run needs a command: veris-proxy run [--sandbox <id>] -- <cmd> [args...]\n" +
			"or name an image and let its own entrypoint run: veris-proxy run --image <image>")
	}
	// Ingress lives in the proxy, and without --image the proxy is this
	// process -- which does not open one. Refusing beats accepting a callback
	// assertion that would never be evaluated.
	if *image == "" {
		for name, set := range map[string]bool{
			"--expose": *expose > 0, "--environment": *environment != "",
			"--require-callback": len(callbackReqs) > 0, "--ttl-minutes": *ttlMinutes > 0,
			"--expose-token": *exposeToken != "", "--expose-hostname": *exposeHostname != "",
		} {
			if set {
				return fmt.Errorf(
					"%s needs --image: the callback path is opened by the proxy "+
						"container. Use `veris-proxy serve %s ...` for a local proxy",
					name, name)
			}
		}
	}

	if len(callbackReqs) > 0 && *expose <= 0 {
		return errors.New(
			"--require-callback asserts what your app received, and nothing can " +
				"arrive without --expose <port>. Add it, or drop the requirement")
	}

	// The container receives one routing target. An inherited VERIS_SANDBOX_ID
	// alongside --environment makes serve refuse to start; a --config is worse,
	// since the entrypoint prefers the environment and the requested config is
	// silently ignored.
	if *environment != "" {
		if sources.File != "" {
			return errors.New(
				"--environment deploys a sandbox and --config routes at whatever the " +
					"file names. Pick one")
		}
		if sources.Sandbox != "" {
			return errors.New(
				"--environment deploys a sandbox of its own and --sandbox attaches " +
					"to one that exists. Pick one")
		}
	}

	// --image hands the whole arrangement to docker: the proxy runs in its own
	// container and the command in another sharing its network namespace, so
	// the image under test needs no capability, no iptables and no change.
	// Resolution stays here rather than inside the container only when a config
	// FILE was named, which the container cannot read from this filesystem.
	if *image != "" {
		// Flags that describe a proxy in THIS process have no meaning when the
		// proxy is in another container. Ignoring them silently is how someone
		// ends up believing --ca-dir took effect.
		for name, set := range map[string]bool{
			"--listen": *listen != "", "--ca-dir": *caDir != "",
			"--java-truststore": *javaStore != "",
		} {
			if set {
				return fmt.Errorf("%s applies to a local proxy, and --image puts "+
					"the proxy in its own container. Drop it, or drop --image", name)
			}
		}
		return runContainerised(dockerRun{
			Image:      *image,
			ProxyImage: *proxyImage,
			// Precedence has to hold here too: an explicit --config must not
			// lose to a VERIS_SANDBOX_ID left in the environment, which would
			// silently route the run at a different sandbox.
			Sandbox:        sandboxOrEnvironment(sources, *environment),
			APIBase:        firstNonEmpty(sources.APIBase, os.Getenv("VERIS_API_BASE")),
			APIKey:         firstNonEmpty(sources.APIKey, os.Getenv("VERIS_API_KEY")),
			Config:         sources.File,
			Volumes:        volumes,
			EnvVars:        envVars,
			Workdir:        *workdir,
			Argv:           argv,
			Requirements:   reqs,
			Quiet:          *quiet,
			KeepProxy:      *keepProxy,
			Expose:         *expose,
			ExposeHost:     *exposeHost,
			TunnelToken:    firstNonEmpty(*exposeToken, os.Getenv("VERIS_TUNNEL_TOKEN")),
			TunnelHostname: *exposeHostname,
			Environment:    *environment,
			TTLMinutes:     *ttlMinutes,
			CallbackReqs:   callbackReqs,
			ProxyUID:       *proxyUID,
			Strict:         *strict,
			LogLevel:       *logLevel,
		})
	}

	cfg, source, err := resolveConfig(sources)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *strict {
		cfg.Mode = config.ModeStrict
	}
	if *caDir != "" {
		cfg.CADir = *caDir
	}
	// Port 0 is how a caller asks for a free port; the real one is read back
	// from the bound listener below.
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := newLogger(*logLevel, *logFormat)
	log.Debug("configuration resolved", "from", source)
	authority, err := ca.Load(expand(cfg.CADir))
	if err != nil {
		return err
	}

	// No transparent listeners here: `run` never installed a redirect, so they
	// would bind and receive nothing -- the state this binary refuses
	// everywhere else. The kernel tier is `serve --transparent`, or `--image`.
	srv := proxy.New(cfg, authority, log, version)

	// Every listener is bound before the child starts, so the child cannot
	// race the proxy and reach the real internet on its first request.
	running, err := srv.Start(proxy.ListenOptions{})
	if err != nil {
		return err
	}
	announce(log, running, cfg, authority)

	status, runErr := supervise(running, cfg, authority, argv, javaOptions{
		store: *javaStore,
		pass:  *javaPass,
	})

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	shutErr := running.Shutdown(shutCtx)

	if runErr != nil {
		// The command never ran, so there is no receipt to report -- printing
		// "the sandbox received nothing" here would read as a finding about
		// the run rather than what it is, a failure to start.
		return runErr
	}

	receipt := running.Receipt()
	if !*quiet {
		printReceipt(os.Stderr, receipt)
	}

	// Everything that went wrong is REPORTED, whatever ends up owning the exit
	// code. Only one status can be returned, so a shutdown failure that loses
	// the tie-break would otherwise vanish -- and it is the one that says the
	// receipt above may be incomplete.
	unmet := unmetRequirements(reqs, receipt)
	for _, u := range unmet {
		fmt.Fprintf(os.Stderr, "veris-proxy: %s\n", u)
	}
	if shutErr != nil {
		fmt.Fprintf(os.Stderr,
			"veris-proxy: the proxy did not shut down cleanly (%v), so this receipt may be short\n",
			shutErr)
	}

	// A failing command is the command's own verdict and keeps its exit code: a
	// harness reading it should see what it always saw.
	if status != 0 {
		return exitCode(status)
	}
	if len(unmet) > 0 {
		return exitCode(exitRequirementUnmet)
	}
	if shutErr != nil {
		return exitCode(exitIndeterminate)
	}
	return nil
}

type javaOptions struct{ store, pass string }

// supervise runs the child with the interception environment and returns its
// exit status. A non-nil error means the child could not be run at all, which
// is different from the child running and failing.
func supervise(running *proxy.Running, cfg *config.Config, authority *ca.CA,
	argv []string, java javaOptions,
) (int, error) {
	proxyURL := running.ProxyURL()

	material, err := publishTrust("", authority)
	if err != nil {
		return 0, fmt.Errorf("publish the trust material: %w", err)
	}
	if java.store == "" {
		// A JDK-derived store is preferred where there is a JDK: it holds
		// exactly the roots that JVM would have had, rather than the host's.
		java.store = firstNonEmpty(javaTrustStore(authority), material.JKSPath)
	}

	env := mergeEnv(os.Environ(), trust.Build(trust.Options{
		ProxyURL:            proxyURL,
		CACertPath:          material.CertPath,
		CABundlePath:        material.BundlePath,
		JavaTrustStore:      java.store,
		JavaTrustStorePass:  java.pass,
		SandboxID:           cfg.SandboxID,
		CanaryToken:         cfg.CanaryToken,
		NoProxy:             cfg.AllowPassthrough,
		NodeAcceptsEnvProxy: nodeAcceptsEnvProxy(),
	}))

	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // running the user's own command is the point
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	procgroup.Isolate(cmd)

	// Ctrl-C reaches this process, not the child's group, so forward it: a test
	// runner's own children would otherwise survive the interrupt. Subscribing
	// BEFORE the child starts matters -- a signal arriving in the gap would
	// take the default action here and leave the child orphaned in its own
	// process group with nobody left to stop it.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("cannot run %s: %w", argv[0], err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// After the first signal the child gets childGrace to exit on its own. A
	// child that traps and ignores SIGTERM must not be able to hold the proxy
	// open forever, which hangs a CI job rather than failing it.
	var grace <-chan time.Time
	forwarded := 0
	for {
		select {
		case sig := <-sigs:
			forwarded++
			if forwarded > 1 {
				// The developer asked twice. Stop being polite.
				procgroup.Terminate(cmd, syscall.SIGKILL)
				continue
			}
			procgroup.Terminate(cmd, sig)
			grace = time.After(childGrace)
		case <-grace:
			fmt.Fprintf(os.Stderr,
				"veris-proxy: %s ignored the signal for %s; killing it\n", argv[0], childGrace)
			procgroup.Terminate(cmd, syscall.SIGKILL)
			grace = nil
		case err := <-done:
			return procgroup.WaitStatus(cmd, err)
		}
	}
}

// childGrace is how long a signalled command has to exit before it is killed.
const childGrace = 10 * time.Second

// mergeEnv layers the interception variables over the inherited environment.
// Appending duplicate entries would work on Linux but is unspecified, and an
// Append variable such as NODE_OPTIONS must extend what the developer already
// set rather than replace it.
func mergeEnv(base []string, vars []trust.Var) []string {
	index := make(map[string]int, len(base))
	out := append([]string{}, base...)
	for i, entry := range out {
		if name, _, ok := strings.Cut(entry, "="); ok {
			index[name] = i
		}
	}
	for _, v := range vars {
		value := v.Value
		if i, exists := index[v.Name]; exists {
			if v.Append {
				if prev := strings.TrimPrefix(out[i], v.Name+"="); prev != "" {
					value = prev + " " + v.Value
				}
			}
			out[i] = v.Name + "=" + value
			continue
		}
		index[v.Name] = len(out)
		out = append(out, v.Name+"="+value)
	}
	return out
}

func parseRequirement(kind, raw string) (requirement, error) {
	name, countStr, hasCount := strings.Cut(strings.TrimSpace(raw), ":")
	if name == "" {
		return requirement{}, fmt.Errorf("--require-%s needs a name", kind)
	}
	count := int64(1)
	if hasCount {
		n, err := strconv.ParseInt(countStr, 10, 64)
		if err != nil || n < 1 {
			return requirement{}, fmt.Errorf("--require-%s %q: %q is not a positive count", kind, raw, countStr)
		}
		count = n
	}
	return requirement{kind: kind, name: name, count: count}, nil
}

func unmetRequirements(reqs []requirement, r proxy.Receipt) []string {
	var out []string
	for _, req := range reqs {
		got := r.ByService[req.name]
		if req.kind == "host" {
			got = r.ByHost[req.name]
		}
		if got < req.count {
			out = append(out, fmt.Sprintf(
				"the run required %s %s at least %d time(s) but the sandbox saw it %d time(s)",
				req.kind, req.name, req.count, got))
		}
	}
	return out
}

// printReceipt is the run's proof of work: what the code under test actually
// sent to the sandbox, as opposed to what it was configured to send.
func printReceipt(w *os.File, r proxy.Receipt) {
	if r.Total == 0 {
		fmt.Fprintln(w, "veris-proxy: the sandbox received nothing from this run.")
		return
	}
	services := make([]string, 0, len(r.ByService))
	for name := range r.ByService {
		services = append(services, name)
	}
	sort.Strings(services)

	fmt.Fprintf(w, "veris-proxy: the sandbox received %d request(s):\n", r.Total)
	for _, name := range services {
		fmt.Fprintf(w, "  %-28s %d\n", name, r.ByService[name])
	}
}

// javaTrustStore returns a JKS built from the local JDK's own cacerts with the
// Veris CA added, or "" when there is no JDK here to copy from.
//
// Adding to a COPY of the JDK's cacerts, never replacing it: a truststore
// holding only our CA breaks every other TLS connection in the JVM. Callers
// fall back to the published truststore, which is the same idea built from the
// host's roots rather than a JDK's.
func javaTrustStore(authority *ca.CA) string {
	candidate := filepath.Join(authority.Dir(), trust.DefaultJavaTrustStoreName)
	if fileExists(candidate) {
		return candidate
	}
	if _, err := trust.BuildJavaTrustStore("", authority.CertPath(), candidate, trustStorePassword); err != nil {
		// No JDK here, or no cacerts to copy. Nothing to do: a non-Java
		// command does not care, and a Java one gets the coverage warning.
		return ""
	}
	return candidate
}

// defaultProxyImage is the proxy's own runner image -- NOT the image under
// test, which has no default and never should.
//
// Its own repository, holding this image and nothing else, because of who pulls
// it: the rest of our images are pulled by the cluster, and this one is pulled
// by whoever is testing. Access is authenticated, so a first run wants
// `gcloud auth configure-docker us-central1-docker.pkg.dev`.
const defaultProxyImage = "us-central1-docker.pkg.dev/veris-ai-dev/" +
	"svc-sandbox-proxy-dev/veris-proxy:runner"

// sandboxOrEnvironment gives the container exactly one routing target.
func sandboxOrEnvironment(src configSources, environment string) string {
	if environment != "" {
		return ""
	}
	return sandboxForContainer(src)
}

// sandboxForContainer mirrors resolveConfig's precedence for the container
// path, where the resolution happens inside the proxy container rather than
// here: an explicit file wins, and only then the flag or the environment.
func sandboxForContainer(src configSources) string {
	if src.File != "" {
		return ""
	}
	return firstNonEmpty(src.Sandbox, os.Getenv(discovery.EnvSandboxID))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// exitCode carries a status out of run() without printing anything extra: the
// child's own output is the message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
