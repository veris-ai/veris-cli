package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	patchBundledCAs := fs.Bool("patch-bundled-cas", false,
		"experimental: find known SDK-bundled CA files (certifi, botocore, "+
			"stripe, ...) in the image and your -v mounts, and over-mount each "+
			"with a copy that also carries the Veris CA (with --image)")
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
	var capAdd []string
	fs.Func("cap-add",
		"Linux `capability` to hand back to your container, repeatable (with "+
			"--image). The workload runs with every capability dropped; an "+
			"entrypoint that switches users (su, gosu, service) needs SETUID and "+
			"SETGID, or build the image to run as that USER. ALL and SYS_ADMIN "+
			"are refused",
		func(v string) error {
			c, err := parseCapability(v)
			if err != nil {
				return err
			}
			capAdd = append(capAdd, c)
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

	if err := parseFlags(fs, args); err != nil {
		return err
	}
	// An empty argv is only a mistake WITHOUT --image. With one, it is the
	// ordinary case: docker runs the image's own ENTRYPOINT and CMD, which is
	// what an application image is built to do and what its author tested. A
	// command supplied here overrides them, exactly as `docker run` does.
	argv := fs.Args()
	if len(argv) == 0 && *image == "" {
		return errors.New("run needs a command: veris run [--sandbox <id>] -- <cmd> [args...]\n" +
			"or name an image and let its own entrypoint run: veris run --image <image>")
	}
	// Ingress lives in the proxy, and without --image the proxy is this
	// process -- which does not open one. Refusing beats accepting a callback
	// assertion that would never be evaluated.
	if *image == "" {
		// Most rows share the callback-path reason; --patch-bundled-cas rides
		// the same table with its own, because its reason is the image's
		// filesystem, not the callback path. A slice, not a map, so the flag
		// reported first is deterministic.
		callbackWhy := func(flag string) string {
			return "the callback path is opened by the proxy container. Use " +
				"`veris serve " + flag + " ...` for a local proxy"
		}
		imageOnly := []struct {
			flag string
			set  bool
			why  string
		}{
			{"--expose", *expose > 0, callbackWhy("--expose")},
			{"--environment", *environment != "", callbackWhy("--environment")},
			{"--require-callback", len(callbackReqs) > 0, callbackWhy("--require-callback")},
			{"--ttl-minutes", *ttlMinutes > 0, callbackWhy("--ttl-minutes")},
			{"--expose-token", *exposeToken != "", callbackWhy("--expose-token")},
			{"--expose-hostname", *exposeHostname != "", callbackWhy("--expose-hostname")},
			{"--patch-bundled-cas", *patchBundledCAs,
				"the reason is the image's filesystem, not the callback path: it " +
					"patches CA files inside a container image"},
			{"--cap-add", len(capAdd) > 0,
				"capabilities belong to a container, and without --image the " +
					"command runs as a local child process with whatever it " +
					"inherits"},
		}
		for _, f := range imageOnly {
			if f.set {
				return fmt.Errorf("%s needs --image: %s", f.flag, f.why)
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
			Sandbox:         sandboxOrEnvironment(sources, *environment),
			APIBase:         firstNonEmpty(sources.APIBase, os.Getenv("VERIS_API_BASE")),
			APIKey:          firstNonEmpty(sources.APIKey, os.Getenv("VERIS_API_KEY")),
			Config:          sources.File,
			Volumes:         volumes,
			EnvVars:         envVars,
			CapAdd:          capAdd,
			Workdir:         *workdir,
			Argv:            argv,
			Requirements:    reqs,
			Quiet:           *quiet,
			KeepProxy:       *keepProxy,
			Expose:          *expose,
			ExposeHost:      *exposeHost,
			TunnelToken:     firstNonEmpty(*exposeToken, os.Getenv("VERIS_TUNNEL_TOKEN")),
			TunnelHostname:  *exposeHostname,
			Environment:     *environment,
			TTLMinutes:      *ttlMinutes,
			CallbackReqs:    callbackReqs,
			PatchBundledCAs: *patchBundledCAs,
			ProxyUID:        *proxyUID,
			Strict:          *strict,
			LogLevel:        *logLevel,
			LogFormat:       *logFormat,
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
	// Trust failures are recorded only by transparent listeners, which this
	// local tier never opens (goproxy's CONNECT loop discards its own MITM
	// handshake error) -- so today this can fire only via the containerised
	// path, and is wired here so both tiers share one verdict when that gap
	// closes.
	unmet := unmetRequirements(reqs, receipt)
	// The host tier cannot patch bundles (no image to scan), so the advice
	// points at the container tier.
	fatal := reportUnmetAndTrust(os.Stderr, unmet, receipt, trustAdvice{})
	if shutErr != nil {
		fmt.Fprintf(os.Stderr,
			"veris: the proxy did not shut down cleanly (%v), so this receipt may be short\n",
			shutErr)
	}

	// A failing command is the command's own verdict and keeps its exit code: a
	// harness reading it should see what it always saw.
	if status != 0 {
		return exitCode(status)
	}
	if fatal {
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
		PassThrough:         passThrough(cfg),
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
				"veris: %s ignored the signal for %s; killing it\n", argv[0], childGrace)
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

// environmentReceiptUnmet is the assertion nobody should have to spell out: a
// run that deployed a whole sandbox and then sent it nothing did not test its
// integration, whatever the suite's own exit code says. The environment already
// names the services, so an empty receipt fails by default; explicit
// --require-service flags take over the judgement entirely when given.
func environmentReceiptUnmet(environment string, reqs []requirement, r proxy.Receipt) []string {
	if environment == "" || len(reqs) > 0 || r.Total > 0 {
		return nil
	}
	// Control-plane reads are the harness's own traffic, so they cannot stand
	// in for the suite having called its dependencies -- but a run that made
	// them deserves the sharper message: the pipe worked, the code under test
	// never used it.
	if r.ControlTotal > 0 {
		return []string{fmt.Sprintf(
			"this run deployed a sandbox and sent it only %d /veris/* "+
				"control-plane request(s), no service traffic: either the suite "+
				"never called its dependencies, or its clients failed before "+
				"any request completed (a TLS trust diagnostic above names the "+
				"host if a client refused the interception CA)", r.ControlTotal)}
	}
	return []string{
		"this run deployed a sandbox and sent it nothing: either the suite " +
			"never called its dependencies, or interception missed them. " +
			"--require-service <name> sharpens this to a per-service assertion",
	}
}

// trustAdvice is what the run knew that the receipt does not: which tier ran,
// whether --patch-bundled-cas was on, and which CA-bundle-shaped files the
// scan saw but does not know. Together they let a refusal diagnostic name the
// exact next action instead of a README section -- the difference between an
// agent correcting itself in one retry and an agent exploring.
type trustAdvice struct {
	ContainerTier  bool
	PatchEnabled   bool
	UnknownBundles []string
}

// nextStep is the single prescriptive tail appended to a refusal diagnostic.
// Every branch ends in an action or a stop -- never only a pointer -- because
// the reader is often an agent deciding what to run next. The three states:
// flag off (turn it on), flag on with unknown candidates (over-mount the
// named file), flag on with none (real pinning; no retry will change it).
func (a trustAdvice) nextStep() string {
	switch {
	case !a.ContainerTier:
		return "Next: run containerised (run --image ...) with --patch-bundled-cas"
	case !a.PatchEnabled:
		return "Next: re-run with --patch-bundled-cas"
	case len(a.UnknownBundles) > 0:
		return fmt.Sprintf(
			"--patch-bundled-cas already covered every known bundle, so this "+
				"client trusts something else. CA-bundle-shaped file(s) the scan "+
				"does not know: %s. Next: append the published Veris CA to a copy "+
				"of one and bind it over that exact path (-v copy:/path:ro)",
			strings.Join(a.UnknownBundles, ", "))
	default:
		return "--patch-bundled-cas already covered every known bundle and no " +
			"other CA-bundle-shaped file exists in the image or -v mounts: this " +
			"is likely real certificate pinning (SPKI or fingerprint), which no " +
			"added root can satisfy. Stop and report it; retrying will not change it"
	}
}

// siblingNote covers the topology the workload-centric advice cannot: the
// refusing client lives in a container the run did not start -- a compose
// service joining the proxy's network namespace -- which shares the kernel
// redirect but never receives the trust handoff (no env-file, no overlays).
// nango-server was exactly this: routing worked, every vendor call died as
// SELF_SIGNED_CERT_IN_CHAIN, and the workload container looked healthy.
// Container tier only; the host tier has no sibling containers to warn about.
func (a trustAdvice) siblingNote() string {
	if !a.ContainerTier {
		return ""
	}
	return " (A sibling container this run did not start -- e.g. a compose " +
		"service sharing the proxy's network -- never receives the trust " +
		"handoff: start it with the run's veris.env as an env-file, or mount " +
		"the proxy's share and point its CA variable at " +
		"/veris-share/veris-ca.pem; `docker inspect` the proxy container's " +
		"/veris-share mount for the host path.)"
}

// trustFailureDiagnostics turns the receipt's per-host TLS trust failures
// into printed diagnostics. fatal marks the case that must fail the run: a
// mapped host whose minted certificate a client refused outright, and which
// completed no vendor-surface request, never exercised its integration --
// whatever the command's own exit code said. A host that refused handshakes
// AND completed requests is reported but not fatal: two clients disagreed
// about the CA, and the completing one may be the harness rather than the
// code under test, so the line must print either way. An aborted-only host is
// reported in probabilistic wording -- an EOF is consistent with a refusal
// but never proof of one -- and turns fatal only when the whole receipt is
// empty: handshakes that died after leaf selection beside a sandbox that
// received nothing is a run that proved nothing, and Node (nango-server's
// runtime) closes without an alert on exactly this path, so leaving it
// advisory let every such run exit green.
func trustFailureDiagnostics(r proxy.Receipt, advice trustAdvice) (msgs []string, fatal bool) {
	// Receipt hosts arrive as the client wrote them; trust failures are keyed
	// by lowercased SNI. Fold case here, or a completed request with a
	// mixed-case Host header would fail to suppress the fatal verdict.
	// Vendor-surface counts only: a /veris/* control-plane read is the
	// harness talking to the sandbox, and letting it vouch for a host is how
	// a fully TLS-broken SDK once passed with everything looking healthy.
	completed := make(map[string]int64, len(r.ByHost))
	for host, n := range r.ByHost {
		completed[strings.ToLower(host)] += n
	}
	for _, f := range r.TrustFailures {
		if !f.Mapped {
			continue
		}
		if done := completed[f.Host]; done > 0 {
			// Exercised AND refused: mixed traffic, so the command's verdict
			// stands, but silence here would hide the one clue that a second
			// client in the run -- an SDK with its own CA bundle -- never
			// reached the sandbox at all.
			if f.Rejected > 0 {
				msgs = append(msgs, fmt.Sprintf(
					"%s: %d TLS handshake(s) rejected (%s) after the certificate "+
						"was minted, even though %d request(s) completed -- another "+
						"client in this run refused the interception CA, likely an "+
						"SDK-bundled CA bundle; its traffic never reached the "+
						"sandbox. %s%s",
					f.Host, f.Rejected, dominantReason(f.Reasons), done,
					advice.nextStep(), advice.siblingNote()))
			}
			continue
		}
		switch {
		case f.Rejected > 0:
			fatal = true
			msgs = append(msgs, fmt.Sprintf(
				"%s: %d TLS handshake(s) rejected (%s) after the certificate was "+
					"minted; 0 requests completed -- the client refused the "+
					"interception CA. %s%s",
				f.Host, f.Rejected, dominantReason(f.Reasons),
				advice.nextStep(), advice.siblingNote()))
		case f.Aborted > 0:
			// Fatal only beside an empty receipt: with ANY vendor-surface
			// traffic flowing, an EOF stays advisory, but dead handshakes as
			// the run's ONLY TLS story mean nothing was proved.
			empty := r.Total == 0
			if empty {
				fatal = true
			}
			verdict := "If the workload's error looks like a connection or " +
				"certificate failure, treat it as a refusal: "
			if empty {
				verdict = "With the sandbox receiving nothing at all, this run " +
					"proved nothing, so it fails rather than passing on silence. "
			}
			msgs = append(msgs, fmt.Sprintf(
				"%s: %d TLS handshake(s) ended after the certificate was minted; "+
					"0 requests completed -- CA rejection or certificate pinning is "+
					"likely, but the connection closed without a TLS alert, so this "+
					"is not certain. %s%s%s",
				f.Host, f.Aborted, verdict, advice.nextStep(), advice.siblingNote()))
		}
	}
	return msgs, fatal
}

// dominantReason names the most frequent certificate alert, so the diagnostic
// leads with what most of the failed handshakes actually said. It is only
// called when at least one rejection was recorded, so the map is never empty;
// a name tie breaks alphabetically, so the choice reads the same every run.
func dominantReason(reasons map[string]int64) string {
	var best string
	var bestN int64
	for name, n := range reasons {
		if n > bestN || (n == bestN && name < best) {
			best, bestN = name, n
		}
	}
	return best
}

// reportUnmetAndTrust prints the run's two failure groups -- the unmet
// requirements, then the TLS trust diagnostics -- and returns whether either
// is fatal to the verdict. One place so both tiers report in the same order.
func reportUnmetAndTrust(w io.Writer, unmet []string, receipt proxy.Receipt, advice trustAdvice) (fatal bool) {
	for _, u := range unmet {
		fmt.Fprintf(w, "veris: %s\n", u)
	}
	trustMsgs, trustFatal := trustFailureDiagnostics(receipt, advice)
	for _, m := range trustMsgs {
		fmt.Fprintf(w, "veris: %s\n", m)
	}
	return len(unmet) > 0 || trustFatal
}

func unmetRequirements(reqs []requirement, r proxy.Receipt) []string {
	var out []string
	for _, req := range reqs {
		got := r.ByService[req.name]
		if req.kind == "host" {
			got = r.ByHost[req.name]
		}
		if got >= req.count {
			continue
		}
		msg := fmt.Sprintf(
			"the run required %s %s at least %d time(s) but the sandbox saw it %d time(s)",
			req.kind, req.name, req.count, got)
		// Control-plane reads used to count here, and a harness seeding its
		// world could satisfy the requirement while every SDK call failed.
		// Name what was excluded, or the count reads as the traffic vanishing.
		if req.kind == "service" {
			if n := r.ByServiceControl[req.name]; n > 0 {
				msg += fmt.Sprintf(
					" (%d /veris/* control-plane request(s) are not counted as "+
						"service traffic)", n)
			}
		}
		out = append(out, msg)
	}
	return out
}

// printReceipt is the run's proof of work: what the code under test actually
// sent to the sandbox, as opposed to what it was configured to send.
// Control-plane traffic prints on its own lines: it proves the harness could
// reach the sandbox, never that the code under test did.
func printReceipt(w *os.File, r proxy.Receipt) {
	if r.Total == 0 {
		if r.ControlTotal > 0 {
			fmt.Fprintf(w,
				"veris: the sandbox received no service traffic from this "+
					"run -- only %d /veris/* control-plane request(s).\n",
				r.ControlTotal)
			return
		}
		fmt.Fprintln(w, "veris: the sandbox received nothing from this run.")
		return
	}
	services := make([]string, 0, len(r.ByService))
	for name := range r.ByService {
		services = append(services, name)
	}
	sort.Strings(services)

	fmt.Fprintf(w, "veris: the sandbox received %d request(s):\n", r.Total)
	for _, name := range services {
		fmt.Fprintf(w, "  %-28s %d\n", name, r.ByService[name])
	}
	if r.ControlTotal > 0 {
		fmt.Fprintf(w, "  plus %d /veris/* control-plane request(s), not counted above\n",
			r.ControlTotal)
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
// Public and anonymously pullable, because of who pulls it: our other images
// are pulled by our cluster, and this one is pulled by whoever is testing, on
// their laptop or in their CI. Requiring a registry login before the first run
// is a wall in front of the one command this tool exists for.
const defaultProxyImage = "ghcr.io/veris-ai/veris-proxy:runner"

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
