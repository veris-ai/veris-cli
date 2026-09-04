package main

// Wiring the callback direction: a listener, a tunnel, and a registration.
//
// The order is load-bearing and is the same argument as the readiness marker on
// the egress side. The listener binds first, the tunnel opens onto it second,
// and only then is the URL registered with the sandbox -- so a destination the
// sandbox knows about is always one that can already answer. Registering first
// would publish a URL that 502s until the rest caught up, and a webhook sent in
// that window is simply lost.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/veris-ai/veris-cli/internal/callback"
	"github.com/veris-ai/veris-cli/internal/config"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/proxy"
	"github.com/veris-ai/veris-cli/internal/tunnel"
)

// ingressOptions is what `--expose` and its companions asked for.
type ingressOptions struct {
	// RunAsUID is the uid the tunnel child drops to, matching the proxy's own.
	RunAsUID int
	Port     int
	Host     string
	Token    string
	Hostname string
	Binary   string
}

// ingress is a live callback path: everything needed to report on it and to
// take it down again.
type ingress struct {
	URL     string
	Inbound *proxy.Ingress

	tunnel *tunnel.Tunnel
	server *http.Server
	// Every client whose PATCH may have landed. A write that succeeded and
	// then failed to probe still has to be cleared, and the next endpoint
	// tried must not displace it.
	clients []*callback.Client
	log     *slog.Logger
}

// reservedPorts are the proxy's own listeners. The workload shares this network
// namespace, so there is one port space between us: exposing one of these would
// not merely be wrong, it would publish the proxy itself -- and
// /__veris/status is unauthenticated.
func reservedPorts(running *proxy.Running) map[int]string {
	out := map[int]string{}
	for _, kind := range []string{"proxy", "transparent-http", "transparent-https"} {
		if addr := running.Addr(kind); addr != "" {
			if p, err := portOfAddr(addr); err == nil {
				out[p] = kind
			}
		}
	}
	return out
}

func portOfAddr(addr string) (int, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		return 0, err
	}
	return p, nil
}

// startIngress opens the callback path and registers it with the sandbox.
func startIngress(
	ctx context.Context, log *slog.Logger, opts ingressOptions,
	cfg *config.Config, running *proxy.Running, pending *pendingIngress,
) (*ingress, error) {
	if taken, ok := reservedPorts(running)[opts.Port]; ok && isLocalOrigin(opts.Host) {
		return nil, fmt.Errorf(
			"--expose %d is the proxy's own %s listener. The workload shares this "+
				"network namespace, so there is one port space between you and the "+
				"proxy; expose the port your app listens on", opts.Port, taken)
	}

	// --environment opened the tunnel before the sandbox existed, so its URL
	// could be set at creation. Adopt it rather than opening a second one.
	if pending != nil {
		pending.adopted = true
		in := &ingress{
			URL: pending.url, Inbound: pending.inbound,
			tunnel: pending.tunnel, server: pending.server, log: log,
		}
		// Registered at creation, so there is nothing to PATCH. The clients are
		// still needed: the confirmation probe once the app is listening runs
		// through them, and without it a --environment run gets no verdict on
		// whether the sandbox can actually reach it.
		for _, ep := range cfg.ServiceEndpoints() {
			in.clients = append(in.clients, callback.New(ep.BaseURL, callback.Options{
				AuthValue:          os.Getenv(cfg.Upstream.AuthValueEnv),
				InsecureSkipVerify: cfg.Upstream.InsecureSkipVerify,
			}))
		}
		if len(in.clients) > 0 {
			if _, err := in.clients[0].ProbeResolved(ctx); err != nil {
				_ = in.Stop(context.Background())
				return nil, fmt.Errorf("confirm callback hostname before starting the app: %w", err)
			}
		}
		// The sandbox may already have probed us at creation, and that is not a
		// callback the run produced.
		in.Inbound.Baseline()
		return in, nil
	}

	inbound, err := proxy.NewIngress(opts.Host, opts.Port)
	if err != nil {
		return nil, err
	}

	// Loopback only. Whatever this binds is what the tunnel publishes, and a
	// routable bind would publish it twice -- once deliberately and once to
	// anything that can reach this host.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind the callback listener: %w", err)
	}
	srv := &http.Server{
		Handler:           inbound.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("callback listener stopped", "err", err)
		}
	}()

	tun, err := tunnel.Start(ctx, tunnel.Options{
		Target:   "http://" + ln.Addr().String(),
		Binary:   opts.Binary,
		Token:    opts.Token,
		Hostname: opts.Hostname,
		RunAsUID: opts.RunAsUID,
		Log:      log,
	})
	if err != nil {
		_ = srv.Close()
		return nil, err
	}

	in := &ingress{URL: tun.URL(), Inbound: inbound, tunnel: tun, server: srv, log: log}
	if err := in.register(ctx, cfg); err != nil {
		_ = in.Stop(context.Background())
		return nil, err
	}
	// Registration probed the URL through this very listener, so the receipt
	// already holds a callback the run did not produce. Zeroing here is what
	// makes `--require-callback` an assertion about the run rather than about
	// our own startup.
	inbound.Baseline()
	return in, nil
}

// register writes the URL into the sandbox and confirms the sandbox can reach
// it. `client.default_base_url` is sandbox-wide -- one world schema shared by
// every service -- so one service is written to rather than all of them; the
// rest are tried only if that one cannot be reached.
func (in *ingress) register(ctx context.Context, cfg *config.Config) error {
	endpoints := cfg.ServiceEndpoints()
	if len(endpoints) == 0 {
		return errors.New(
			"no service endpoint to register the callback URL with, so the sandbox " +
				"would never know where to deliver")
	}

	var lastErr error
	for _, ep := range endpoints {
		c := callback.New(ep.BaseURL, callback.Options{
			AuthValue:          os.Getenv(cfg.Upstream.AuthValueEnv),
			InsecureSkipVerify: cfg.Upstream.InsecureSkipVerify,
		})
		// Held before the write, not after it: a PATCH that lands and a probe
		// that then fails must still leave something able to clear the URL, or
		// the sandbox is left dispatching at a tunnel we are about to close.
		in.clients = append(in.clients, c)
		// The registration is a sandbox-wide singleton, so a URL already there
		// belongs to somebody else's run and this overwrites it -- their
		// callbacks then arrive at OUR app, with nothing said. Cannot be
		// prevented from here; saying so is the least that is owed.
		if prev, err := c.Current(ctx); err == nil &&
			prev.BaseURL != "" && prev.BaseURL != in.URL {
			in.log.Warn("another run had already registered a callback URL on this "+
				"sandbox, and it has been replaced. Its callbacks will arrive here. "+
				"Give concurrent runs their own sandbox",
				"replaced", prev.BaseURL, "with", in.URL)
		}
		state, err := c.Register(ctx, in.URL)
		if err != nil {
			if errors.Is(err, callback.ErrDNSNotReady) {
				return err
			}
			lastErr = err
			continue
		}
		switch {
		case state.Answered():
			in.log.Info("callbacks registered", "url", in.URL, "via", ep.Name)
		case state.DeadTunnel() != "":
			// The edge answered for an absent origin. Reported rather than
			// treated as ready, because a run started here receives nothing.
			return fmt.Errorf(
				"the sandbox reached the tunnel but not your app (%s). The tunnel is "+
					"up and %s is not answering behind it",
				state.DeadTunnel(), in.URL)
		default:
			// Not fatal: the app legitimately may not be listening yet, and the
			// sandbox re-probes. Saying so beats a silent maybe.
			in.log.Warn("callbacks registered, but the sandbox could not reach the app yet",
				"url", in.URL, "probe_state", state.State,
				"hint", "start your app, then it will answer the next delivery")
		}
		return nil
	}
	return fmt.Errorf("register the callback URL with the sandbox: %w", lastErr)
}

// Stop clears the registration and closes the tunnel.
//
// Clearing matters as much as closing: a hostname left registered outlives the
// tunnel that served it, and the sandbox's dispatcher keeps trying it for the
// next run.
func (in *ingress) Stop(ctx context.Context) error {
	if in == nil {
		return nil
	}
	for _, c := range in.clients {
		// Only ours. register warns when it replaces someone else's URL, so a
		// newer run may already own this row -- clearing it then would stop
		// THEIR callbacks.
		// Ownership must be PROVEN before clearing, not merely not-disproven.
		// A transient timeout here with another run holding the row would
		// otherwise erase their URL and stop their callbacks; leaving a stale
		// row behind is the cheaper mistake, and the probe reports it.
		cur, err := c.Current(ctx)
		if err != nil {
			in.log.Warn("could not read who owns the callback registration, so it "+
				"is left as it is", "err", err)
			continue
		}
		if cur.BaseURL != "" && cur.BaseURL != in.URL {
			in.log.Info("another run has taken over the callback registration; "+
				"leaving it alone", "theirs", cur.BaseURL)
			continue
		}
		if err := c.Clear(ctx); err != nil {
			in.log.Warn("could not clear the callback registration; the sandbox "+
				"still points at a tunnel that is closing", "err", err)
		}
	}
	if in.tunnel != nil {
		_ = in.tunnel.Stop()
	}
	if in.server != nil {
		_ = in.server.Close()
	}
	return nil
}

// Receipt is what arrived while the run was live.
func (in *ingress) Receipt() proxy.InboundReceipt {
	if in == nil {
		return proxy.InboundReceipt{}
	}
	return in.Inbound.Receipt()
}

// printInbound is the callback direction's proof of work: what the app actually
// received, as opposed to what it was configured to receive.
//
// The mirror of printReceipt, and it exists for the same reason. A webhook
// suite that stopped receiving still passes; only a count says otherwise.
func printInbound(w io.Writer, r proxy.InboundReceipt) {
	if r.Total == 0 {
		fmt.Fprintln(w, "veris: your app received no callbacks from this run.")
		return
	}
	fmt.Fprintf(w, "veris: your app received %d callback(s):\n", r.Total)
	for _, c := range r.Callbacks {
		fmt.Fprintf(w, "  %-6s %-28s %d -> %d\n", c.Method, c.Path, c.Count, c.Status)
	}
	if r.Failed > 0 {
		fmt.Fprintf(w,
			"  %d never reached your app: it was not answering on the exposed port\n",
			r.Failed)
	}
}

// unmetCallbacks reports requirements the inbound receipt did not satisfy.
func unmetCallbacks(reqs []requirement, r proxy.InboundReceipt) []string {
	var out []string
	for _, req := range reqs {
		// Delivered, not merely arrived. A callback the app never answered --
		// because it was not listening -- must not satisfy a requirement that
		// says it received one.
		got := r.DeliveredByPath[req.name]
		if req.name == "" || req.name == "*" {
			got = r.Delivered
		}
		if got < req.count {
			out = append(out, fmt.Sprintf(
				"the run required callback %s at least %d time(s) but your app "+
					"received it %d time(s)", req.name, req.count, got))
		}
	}
	return out
}

// --- deploying a sandbox of our own -----------------------------------------

// pendingIngress is a callback path that is open but not yet registered, which
// is the state `--environment` needs: the URL has to exist before the sandbox
// is created, so it can be handed over at creation instead of PATCHed after.
type pendingIngress struct {
	url      string
	inbound  *proxy.Ingress
	tunnel   *tunnel.Tunnel
	server   *http.Server
	adopted  bool
	logger   *slog.Logger
	original ingressOptions
}

// URL is the public callback URL, or "" when nothing was opened.
func (p *pendingIngress) URL() string {
	if p == nil {
		return ""
	}
	return p.url
}

// CloseOnFailure tears the tunnel down unless serve went on to adopt it, so an
// error between opening and adopting cannot leak a live public URL.
func (p *pendingIngress) CloseOnFailure() {
	if p == nil || p.adopted {
		return
	}
	if p.tunnel != nil {
		_ = p.tunnel.Stop()
	}
	if p.server != nil {
		_ = p.server.Close()
	}
}

// openIngress binds the listener and opens the tunnel, stopping short of
// registration -- there is nothing to register with yet.
func openIngress(ctx context.Context, log *slog.Logger, opts ingressOptions) (*pendingIngress, error) {
	inbound, err := proxy.NewIngress(opts.Host, opts.Port)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind the callback listener: %w", err)
	}
	srv := &http.Server{Handler: inbound.Handler(), ReadHeaderTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("callback listener stopped", "err", err)
		}
	}()
	tun, err := tunnel.Start(ctx, tunnel.Options{
		Target:   "http://" + ln.Addr().String(),
		Binary:   opts.Binary,
		Token:    opts.Token,
		Hostname: opts.Hostname,
		RunAsUID: opts.RunAsUID,
		Log:      log,
	})
	if err != nil {
		_ = srv.Close()
		return nil, err
	}
	return &pendingIngress{
		url: tun.URL(), inbound: inbound, tunnel: tun, server: srv,
		logger: log, original: opts,
	}, nil
}

// sandboxLifetime is a sandbox this process created and is responsible for.
type sandboxLifetime struct {
	SandboxID     string
	EnvironmentID string
	client        *discovery.Client
}

// deploySandbox creates a sandbox for an environment, with the callback URL
// already set, and waits for it to be usable.
func deploySandbox(
	ctx context.Context, log *slog.Logger, src configSources,
	environmentID string, ttlMinutes int, callbackURL string,
) (*sandboxLifetime, error) {
	client, err := discovery.NewClient(src.APIBase, src.APIKey)
	if err != nil {
		return nil, err
	}
	log.Info("deploying a sandbox", "environment", environmentID,
		"callbacks", callbackURL, "ttl_minutes", ttlMinutes)

	sandbox, err := client.Create(ctx, environmentID, discovery.CreateOptions{
		ClientBaseURL: callbackURL,
		TTLMinutes:    ttlMinutes,
	})
	if err != nil {
		return nil, err
	}
	life := &sandboxLifetime{
		SandboxID: sandbox.ID, EnvironmentID: environmentID, client: client,
	}
	// Creation returns while the sandbox is still provisioning, so routing at
	// it now would fail a suite against a sandbox that was merely starting.
	if _, err := client.WaitReady(ctx, sandbox.ID, sandboxReadyTimeout); err != nil {
		life.Delete(log)
		return nil, err
	}
	log.Info("sandbox ready", "sandbox_id", sandbox.ID)
	return life, nil
}

// sandboxReadyTimeout bounds the wait for a new sandbox to come up. Pods have
// to schedule and images have to pull, so this is minutes rather than seconds.
const sandboxReadyTimeout = 5 * time.Minute

// Delete removes the sandbox this run created. Reported rather than fatal: it
// runs during teardown, and the TTL is the backstop.
func (l *sandboxLifetime) Delete(log *slog.Logger) {
	if l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := l.client.Delete(ctx, l.EnvironmentID, l.SandboxID); err != nil {
		log.Warn("could not delete the sandbox this run created; it will expire "+
			"on its TTL", "sandbox_id", l.SandboxID, "err", err)
		return
	}
	log.Info("sandbox deleted", "sandbox_id", l.SandboxID)
}

// isLocalOrigin reports whether the exposed port is in the proxy's own port
// space. Only then can it collide with our listeners -- host.docker.internal:8080
// is a different machine's 8080 and no business of ours.
func isLocalOrigin(host string) bool {
	switch host {
	case "", "127.0.0.1", "localhost", "::1", "[::1]", "0.0.0.0":
		return true
	}
	return false
}

// cfg0Listen reads the port from a --listen override, or 0 when none was given.
func cfg0Listen(listen string) int {
	if listen == "" {
		return 0
	}
	p, err := portOfAddr(listen)
	if err != nil {
		return 0
	}
	return p
}

// refuseConfiguredPort rejects an --expose that collides with a listener we
// already know we will bind.
//
// The same check runs again once the listeners are up, because :0 resolves
// only then. This earlier pass exists so the common collisions are caught
// before a public URL exists at all.
func refuseConfiguredPort(expose int, host string, listen int, tHTTP, tHTTPS string, transparent bool) error {
	if !isLocalOrigin(host) {
		return nil
	}
	known := map[int]string{}
	if listen > 0 {
		known[listen] = "proxy"
	}
	if transparent {
		for addr, kind := range map[string]string{tHTTP: "transparent-http", tHTTPS: "transparent-https"} {
			if p, err := portOfAddr(addr); err == nil && p > 0 {
				known[p] = kind
			}
		}
	}
	if kind, ok := known[expose]; ok {
		return fmt.Errorf(
			"--expose %d is the proxy's own %s listener. The workload shares this "+
				"network namespace, so there is one port space between you and the "+
				"proxy; expose the port your app listens on", expose, kind)
	}
	return nil
}

// originReadyTimeout bounds the wait for the app to start listening. The
// workload container is started only after the proxy reports ready, so this is
// generous: image pulls and language runtimes boot slowly.
const originReadyTimeout = 3 * time.Minute

// confirmWhenReady waits for the app to start listening, then asks the sandbox
// to probe again.
//
// The startup probe cannot succeed in the container tier, and not because
// anything is wrong: the workload container is started only AFTER the proxy
// reports ready, so at registration time nothing is listening on the exposed
// port yet. Reporting that as the verdict would teach every user to ignore it.
//
// So the verdict is deferred rather than faked. Nothing blocks on this -- the
// run proceeds -- but the line a user reads about their callback path is the
// one taken once their app was actually up.
func (in *ingress) confirmWhenReady(ctx context.Context, origin string) {
	go func() {
		deadline := time.Now().Add(originReadyTimeout)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			conn, err := net.DialTimeout("tcp", origin, 2*time.Second)
			if err != nil {
				continue
			}
			_ = conn.Close()

			var lastErr error
			for _, c := range in.clients {
				// Discounted BEFORE the probe is issued, not after. A workload
				// that exits the moment it answers can have its receipt read
				// while the probe is recorded and the discount has not run --
				// and `--require-callback '*'` would pass on our own probe.
				// Refunded below if the probe never reached this listener.
				// Provisional, on the assumption the probe answers 200. The
				// real status is known only afterwards, so this is corrected
				// below -- but it is taken FIRST so a receipt read mid-probe
				// can never count it.
				in.Inbound.DiscountOne("GET", "/", 200, true)
				state, err := c.Probe(ctx)
				if err != nil {
					lastErr = err
					in.Inbound.RefundOne("GET", "/", 200, true)
					continue
				}
				if state.Answered() {
					in.log.Info("callback path confirmed: the sandbox reached your app",
						"url", in.URL)
				} else {
					in.log.Warn("your app is listening but the sandbox could not "+
						"reach it through the tunnel",
						"url", in.URL, "probe_state", state.State,
						"dead_tunnel", state.DeadTunnel())
				}
				// The probe came through this listener, so it is discounted --
				// but ONLY it. The app is listening by now, so a real callback
				// may have raced this probe, and baselining the whole receipt
				// would silently lose it.
				// Only an ANSWERED probe got a response back through the
				// tunnel, so only that one can have come through this
				// listener. Anything else -- unreachable, or the edge
				// answering for an absent origin -- never did, and the
				// pre-emptive discount is given back.
				// Correct the provisional discount to the status the app
				// actually returned. A 204 left the entry in
				// DeliveredByPath["/"], where --require-callback / would pass
				// on it; a 4xx subtracted from Delivered, which was never
				// incremented, driving the count negative.
				in.Inbound.RefundOne("GET", "/", 200, true)
				if state.Answered() {
					status := 200
					if r := state.LastProbeResult; r != nil {
						if v, ok := r["status"].(float64); ok {
							status = int(v)
						}
					}
					in.Inbound.DiscountOne("GET", "/", status, status < 400)
				}
				return
			}
			// Every probe failed, or there was nobody to probe. Silence here
			// would be the exact failure this direction exists to refuse: the
			// user would read nothing and assume the path was fine.
			in.log.Warn("your app is listening, but the callback path could not be "+
				"confirmed with the sandbox",
				"url", in.URL, "endpoints", len(in.clients), "err", lastErr)
			return
		}
		in.log.Warn("nothing started listening on the exposed port, so no callback "+
			"can be delivered", "origin", origin, "waited", originReadyTimeout)
	}()
}

// originAddr is where the app is expected to be listening.
func (in *ingress) originAddr() string { return in.Inbound.Origin() }
