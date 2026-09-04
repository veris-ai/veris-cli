package main

// Webhooks in the host tier.
//
// Without --image the proxy is this process, and this process opens no
// callback path: the listener, the tunnel and the registration live in
// `veris serve --expose`, which the container tier runs inside its proxy
// container. Rather than grow a second copy of them here, the host tier
// composes that same serve as a child: start it with --write-env and
// --ready-file, wait for the marker, source the environment it wrote into the
// command under test, run the command, read both receipts over the status
// endpoint, and SIGTERM serve -- which clears its own registration and closes
// the tunnel on the way out. One proxy, one ingress, two tiers.
//
// The command under test sees what the in-process host tier hands it today,
// plus VERIS_PUBLIC_URL; the ledger is fed as the other tiers feed it, so
// --require-callback is judged on both sides. One known difference: without
// --java-truststore the in-process tier prefers a JKS derived from the local
// JDK's own cacerts (supervise), while serve's --write-env publishes the
// store built from the system roots, so a JVM suite that relies on a root
// present only in the JDK's cacerts should pass --java-truststore here.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/procgroup"
	"github.com/veris-ai/veris-cli/internal/proxy"
	"github.com/veris-ai/veris-cli/internal/trust"
)

// serveCommand builds the child that runs `veris serve`: this binary again,
// with args (which begin with "serve"). A variable so a test can substitute a
// fake serve that writes the same files and answers the same endpoint. The
// caller sets the environment and the streams; a non-nil Env on the returned
// command is kept as the base.
var serveCommand = func(args []string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate this binary to start the proxy: %w", err)
	}
	return exec.Command(exe, args...), nil //nolint:gosec // our own binary
}

// hostReadyTimeout bounds the wait for serve's ready marker. A quick tunnel
// may take up to a minute to announce its hostname, and the registration
// probe and a sandbox read come after it, so this is minutes, not seconds.
const hostReadyTimeout = 3 * time.Minute

// serveStopGrace is how long serve gets after SIGTERM before it is killed.
// It clears the callback registration and drains the tunnel in that time; the
// container tier gives it the same twenty seconds.
const serveStopGrace = 20 * time.Second

// hostTunnel is a serve child owning the proxy and the callback path for one
// host-tier run, from start to teardown.
type hostTunnel struct {
	cmd *exec.Cmd
	// dir holds the env and ready files serve writes, and goes with the run.
	dir string
	// done yields cmd.Wait's result once, then stays closed.
	done chan error
	// proxyURL is the proxy's bound address, read back from the ready file
	// rather than assumed: the child listens on :0.
	proxyURL string
	// env is the interception environment serve wrote, VERIS_PUBLIC_URL
	// included, in the shape mergeEnv layers over the inherited one.
	env []trust.Var
	// publicURL is where the sandbox delivers callbacks.
	publicURL string

	stopOnce sync.Once
	stopErr  error
}

// startHostTunnel starts serve and waits until it reports ready. sigs is the
// run's own signal channel: a Ctrl-C while the tunnel is opening stops the
// child rather than leaving it alive in its own process group. On error
// nothing is left running.
func startHostTunnel(o localRun, sigs <-chan os.Signal) (*hostTunnel, error) {
	dir, err := os.MkdirTemp("", "veris-run-*")
	if err != nil {
		return nil, err
	}
	ht := &hostTunnel{dir: dir, done: make(chan error, 1)}
	envPath := filepath.Join(dir, "veris.env")
	readyPath := filepath.Join(dir, "ready")

	cmd, err := serveCommand(serveArgs(o, envPath, readyPath))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	// serve's --environment defaults to $VERIS_ENVIRONMENT_ID, the variable
	// the CLI's own commands read their environment from, and a serve that
	// sees it either refuses to start beside --sandbox or deploys a sandbox of
	// its own. The lifecycle is this run's business (up and --fresh own it),
	// so the inherited variable does not reach the child.
	cmd.Env = withoutVar(cmd.Env, cfg.EnvEnvironmentID)
	// The key and the tunnel token travel in the environment, never in argv,
	// where `ps` and a CI log would show them. serve reads both from exactly
	// these variables.
	cmd.Env = append(cmd.Env, serveEnv(o)...)
	// serve writes nothing to stdout that a run wants; its stderr is the
	// user's, minus the inbound receipt it prints at exit, which this run
	// prints itself from the status endpoint.
	cmd.Stdout = os.Stderr
	cmd.Stderr = &inboundFilter{w: os.Stderr}
	// Its own process group: the terminal's Ctrl-C reaches this process,
	// which stops serve once the command under test is gone and the receipts
	// are read -- not before, or the receipts are lost with it.
	procgroup.Isolate(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start the proxy: %w", err)
	}
	ht.cmd = cmd
	go func() { ht.done <- cmd.Wait(); close(ht.done) }()

	if err := ht.waitReady(readyPath, sigs); err != nil {
		ht.close()
		return nil, err
	}
	if err := ht.load(envPath, readyPath); err != nil {
		ht.close()
		return nil, err
	}
	return ht, nil
}

// waitReady polls for the marker, watching the child so a startup failure
// surfaces as the child's own exit rather than as a timeout.
func (ht *hostTunnel) waitReady(readyPath string, sigs <-chan os.Signal) error {
	deadline := time.Now().Add(hostReadyTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-ht.done:
			return fmt.Errorf("the proxy exited during startup (%v); its output is above",
				exitDescription(err))
		case <-sigs:
			// The developer left. Say so with the conventional status, and
			// take serve down on the way out.
			return exitCode(130)
		case <-tick.C:
			if info, err := os.Stat(readyPath); err == nil && info.Size() > 0 {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the proxy did not become ready within %s; its output is above",
					hostReadyTimeout)
			}
		}
	}
}

// exitDescription is what a child's Wait error says, "exit status 0" for a
// clean exit that was nonetheless too early.
func exitDescription(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// load reads the two files serve wrote once the marker exists: the proxy's
// bound address, and the environment the command under test will run with.
func (ht *hostTunnel) load(envPath, readyPath string) error {
	ready, err := os.ReadFile(readyPath)
	if err != nil {
		return fmt.Errorf("read the proxy's ready file: %w", err)
	}
	for _, line := range strings.Split(string(ready), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "proxy="); ok {
			ht.proxyURL = v
		}
	}
	if ht.proxyURL == "" {
		return errors.New("the proxy's ready file names no proxy address")
	}
	body, err := os.ReadFile(envPath)
	if err != nil {
		return fmt.Errorf("read the environment the proxy wrote: %w", err)
	}
	if ht.env, err = parseEnvFile(body); err != nil {
		return fmt.Errorf("%s: %w", envPath, err)
	}
	for _, v := range ht.env {
		if v.Name == "VERIS_PUBLIC_URL" {
			ht.publicURL = v.Value
		}
	}
	if ht.publicURL == "" {
		return errors.New("the proxy reported ready without a public callback URL")
	}
	return nil
}

// childEnv is the environment the command under test runs with. An explicit
// --java-truststore replaces the store serve published, as it does in the
// in-process tier.
func (ht *hostTunnel) childEnv(java javaOptions) []trust.Var {
	if java.store == "" {
		return ht.env
	}
	out := append([]trust.Var{}, ht.env...)
	for i, v := range out {
		if v.Name != "JAVA_TOOL_OPTIONS" {
			continue
		}
		fields := strings.Fields(v.Value)
		for j, f := range fields {
			switch {
			case strings.HasPrefix(f, "-Djavax.net.ssl.trustStore="):
				fields[j] = "-Djavax.net.ssl.trustStore=" + java.store
			case strings.HasPrefix(f, "-Djavax.net.ssl.trustStorePassword="):
				fields[j] = "-Djavax.net.ssl.trustStorePassword=" + java.pass
			}
		}
		out[i].Value = strings.Join(fields, " ")
	}
	return out
}

// receipts is the run's single verdict read: both receipts in one drained
// status read, so the in-flight handshakes have settled and the two halves
// describe the same moment.
func (ht *hostTunnel) receipts() (proxy.Receipt, *proxy.InboundReceipt, error) {
	return readStatusReceipts(ht.proxyURL + proxy.StatusPath)
}

// readStatusReceipts reads the engine's receipt and the inbound one from a
// proxy's status endpoint with drain=1. The inbound receipt is a pointer for
// the reason fetchInboundReceipt gives: a proxy that opened no callback path
// omits the field, and that must not read as an empty receipt.
func readStatusReceipts(statusURL string) (proxy.Receipt, *proxy.InboundReceipt, error) {
	var out struct {
		Receipt proxy.Receipt         `json:"receipt"`
		Inbound *proxy.InboundReceipt `json:"inbound_receipt"`
	}
	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Get(statusURL + "?drain=1")
	if err != nil {
		return out.Receipt, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out.Receipt, nil, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out.Receipt, nil, fmt.Errorf("status is not readable: %w", err)
	}
	return out.Receipt, out.Inbound, nil
}

// stop asks serve to leave and waits for it: SIGTERM, then SIGKILL after the
// grace. Idempotent, and every path out of the run calls it. The error is a
// child that had already died, or one that had to be killed -- either way
// the registration may not have been cleared, which the caller reports.
func (ht *hostTunnel) stop() error {
	ht.stopOnce.Do(func() {
		select {
		case err, ok := <-ht.done:
			// Gone before it was asked: the proxy died mid-run, and it says so
			// on stderr above. The receipt read has already reported that the
			// receipt could not be read; this names the cause.
			if ok {
				ht.stopErr = fmt.Errorf("the proxy exited during the run (%s)", exitDescription(err))
			}
			return
		default:
		}
		procgroup.Terminate(ht.cmd, syscall.SIGTERM)
		select {
		case <-ht.done:
			return
		case <-time.After(serveStopGrace):
		}
		procgroup.Terminate(ht.cmd, syscall.SIGKILL)
		<-ht.done
		ht.stopErr = fmt.Errorf("the proxy ignored SIGTERM for %s and was killed, "+
			"so the callback registration may still point at a closed tunnel", serveStopGrace)
	})
	return ht.stopErr
}

// close stops serve if it is still running and removes the run's files.
func (ht *hostTunnel) close() {
	_ = ht.stop()
	_ = os.RemoveAll(ht.dir)
}

// serveArgs is the serve command line for one host-tier run: the same
// routing target this run resolved, the exposed port, the handoff files, and
// every proxy setting the run's own flags carried. Nothing secret: the key
// and the tunnel token go in serveEnv.
func serveArgs(o localRun, envPath, readyPath string) []string {
	listen := o.listen
	if listen == "" {
		// A free port, read back from the ready file. Loopback: the command
		// under test runs on this host.
		listen = "127.0.0.1:0"
	}
	args := []string{"serve",
		"--listen", listen,
		"--expose", strconv.Itoa(o.expose),
		"--write-env", envPath,
		"--ready-file", readyPath,
		"--log-level", o.logLevel,
		"--log-format", o.logFormat,
	}
	// resolveConfig's precedence, restated: an explicit file, else an
	// explicit sandbox, else the folder's pointer -- which run sets only when
	// $VERIS_PROXY_CONFIG and $VERIS_SANDBOX_ID are silent, so serve, which
	// inherits both, resolves the same target.
	switch {
	case o.sources.File != "":
		args = append(args, "--config", o.sources.File)
	case o.sources.Sandbox != "":
		args = append(args, "--sandbox", o.sources.Sandbox)
	case o.sources.Local != "":
		args = append(args, "--sandbox", o.sources.Local)
	}
	if o.sources.Refresh {
		args = append(args, "--refresh")
	}
	if o.strict {
		args = append(args, "--strict")
	}
	if o.caDir != "" {
		args = append(args, "--ca-dir", o.caDir)
	}
	if o.exposeHost != "" {
		args = append(args, "--expose-host", o.exposeHost)
	}
	if o.tunnelHostname != "" {
		args = append(args, "--expose-hostname", o.tunnelHostname)
	}
	// --route, re-serialised per entry in a stable order, so serve derives
	// the routes this run would have.
	services := make([]string, 0, len(o.sources.Overrides))
	for name := range o.sources.Overrides {
		services = append(services, name)
	}
	sort.Strings(services)
	for _, name := range services {
		for _, e := range o.sources.Overrides[name] {
			if len(e.Paths) == 0 {
				args = append(args, "--route", name+"="+e.Host)
				continue
			}
			for _, p := range e.Paths {
				args = append(args, "--route", name+"="+e.Host+p)
			}
		}
	}
	return args
}

// serveEnv is what the run hands serve through the environment: the control
// plane the run resolved, and the tunnel token. Only what this run knows;
// serve inherits the rest.
func serveEnv(o localRun) []string {
	var env []string
	if o.sources.APIBase != "" {
		env = append(env, discovery.EnvAPIBase+"="+o.sources.APIBase)
	}
	if o.sources.APIKey != "" {
		env = append(env, discovery.EnvAPIKey+"="+o.sources.APIKey)
	}
	if o.tunnelToken != "" {
		env = append(env, "VERIS_TUNNEL_TOKEN="+o.tunnelToken)
	}
	return env
}

// withoutVar is env minus every entry for name.
func withoutVar(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if k, _, _ := strings.Cut(entry, "="); k == name {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// parseEnvFile reads the file `serve --write-env` writes in its posix
// format, back into the variables it was written from: `export NAME='value'`
// for a plain variable, and `export NAME="${NAME:+$NAME }value"` for one that
// extends what the developer already set. Sourcing it through a shell would
// cost a shell and its quoting; reading the two shapes the writer produces
// costs nothing and keeps mergeEnv's append semantics.
func parseEnvFile(body []byte) ([]trust.Var, error) {
	var vars []trust.Var
	for i, raw := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimSpace(string(raw))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rest, ok := strings.CutPrefix(line, "export ")
		if !ok {
			return nil, fmt.Errorf("line %d is not an export the proxy writes: %q", i+1, line)
		}
		name, value, ok := strings.Cut(rest, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("line %d names no variable: %q", i+1, line)
		}
		appendPrefix := `"${` + name + `:+$` + name + ` }`
		switch {
		case len(value) >= 2 && strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'"):
			vars = append(vars, trust.Var{
				Name:  name,
				Value: strings.ReplaceAll(value[1:len(value)-1], `'\''`, `'`),
			})
		case strings.HasPrefix(value, appendPrefix) && strings.HasSuffix(value, `"`) && len(value) > len(appendPrefix):
			vars = append(vars, trust.Var{
				Name:   name,
				Value:  unescapeDouble(value[len(appendPrefix) : len(value)-1]),
				Append: true,
			})
		default:
			return nil, fmt.Errorf("line %d is not quoted the way the proxy writes it: %q", i+1, line)
		}
	}
	return vars, nil
}

// unescapeDouble undoes shellEscapeDouble.
func unescapeDouble(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '"', '\\', '$', '`':
				i++
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

// inboundFilter forwards serve's stderr, minus the inbound receipt serve
// prints when it exits: the header line and the indented lines under it.
// The run prints that receipt itself, from the status endpoint, beside the
// engine's; printed twice it would read as two different runs.
type inboundFilter struct {
	w io.Writer
	// buf holds a partial line until its newline arrives.
	buf []byte
	// dropping is set by the header and cleared by the first line that is
	// not indented under it.
	dropping bool
}

const inboundHeader = "veris: your app received "

func (f *inboundFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := f.buf[:i+1]
		f.buf = f.buf[i+1:]
		if err := f.line(line); err != nil {
			return len(p), err
		}
	}
}

func (f *inboundFilter) line(line []byte) error {
	s := string(line)
	switch {
	case strings.HasPrefix(s, inboundHeader):
		f.dropping = true
		return nil
	case f.dropping && strings.HasPrefix(s, "  "):
		return nil
	}
	f.dropping = false
	_, err := f.w.Write(line)
	return err
}

// flush writes whatever partial line is left when the child is gone.
func (f *inboundFilter) flush() {
	if len(f.buf) > 0 {
		_, _ = f.w.Write(f.buf)
		f.buf = nil
	}
}

// runHostExposed is the host tier with a callback path: runLocal's shape,
// with serve as a child in place of the in-process proxy. Every return
// stops serve.
func runHostExposed(o localRun) error {
	// Subscribed before serve starts and held until the run returns, so no
	// signal in between takes the default action and leaves serve alive in
	// its own process group. supervise forwards the same signals to the
	// command under test while it runs.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	ht, err := startHostTunnel(o, sigs)
	if err != nil {
		return err
	}
	// Registered before close, so it runs after: whatever serve wrote last
	// without a newline is flushed once serve is gone and its output copied.
	if f, ok := ht.cmd.Stderr.(*inboundFilter); ok {
		defer f.flush()
	}
	defer ht.close()
	if !o.quiet {
		fmt.Fprintf(os.Stderr, "veris: callbacks arrive at %s\n", ht.publicURL)
	}

	// The sandbox side: the watermark once the proxy is live and before the
	// child starts, exactly as the in-process tier takes it.
	bg := context.Background()
	p := newProof(bg, ledgerSandbox(o.sources), o.client, o.sources.Overrides)
	p.watermark(bg, os.Stderr, o.quiet)
	started := time.Now()

	// serve's environment file already carries what its config hands over;
	// the handoff is layered again from the sandbox's description so the
	// run says what was handed, then -e last so the user's own value wins.
	all := handoffs(p.sandboxServices(), o.sources.Overrides, nil)
	handed := withoutUserVars(all, o.userEnv)
	announceHandoffs(os.Stderr, handed)
	announceSuppressed(os.Stderr, all, o.userEnv)
	env := mergeEnv(os.Environ(), ht.childEnv(o.java))
	env = mergeEnv(env, handedVars(handed))
	env = mergeEnv(env, userEnvVars(o.userEnv))
	status, runErr := superviseEnv(env, o.argv)
	finished := time.Now()
	if o.fresh {
		defer holdSignals()()
	}
	if runErr != nil {
		// The command never ran: nothing to report, and serve goes with it.
		return runErr
	}

	// The verdict read comes BEFORE serve is asked to leave; both receipts
	// live in its process. Then the teardown, whose failure says the
	// registration may be stale.
	var engine *proxy.Receipt
	receipt, inbound, readErr := ht.receipts()
	if readErr == nil && inbound == nil {
		readErr = errors.New("the proxy reported no inbound receipt")
	}
	if readErr == nil {
		engine = &receipt
	}
	stopErr := ht.stop()
	return o.conclude(p, status, engine, inbound, readErr, stopErr, started, finished)
}
