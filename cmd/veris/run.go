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

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/ca"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/config"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/procgroup"
	"github.com/veris-ai/veris-cli/internal/proxy"
	"github.com/veris-ai/veris-cli/internal/routes"
	"github.com/veris-ai/veris-cli/internal/trust"
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
			"deliver callbacks to it (with --image, the port your image listens "+
			"on; without, the proxy runs as `veris serve --expose` beside the command)")
	exposeHost := fs.String("expose-host", "",
		"`host` the exposed port is on; defaults to loopback, which is right when "+
			"your image shares the proxy's network namespace")
	exposeToken := fs.String("expose-token", "",
		"cloudflared named-tunnel `token` (defaults to $VERIS_TUNNEL_TOKEN)")
	exposeHostname := fs.String("expose-hostname", "",
		"public `hostname` a named tunnel serves; required with --expose-token; "+
			"configure its Cloudflare service URL as http://"+namedCallbackAddress)
	environment := fs.String("environment", "",
		"deploy a fresh sandbox from this environment `id` and delete it after, "+
			"instead of attaching to an existing --sandbox")
	ttlMinutes := fs.Int("ttl-minutes", 0,
		"how long a sandbox created by --environment may live if teardown never runs")
	session := fs.Bool("session", false,
		"the command is an interactive session you type at (a shell): keep it "+
			"in this terminal's foreground process group and let the terminal "+
			"deliver its signals, instead of isolating it and forwarding them")
	fresh := fs.Bool("fresh", false,
		"deploy a sandbox of the environment on this machine (as veris up does, "+
			"data files included), run, and delete it afterwards: up, run and "+
			"down in one process, for CI")
	keep := fs.Bool("keep", false,
		"leave a --fresh sandbox running afterwards, as this folder's")
	freshTTL := fs.Int("ttl", 0,
		"lifetime in `minutes` of a --fresh sandbox if teardown never runs (config, then 120)")
	receiptPath := fs.String("receipt", "",
		"write the run's receipt as JSON to this `file`: both ledgers and the "+
			"verdict, never on stdout")
	env := fs.String("env", "",
		"environment `name` in .veris/twin.yaml whose run.command and proxy "+
			"settings fill in what this command line leaves out")
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
		"the default for an SDK that bundles its own CA (certifi, botocore, "+
			"stripe, ...): find the known bundled CA files in the image and your "+
			"-v mounts, and over-mount each with a copy that also carries the "+
			"Veris CA (with --image)")
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
	fs.Func("e",
		"environment `variable` for the command as NAME=value, repeatable; one "+
			"named here is never overwritten by a handed env hint (with --image a "+
			"bare NAME passes the host's value, as docker run does)",
		func(v string) error {
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
	// The project answers for what the command line left out, so the daily
	// form is a bare `veris run`: the environment config's run.command and
	// proxy block, the folder's sandbox pointer, and the login's key and
	// plane. Every one of them yields to a flag or an environment variable
	// that says otherwise; the engine below sees the merged result and
	// nothing else changes.
	// A profiles or project file that will not parse is fatal only when the
	// run would read from it: a command line that names its own target and
	// no --env never consulted those files before they existed, and a stray
	// file must not take that away. The warning explains a later "no API
	// key", which the broken profile would otherwise have supplied.
	s, err := runSession(sources, *env, "run")
	if err != nil {
		if *env != "" || *fresh || !explicitTarget(sources, *environment) {
			return err
		}
		fmt.Fprintf(os.Stderr, "veris: warning: %v; the project file and login are not consulted for this run\n", err)
	}
	d := projectDefaults{argv: fs.Args(), reqs: reqs, callbackReqs: callbackReqs,
		expose: *expose, image: *image, strict: *strict}
	var envName, pointer string
	if s != nil {
		if envName, err = d.fill(s, *env, flagsGiven(fs)); err != nil {
			return err
		}
		// A fresh run routes at the sandbox it is about to make, never at
		// the folder's.
		if !*fresh {
			if pointer, err = pointerSandbox(s, sources, *environment); err != nil {
				return err
			}
		}
		sources.Local = pointer
		sources.APIBase = firstNonEmpty(sources.APIBase, s.res.APIBase)
		sources.APIKey = firstNonEmpty(sources.APIKey, s.res.APIKey)
	}
	// `--environment "$VERIS_ENVIRONMENT_ID"` in a shell where that is unset
	// hands the flag nothing, and nothing is not "no flag": every check below
	// reads an empty --environment as absent, so the run would route at the
	// folder's sandbox in the host tier, and in the container tier start a
	// proxy with no target and fail inside it. Said here, once, as itself.
	if flagsGiven(fs)["environment"] && *environment == "" {
		return errors.New(
			"--environment is empty, so there is no environment to deploy from: " +
				"pass its id (an unset $VERIS_ENVIRONMENT_ID expands to nothing)")
	}
	argv, reqs, callbackReqs := d.argv, d.reqs, d.callbackReqs
	*expose, *image, *strict = d.expose, d.image, d.strict
	// An empty argv is only a mistake WITHOUT --image. With one, it is the
	// ordinary case: docker runs the image's own ENTRYPOINT and CMD, which is
	// what an application image is built to do and what its author tested. A
	// command supplied here overrides them, exactly as `docker run` does.
	if len(argv) == 0 && *image == "" {
		if envName != "" {
			return fmt.Errorf("run needs a command (none configured for '%s'): "+
				"pass one after --, or record one as run.command in %s "+
				"(veris env create --command writes it)", envName, s.res.Project.Path)
		}
		return errors.New("run needs a command: veris run [--sandbox <id>] -- <cmd> [args...]\n" +
			"or name an image and let its own entrypoint run: veris run --image <image>")
	}
	// --fresh deploys the sandbox the run routes at, so nothing else may name
	// one: the same one-target rule --environment states below, from the
	// other side. It is checked first because --environment alone is an
	// image-only flag, and "needs --image" would hide the real conflict.
	if *fresh {
		switch {
		case sources.File != "":
			return errors.New(
				"--fresh deploys a sandbox and --config routes at whatever the " +
					"file names. Pick one")
		case sources.Sandbox != "":
			return errors.New(
				"--fresh deploys a sandbox of its own and --sandbox attaches to " +
					"one that exists. Pick one")
		case *environment != "":
			return errors.New(
				"--fresh and --environment both deploy a sandbox: --fresh does it " +
					"here, for either tier, and reads the sandbox's own ledger. " +
					"Drop --environment")
		}
	} else {
		for _, f := range []struct {
			flag string
			set  bool
		}{{"--keep", *keep}, {"--ttl", *freshTTL > 0}} {
			if f.set {
				return fmt.Errorf("%s only applies to a sandbox --fresh deploys", f.flag)
			}
		}
	}

	// Without --image the proxy is this process, or -- when a callback path
	// is wanted -- a `veris serve --expose` child beside it (hosttunnel.go).
	// What stays refused is a sandbox lifecycle: the CLI's up and --fresh own
	// deploy and delete in the host tier, and handing the proxy --environment
	// would give the run a second owner of the same sandbox. A slice, not a
	// map, so the flag reported first is deterministic.
	if *image == "" {
		lifecycleWhy := func(flag string) string {
			return "the sandbox's lifecycle is the CLI's in the host tier: " +
				"veris up deploys one for this folder, and run --fresh " +
				"deploys and deletes one around the command. Use " +
				"`veris serve " + flag + " ...` for a local proxy that owns its own"
		}
		imageOnly := []struct {
			flag string
			set  bool
			why  string
		}{
			{"--environment", *environment != "", lifecycleWhy("--environment")},
			{"--ttl-minutes", *ttlMinutes > 0, lifecycleWhy("--ttl-minutes")},
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
		return fmt.Errorf(
			"%s asserts what your app received, and nothing can "+
				"arrive without --expose <port>. Add it, or drop the requirement", d.label("--require-callback"))
	}
	// The tunnel's own rule, checked here so the host tier refuses before a
	// serve child is started for nothing; the container tier's serve says
	// the same inside the container. The token is the flag or the shell's
	// $VERIS_TUNNEL_TOKEN, exactly what serve will be handed below.
	tunnelToken := firstNonEmpty(*exposeToken, os.Getenv("VERIS_TUNNEL_TOKEN"))
	if tunnelToken != "" && *exposeHostname == "" {
		return errors.New(
			"--expose-token names a tunnel that serves a hostname it is configured " +
				"with and announces nothing, so --expose-hostname is required alongside it")
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
				if d.fromFile["--image"] != "" {
					return fmt.Errorf("%s applies to a local proxy, and %s puts "+
						"the proxy in its own container. Drop it, or pass --image '' to run locally", name, d.label("--image"))
				}
				return fmt.Errorf("%s applies to a local proxy, and --image puts "+
					"the proxy in its own container. Drop it, or drop --image", name)
			}
		}
		// The proxy container is handed one routing target, and handed none
		// it starts, finds nothing to route and exits with the runner image's
		// own words -- set VERIS_SANDBOX_ID, or mount a config at
		// /veris/config.json -- which are for someone driving that image by
		// hand, not for this command. The host tier refuses the same run
		// before it does anything (resolveConfig), so this tier refuses here,
		// in this command's words. --fresh is exempt: its sandbox does not
		// exist yet, and it is the target.
		if !*fresh && sources.File == "" && *environment == "" && sandboxForContainer(sources) == "" {
			return fmt.Errorf(
				"nothing to route: pass --sandbox <id> (or set $%s), "+
					"--environment <id> to deploy one for this run, or --config <file>; "+
					"veris up gives this folder a sandbox of its own",
				discovery.EnvSandboxID)
		}
	}

	// Every refusal is behind us: a fresh sandbox is deployed only for a
	// command line that will run. From here every return passes through
	// finishFresh, which deletes it.
	var fr *freshRun
	if *fresh {
		if fr, err = startFresh(context.Background(), s, *env, *freshTTL, *keep, os.Stderr); err != nil {
			return err
		}
		sources.Sandbox, sources.Refresh, sources.Local = fr.sb.ID, true, ""
		sources.APIBase = firstNonEmpty(sources.APIBase, fr.c.Base)
		sources.APIKey = firstNonEmpty(sources.APIKey, fr.c.Key)
	}
	ledgerAPI := ledgerClient(s, sources)

	if *image != "" {
		announcePointer(pointer)
		return finishFresh(fr, runContainerisedProved(dockerRun{
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
			Session:         *session,
			Requirements:    reqs,
			Quiet:           *quiet,
			KeepProxy:       *keepProxy,
			Expose:          *expose,
			ExposeHost:      *exposeHost,
			TunnelToken:     tunnelToken,
			TunnelHostname:  *exposeHostname,
			Environment:     *environment,
			TTLMinutes:      *ttlMinutes,
			CallbackReqs:    callbackReqs,
			PatchBundledCAs: *patchBundledCAs,
			ProxyUID:        *proxyUID,
			Strict:          *strict,
			LogLevel:        *logLevel,
			LogFormat:       *logFormat,
		}, ledgerAPI, callbackReqs, *fresh, *quiet, *receiptPath))
	}

	announcePointer(pointer)
	return finishFresh(fr, runLocal(localRun{
		sources:        sources,
		listen:         *listen,
		caDir:          *caDir,
		strict:         *strict,
		logLevel:       *logLevel,
		logFormat:      *logFormat,
		java:           javaOptions{store: *javaStore, pass: *javaPass},
		argv:           argv,
		userEnv:        envVars,
		reqs:           reqs,
		callbackReqs:   callbackReqs,
		quiet:          *quiet,
		receipt:        *receiptPath,
		client:         ledgerAPI,
		session:        *session,
		fresh:          *fresh,
		expose:         *expose,
		exposeHost:     *exposeHost,
		tunnelToken:    tunnelToken,
		tunnelHostname: *exposeHostname,
	}))
}

// localRun is the host tier's settings once the command line, the project
// and the login have been merged: what runLocal needs and nothing it does
// not.
type localRun struct {
	sources       configSources
	listen, caDir string
	strict        bool
	logLevel      string
	logFormat     string
	java          javaOptions
	argv          []string
	// userEnv is -e as given: variables set for the command, and the names
	// no handed env hint may overwrite.
	userEnv            []string
	reqs, callbackReqs []requirement
	quiet              bool
	// receipt is --receipt PATH, "" for none.
	receipt string
	// client reads the sandbox for the ledger; nil when nothing supplied a
	// key, which the ledger then says.
	client *api.Client
	// fresh is a sandbox this run deployed: an empty ledger fails it.
	fresh bool
	// session is --session: the child is typed at, so it keeps this
	// terminal's foreground process group and its signals.
	session bool
	// expose is the callback direction: the port to publish, 0 for none.
	// With one, the proxy is a `veris serve --expose` child rather than
	// this process (hosttunnel.go), and the rest describe its tunnel.
	expose         int
	exposeHost     string
	tunnelToken    string
	tunnelHostname string
}

// runLocal is the host tier: the proxy in this process, the command as a
// local child, the engine's receipt and the sandbox's ledger afterwards.
// With --expose the proxy is a serve child instead, and the rest is the same.
func runLocal(o localRun) error {
	if o.expose > 0 {
		return runHostExposed(o)
	}
	cfg, source, err := resolveConfig(o.sources)
	if err != nil {
		return err
	}
	if o.listen != "" {
		cfg.Listen = o.listen
	}
	if o.strict {
		cfg.Mode = config.ModeStrict
	}
	if o.caDir != "" {
		cfg.CADir = o.caDir
	}
	// Port 0 is how a caller asks for a free port; the real one is read back
	// from the bound listener below.
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := newLogger(o.logLevel, o.logFormat)
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

	// The sandbox side: the watermark once the engine is live and before the
	// child starts, so nothing the child sends lands below it.
	bg := context.Background()
	p := newProof(bg, ledgerSandbox(o.sources), o.client, o.sources.Overrides)
	p.watermark(bg, os.Stderr, o.quiet)
	started := time.Now()

	// What the engine cannot route it hands over: the config's pass-env
	// entries, minus any the command line set itself.
	handed := withoutUserVars(cfg.PassEnv, o.userEnv)
	announceHandoffs(os.Stderr, handed)
	announceSuppressed(os.Stderr, cfg.PassEnv, o.userEnv)
	status, runErr := supervise(running, cfg, authority, o.argv, o.java, handed, o.userEnv, o.session)
	finished := time.Now()
	// The child is gone; what is left is the after-read and, for a --fresh
	// sandbox, the teardown. A second Ctrl-C in between would take the
	// default action and leave that sandbox alive until its TTL, so signals
	// are held from here until the run returns (teardown holds them again).
	if o.fresh {
		defer holdSignals()()
	}

	shutCtx, cancel := context.WithTimeout(bg, shutdownGrace)
	defer cancel()
	shutErr := running.Shutdown(shutCtx)

	if runErr != nil {
		// The command never ran, so there is no receipt to report -- printing
		// "the sandbox received nothing" here would read as a finding about
		// the run rather than what it is, a failure to start.
		return runErr
	}

	receipt := running.Receipt()
	return o.conclude(p, status, &receipt, nil, nil, shutErr, started, finished)
}

// conclude is the host tier's verdict once the command has exited and the
// proxy is gone: the receipts printed, the requirements judged on the engine
// and then on the sandbox's ledger, the exit code decided, the receipt file
// written. Shared by the in-process proxy and the serve child, so both read
// the same way.
//
// engine is nil when the receipt could not be read (readErr says why); the
// ledger is still judged, since the sandbox's account does not depend on
// ours. inbound is nil when nothing was exposed. shutErr is a proxy that did
// not leave cleanly, which is the one thing that says the receipt above may
// be incomplete.
func (o localRun) conclude(p *proof, status int, engine *proxy.Receipt,
	inbound *proxy.InboundReceipt, readErr, shutErr error, started, finished time.Time,
) error {
	bg := context.Background()
	fatal := false
	if readErr != nil {
		fmt.Fprintf(os.Stderr,
			"veris: could not read the receipt (%v), so what the sandbox "+
				"received is unknown\n", readErr)
	} else {
		if !o.quiet {
			printReceipt(os.Stderr, *engine)
		}
		// Everything that went wrong is REPORTED, whatever ends up owning the
		// exit code. Only one status can be returned, so a shutdown failure
		// that loses the tie-break would otherwise vanish -- and it is the one
		// that says the receipt above may be incomplete.
		// Trust failures are recorded only by transparent listeners, which
		// this local tier never opens (goproxy's CONNECT loop discards its own
		// MITM handshake error) -- so today this can fire only via the
		// containerised path, and is wired here so both tiers share one
		// verdict when that gap closes.
		// The engine judges alone only what the sandbox ledger will not: a
		// service requirement the ledger judges gets one verdict there, with
		// the engine's count merged in.
		unmet := unmetRequirements(p.enginesAlone(o.reqs), *engine)
		if inbound != nil {
			if !o.quiet {
				printInbound(os.Stderr, *inbound)
			}
			unmet = append(unmet, unmetCallbacks(o.callbackReqs, *inbound)...)
		}
		// The host tier cannot patch bundles (no image to scan), so the
		// advice points at the container tier.
		fatal = reportUnmetAndTrust(os.Stderr, unmet, *engine, trustAdvice{})
	}
	// Then the sandbox's own account, judged with the same requirements.
	l, v := p.finish(bg, os.Stderr, engine, o.reqs, o.callbackReqs, o.fresh, o.quiet, int(status))
	if shutErr != nil {
		fmt.Fprintf(os.Stderr,
			"veris: the proxy did not shut down cleanly (%v), so this receipt may be short\n",
			shutErr)
	}

	// An unmet requirement outranks everything, the command's own failure
	// included: a suite that crashed before it reached the sandbox proved
	// nothing about the integration, and 3 says so where the child's code
	// would read as an ordinary red test. Otherwise a failing command keeps
	// its exit code, so a harness sees what it always saw, and only a clean
	// run is downgraded to indeterminate.
	code := 0
	switch {
	case fatal || v.Fatal:
		code = exitRequirementUnmet
	case status != 0:
		code = status
	case readErr != nil || shutErr != nil || v.Indeterminate:
		code = exitIndeterminate
	}
	writeReceipt(os.Stderr, o.receipt, p, l, engine, v.Assertions, started, finished, code)
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// runContainerisedProved is the container tier with the sandbox's ledger
// read around it: the watermark before the containers start, the after-read
// once they are gone. The engine's receipt is read back from the proxy
// container by the container run and handed here, so the assertions are
// judged on both ledgers as the host tier judges them.
func runContainerisedProved(spec dockerRun, client *api.Client, callbackReqs []requirement,
	fresh, quiet bool, receiptPath string,
) error {
	bg := context.Background()
	// The container tier carries no --route: the proxy container derives the
	// routes itself, from the sandbox alone, so the ledger reads it the same
	// way.
	p := newProof(bg, spec.Sandbox, client, nil)
	p.watermark(bg, os.Stderr, quiet)
	started := time.Now()
	// The twins the proxy container cannot route are handed to the workload
	// from here, where the sandbox's description is known, so a runner image
	// need not be current for the handoff to happen; a -e of the user's is
	// their explicit answer and is left alone.
	all := handoffs(p.sandboxServices(), nil, nil)
	spec.Handoff = withoutUserVars(all, spec.EnvVars)
	announceHandoffs(os.Stderr, spec.Handoff)
	announceSuppressed(os.Stderr, all, spec.EnvVars)
	// The container run judges on the engine alone what the ledger will not
	// judge; the rest is judged once, below, with the engine's count merged.
	reqs := spec.Requirements
	spec.Requirements = p.enginesAlone(reqs)
	engine, err := runContainerised(spec)
	finished := time.Now()
	if fresh {
		// As in runLocal: nothing must interrupt the after-read and the
		// teardown of a sandbox this run deployed.
		defer holdSignals()()
	}
	var status exitCode
	if err != nil && !errors.As(err, &status) {
		// The containers never ran: nothing to read after.
		return err
	}
	l, v := p.finish(bg, os.Stderr, engine, reqs, callbackReqs, fresh, quiet, int(status))
	// Same ranking as the host tier: unmet first, then the command's own
	// code, then indeterminate.
	code := int(status)
	switch {
	case v.Fatal:
		code = exitRequirementUnmet
	case code != 0:
	case v.Indeterminate:
		code = exitIndeterminate
	}
	writeReceipt(os.Stderr, receiptPath, p, l, engine, v.Assertions, started, finished, code)
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// notProxied is discovery's rule -- no hostname the engine intercepts -- on
// a service as the control plane describes it to the CLI.
func notProxied(svc api.ServiceInfo, overrides map[string][]routes.Entry) bool {
	return discovery.NotProxied(discovery.Service{
		Name: svc.Name, URL: svc.URL, EnvHint: svc.EnvHint, Routes: svc.Routes,
	}, overrides)
}

// handoffs is what a run hands the command for the sandbox's twins the
// engine does not route: each one's env hint set to its URL, exactly as the
// proxy's own config hands a database DSN over (discovery.ToConfig), so a
// yente or a postgres is reached without the code under test being told
// anything but the variable it already reads. A variable the command line
// set with -e is the user's explicit answer and is never overwritten; a twin
// with no hint has no name to be handed under.
func handoffs(services []api.ServiceInfo, overrides map[string][]routes.Entry, userEnv []string) []config.PassEnvVar {
	set := userSetVars(userEnv)
	var out []config.PassEnvVar
	for _, svc := range services {
		if svc.EnvHint == "" || svc.URL == "" || set[svc.EnvHint] || !notProxied(svc, overrides) {
			continue
		}
		out = append(out, config.PassEnvVar{Name: svc.EnvHint, Value: svc.URL, Service: svc.Name})
	}
	return out
}

// withoutUserVars is vars minus every one the command line set with -e.
func withoutUserVars(vars []config.PassEnvVar, userEnv []string) []config.PassEnvVar {
	set := userSetVars(userEnv)
	var out []config.PassEnvVar
	for _, v := range vars {
		if !set[v.Name] {
			out = append(out, v)
		}
	}
	return out
}

// userSetVars is the set of names -e named, with or without a value.
func userSetVars(userEnv []string) map[string]bool {
	set := make(map[string]bool, len(userEnv))
	for _, e := range userEnv {
		name, _, _ := strings.Cut(e, "=")
		set[name] = true
	}
	return set
}

// handedVars is a handoff in the shape mergeEnv layers over the child's
// environment.
func handedVars(handed []config.PassEnvVar) []trust.Var {
	var out []trust.Var
	for _, h := range handed {
		out = append(out, trust.Var{Name: h.Name, Value: h.Value,
			Reason: h.Service + " is not proxied; its URL is handed over rather than routed"})
	}
	return out
}

// userEnvVars is -e in the shape mergeEnv layers over the child's
// environment. A bare NAME sets nothing here: the host tier's child inherits
// the host's variables anyway, which is what docker's form means.
func userEnvVars(userEnv []string) []trust.Var {
	var out []trust.Var
	for _, e := range userEnv {
		if name, value, ok := strings.Cut(e, "="); ok {
			out = append(out, trust.Var{Name: name, Value: value, Reason: "set with -e"})
		}
	}
	return out
}

// announceHandoffs says on stderr what the command was handed and why:
//
//	veris: yente: not proxied; handed YENTE_API_BASE=https://…
//
// One line per variable, quiet or not, since it is a routing decision the
// receipt cannot show -- the twin's traffic never passes the engine.
func announceHandoffs(w io.Writer, handed []config.PassEnvVar) {
	for _, h := range handed {
		fmt.Fprintf(w, "veris: %s: not proxied; handed %s=%s\n", h.Service, h.Name, h.Value)
	}
}

// announceSuppressed names each twin whose URL was NOT handed over, because
// the command line set the same variable with -e:
//
//	veris: stripe: not proxied, and not handed over: $STRIPE_API_BASE was set with -e
//
// The -e wins deliberately -- it is the user's explicit answer -- but the
// routing note printed earlier says the twin is "handed to the command as
// $NAME", and without this line that promise would be the only thing said
// about a twin whose traffic is now going wherever the user's own value
// points.
func announceSuppressed(w io.Writer, all []config.PassEnvVar, userEnv []string) {
	set := userSetVars(userEnv)
	for _, h := range all {
		if set[h.Name] {
			fmt.Fprintf(w, "veris: %s: not proxied, and not handed over: $%s was set with -e\n",
				h.Service, h.Name)
		}
	}
}

// finishFresh tears down the sandbox a --fresh run made, whatever the run
// returned, and hands the run's own error back: the exit code is the run's,
// and the teardown lines land after its receipt.
func finishFresh(fr *freshRun, err error) error {
	if fr == nil {
		return err
	}
	fr.teardown(context.Background(), os.Stderr, exitFrom(err))
	return err
}

type javaOptions struct{ store, pass string }

// supervise runs the child with the interception environment and returns its
// exit status. A non-nil error means the child could not be run at all, which
// is different from the child running and failing. handed is what the engine
// cannot route and hands over instead (the config's pass-env, minus what -e
// named); userEnv is -e itself, layered last so it wins over everything.
func supervise(running *proxy.Running, cfg *config.Config, authority *ca.CA,
	argv []string, java javaOptions, handed []config.PassEnvVar, userEnv []string,
	session bool,
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
		PassThrough:         passThroughVars(handed),
	}))
	return superviseEnv(mergeEnv(env, userEnvVars(userEnv)), argv, session)
}

// superviseEnv runs the child with env as its whole environment and returns
// its exit status: supervise once the interception environment is known,
// which the serve child hands over as a file rather than building here.
func superviseEnv(env []string, argv []string, session bool) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // running the user's own command is the point
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// A session is a program the developer TYPES AT, and that changes who owns
	// the terminal. Only the foreground process group may read it: a child put
	// in its own group is sent SIGTTIN on its first read and stops, which
	// looks exactly like a shell that started and then froze. So a session
	// stays in this process group, which is already the foreground one, and
	// the terminal delivers its own signals straight to it -- Ctrl-C belongs
	// to the shell's job control, not to us, and leaving is exit or Ctrl-D.
	//
	// Every other child is isolated, because there the opposite is true: a
	// test runner spawns compilers, servers and workers, and signalling only
	// the runner leaves them alive holding the pipe this wait depends on.
	if !session {
		procgroup.Isolate(cmd)
	}

	// Ctrl-C reaches this process, not the child's group, so forward it: a test
	// runner's own children would otherwise survive the interrupt. Subscribing
	// BEFORE the child starts matters -- a signal arriving in the gap would
	// take the default action here and leave the child orphaned in its own
	// process group with nobody left to stop it.
	//
	// In a session the same signal reaches the child directly, since it shares
	// this group. Forwarding it as well would deliver it twice, and taking the
	// default action would kill the proxy out from under a shell the developer
	// is still using, so an interrupt is held here and dropped.
	sigs := make(chan os.Signal, 1)
	if session {
		signal.Notify(sigs, syscall.SIGTERM)
		held := make(chan os.Signal, 1)
		signal.Notify(held, os.Interrupt)
		defer signal.Stop(held)
		go func() {
			for range held {
			}
		}()
	} else {
		signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	}
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
func printReceipt(w io.Writer, r proxy.Receipt) {
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
const defaultProxyImage = "ghcr.io/veris-ai/veris-cli:runner"

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
	return firstNonEmpty(src.Sandbox, os.Getenv(discovery.EnvSandboxID), src.Local)
}

// runSession resolves the project, the login and the folder for run, which
// parses its own flags and so never receives the tree's Context: the one it
// builds carries --api-base, the only global with a bearing on resolution,
// and the --env and --sandbox it parsed.
func runSession(src configSources, envFlag, verb string) (*session, error) {
	ctx := &cli.Context{
		Globals: &cli.Globals{APIBase: src.APIBase},
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Path:    []string{"veris", verb},
	}
	return newSession(ctx, envFlag, src.Sandbox)
}

// projectDefaults are the settings an environment config may answer for when
// the command line did not: the command after --, the requirements, what to
// expose, which image, and strict mode. They are loaded with what the flags
// said and filled in around it.
type projectDefaults struct {
	argv               []string
	reqs, callbackReqs []requirement
	expose             int
	image              string
	strict             bool
	// fromFile names, per flag, the file setting that answered for it
	// ("proxy.expose in .veris/twin.yaml"), so a refusal names something
	// the user can find rather than a flag they never typed.
	fromFile map[string]string
}

// label is how a refusal names flag: the file setting when that is where
// the value came from, the flag itself otherwise.
func (d *projectDefaults) label(flag string) string {
	if from := d.fromFile[flag]; from != "" {
		return from
	}
	return flag
}

// fill takes from the environment config every setting the command line
// left out and returns the environment's name, "" when no config applied.
// given is the set of flag names on the command line: absence is what lets
// the file answer, not emptiness, so `--expose 0` still wins over the file's
// proxy.expose. The config is the --env one when that was passed -- a name
// the project file does not know is refused as requireEnv does -- else
// whatever the session resolved: VERIS_ENV, the folder's `use`, the project
// default. A bare id names no config and fills nothing.
func (d *projectDefaults) fill(s *session, envFlag string, given map[string]bool) (string, error) {
	conf, name := s.res.Env, s.res.EnvName
	if envFlag != "" {
		// --env names a config, and without a project file there is none:
		// saying so beats a run that quietly ignores the flag.
		if s.res.Project == nil {
			_, err := s.requireProject()
			return "", err
		}
		var err error
		if name, _, conf, err = s.requireEnv(); err != nil {
			return "", err
		}
	}
	if conf == nil {
		return "", nil
	}
	d.fromFile = map[string]string{}
	inFile := func(setting string) string {
		return "proxy." + setting + " in " + s.res.Project.Path
	}
	if len(d.argv) == 0 {
		d.argv = conf.Run.Command
	}
	// The file's entries are parsed exactly as the flags are, so a count
	// works the same in both; an entry that would not parse as a flag names
	// the file, since the user never typed it.
	fromFile := func(kind string, raw []string, into *[]requirement) error {
		for _, v := range raw {
			r, err := parseRequirement(kind, v)
			if err != nil {
				return fmt.Errorf("%s: environments.%s.proxy.require_%s: %w",
					s.res.Project.Path, name, kind, err)
			}
			*into = append(*into, r)
		}
		return nil
	}
	if !given["require-service"] {
		if err := fromFile("service", conf.Proxy.RequireService, &d.reqs); err != nil {
			return "", err
		}
	}
	if !given["require-callback"] {
		if err := fromFile("callback", conf.Proxy.RequireCallback, &d.callbackReqs); err != nil {
			return "", err
		}
		if len(conf.Proxy.RequireCallback) > 0 {
			d.fromFile["--require-callback"] = inFile("require_callback")
		}
	}
	if !given["expose"] && conf.Proxy.Expose > 0 {
		d.expose = conf.Proxy.Expose
		d.fromFile["--expose"] = inFile("expose")
	}
	if !given["image"] && conf.Proxy.Image != "" {
		d.image = conf.Proxy.Image
		d.fromFile["--image"] = inFile("image")
	}
	if !given["strict"] && conf.Proxy.Strict {
		d.strict = true
	}
	return name, nil
}

// flagsGiven is the set of flags that appeared on the command line, by name.
func flagsGiven(fs *flag.FlagSet) map[string]bool {
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	return given
}

// explicitTarget is whether the command line or the environment names what
// this run routes at: --config, --sandbox, --environment (which deploys its
// own), $VERIS_PROXY_CONFIG or $VERIS_SANDBOX_ID.
func explicitTarget(src configSources, environment string) bool {
	return src.File != "" || src.Sandbox != "" || environment != "" ||
		os.Getenv(discovery.EnvConfig) != "" || os.Getenv(discovery.EnvSandboxID) != ""
}

// pointerSandbox is the folder's sandbox when it is what this run routes at:
// every explicit target is silent, and .veris/twin.local.yaml holds a
// pointer. Without a project file there is no local file, and so no pointer.
//
// The pointer records which environment its sandbox came from, and a run
// whose environment is a different one is refused: `veris run --env ci` at
// dev's sandbox would take ci's command and proxy settings to a world of
// dev's services, and nothing downstream could tell.
func pointerSandbox(s *session, src configSources, environment string) (string, error) {
	if explicitTarget(src, environment) {
		return "", nil
	}
	if s.res.Local == nil || s.res.Local.Sandbox == nil {
		return "", nil
	}
	ptr := s.res.Local.Sandbox
	envID := s.res.EnvName
	if s.res.Env != nil {
		envID = s.res.Env.ID
	} else if !looksLikeID(envID) {
		envID = ""
	}
	if envID != "" && ptr.EnvironmentID != "" && ptr.EnvironmentID != envID {
		return "", fmt.Errorf("this folder's sandbox %s belongs to environment %s, not '%s'. "+
			"Run veris up %s for one of its own, or pass --sandbox",
			ptr.ID, ownerLabel(s, ptr.EnvironmentID), s.res.EnvName, s.res.EnvName)
	}
	return ptr.ID, nil
}

// ownerLabel is envLabel with the id beside the name, "dev (k3j2v0d8…)",
// since the line it lands on is about two ids that differ.
func ownerLabel(s *session, id string) string {
	if name := projectEnvName(s, id); name != "" {
		return name + " (" + shortID(id) + ")"
	}
	return shortID(id)
}

// announcePointer says on stderr that the folder's pointer is what routes
// this run. It is printed only once every usage check has passed, so a
// refused command line never announces a routing decision that did not
// happen.
func announcePointer(id string) {
	if id != "" {
		fmt.Fprintf(os.Stderr, "veris: using sandbox %s (this folder)\n", id)
	}
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
