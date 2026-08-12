// Package proxy implements the Veris interception proxy.
//
// The proxy terminates TLS for hosts the code under test believes it is
// calling, rewrites the destination to the corresponding simulated service in
// a Veris dependency sandbox, and forwards the request unchanged otherwise.
// The code under test is never modified.
package proxy

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/elazarl/goproxy"

	"github.com/veris-ai/veris-proxy/internal/ca"
	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/hostport"
)

// Control-plane paths served by the proxy itself, on any host. They are
// namespaced under /__veris/ to keep the chance of colliding with a real API
// path negligible.
const (
	CanaryPath = "/__veris/canary"
	StatusPath = "/__veris/status"
)

// Headers added to every intercepted request so the sandbox can route and
// attribute it.
const (
	HeaderOriginalHost = "X-Veris-Original-Host"
	HeaderSandbox      = "X-Veris-Sandbox"
	HeaderService      = "X-Veris-Service"
	HeaderEnv          = "X-Veris-Env"
)

// Server wraps a configured goproxy instance.
type Server struct {
	cfg     *config.Config
	ca      *ca.CA
	log     *slog.Logger
	version string
	started time.Time

	handler  *goproxy.ProxyHttpServer
	upstream *http.Client

	intercepted atomic.Int64
	blocked     atomic.Int64
	passedvia   atomic.Int64
	canaryHits  atomic.Int64
	// inbound is set when --expose opened a callback path, so the status
	// endpoint can report what the app received.
	inbound atomic.Pointer[*Ingress]

	receipt       receipt
	trustFailures trustLog
}

// New builds a Server. It does not listen; call Handler and serve it, or use
// ListenAndServe.
func New(cfg *config.Config, authority *ca.CA, log *slog.Logger, version string) *Server {
	s := &Server{
		cfg:     cfg,
		ca:      authority,
		log:     log,
		version: version,
		started: time.Now(),
	}

	// A canary always exists. Its job is to answer "is this the proxy for MY
	// run", and a per-process token answers that whether or not the caller
	// supplied one -- so `check` is a real assertion by default instead of
	// degrading to bare liveness whenever the config happened to omit it.
	if s.cfg.CanaryToken == "" {
		s.cfg.CanaryToken = mintCanary()
	}

	s.upstream = newUpstreamClient(cfg.Upstream.InsecureSkipVerify)

	p := goproxy.NewProxyHttpServer()
	p.Verbose = false
	p.Logger = slogAdapter{log}

	// Most large vendors serve HTTP/2, so a client that negotiates h2 against
	// the real API must be able to negotiate it here too. goproxy leaves this
	// off by default; without it every intercepted CONNECT tunnel downgrades
	// the client to HTTP/1.1.
	p.AllowHTTP2 = true

	// goproxy's default transport reads HTTP_PROXY/HTTPS_PROXY from the
	// environment. Since we are the proxy, and since the CLI sets those
	// variables in the environment of the process under test, inheriting them
	// here risks the proxy chaining to itself. Always dial directly.
	p.Tr.Proxy = nil
	p.Tr.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.Upstream.InsecureSkipVerify, //nolint:gosec // operator opt-in for local sandboxes
	}
	// Assigning TLSClientConfig turns off the automatic h2 upgrade, so the leg
	// from here to the sandbox has to ask for it back.
	p.Tr.ForceAttemptHTTP2 = true

	// Our CA package owns leaf issuance so we control the SANs and, critically,
	// serve leaf + CA rather than leaf alone. Clients on OpenSSL and Node
	// reject a bare leaf with UNABLE_TO_VERIFY_LEAF_SIGNATURE.
	p.CertStore = certStore{authority}

	p.OnRequest().HandleConnectFunc(s.onConnect)
	p.OnRequest().DoFunc(s.onRequest)

	// A request with a relative path means someone pointed a client straight at
	// the proxy's own address rather than configuring it as a proxy. Serve the
	// control-plane endpoints there too, since that is the most convenient way
	// for a health check to reach them.
	p.NonproxyHandler = http.HandlerFunc(s.serveDirect)

	s.handler = p
	return s
}

// Handler returns the HTTP handler for the proxy.
func (s *Server) Handler() http.Handler { return s.handler }

// Config exposes the configuration the server was built from.
func (s *Server) Config() *config.Config { return s.cfg }

// recordIntercept is the single accounting point for a routed request, called
// once a target has been resolved and before the request is rewritten. Both
// the explicit-proxy and the transparent paths go through it, so the run
// receipt cannot drift from the counters.
func (s *Server) recordIntercept(host string, target *config.Target, mode, method, path string) {
	s.intercepted.Add(1)
	s.receipt.record(host, target)
	s.log.Info("intercepted",
		"service", target.Service,
		"host", host,
		"method", method,
		"path", path,
		"upstream", target.Upstream.Host,
		"mode", mode,
	)
}

// onConnect decides whether to intercept a CONNECT tunnel.
func (s *Server) onConnect(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
	if s.cfg.IsPassthrough(host) {
		// A plain tunnel: we never see the plaintext, which is exactly what we
		// want for the service under test talking to itself.
		return goproxy.OkConnect, host
	}
	// Everything else is intercepted, including hosts with no mapping. We want
	// to see and *block* those rather than let them tunnel out to the real
	// internet unobserved.
	return goproxy.MitmConnect, host
}

// onRequest is the single decision point for every request that reaches the
// proxy, whether it arrived in plaintext or through an intercepted tunnel.
func (s *Server) onRequest(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
	if req.URL.Path == CanaryPath || req.URL.Path == StatusPath {
		return req, s.controlResponse(req)
	}

	// On an intercepted CONNECT, goproxy leaves the port on req.URL.Host, so
	// api.stripe.com arrives as api.stripe.com:443. Strip it before anything
	// user-visible or anything used as a map key.
	host := hostport.StripPort(firstNonEmpty(req.URL.Host, req.Host))

	if s.cfg.IsPassthrough(host) {
		s.passedvia.Add(1)
		s.log.Debug("passthrough", "host", host, "path", req.URL.Path)
		return req, nil
	}

	target, ok := s.cfg.Resolve(host, req.URL.Path)
	if !ok {
		s.blocked.Add(1)
		if s.cfg.Mode == config.ModePassthrough {
			s.log.Warn("unmapped host forwarded to the real internet",
				"host", host, "path", req.URL.Path,
				"hint", "mode=passthrough; set mode=strict before trusting a green test run")
			return req, nil
		}
		s.log.Error("blocked unmapped host", "host", host, "method", req.Method, "path", req.URL.Path)
		return req, s.blockResponse(req, host)
	}

	s.recordIntercept(host, target, "proxy", req.Method, req.URL.Path)
	rewriteTo(req, target)
	s.annotate(req)
	return req, nil
}

// rewriteTo redirects req at the sandbox upstream while preserving everything
// the origin API needs to interpret it.
func rewriteTo(req *http.Request, target *config.Target) {
	original := hostport.StripPort(firstNonEmpty(req.URL.Host, req.Host))

	req.URL.Scheme = target.Upstream.Scheme
	req.URL.Host = target.Upstream.Host
	req.URL.Path = joinPath(target.Upstream.Path, req.URL.Path)

	// Both must be set. goproxy uses req.URL.Host for dialing but req.Host
	// becomes the outgoing Host header, and leaving the original there sends
	// the sandbox a Host it does not serve.
	req.Host = target.Upstream.Host

	req.Header.Set(HeaderOriginalHost, original)
	req.Header.Set(HeaderService, target.Service)
}

// annotate adds sandbox identity to an outgoing request.
func (s *Server) annotate(req *http.Request) {
	req.Header.Set(HeaderSandbox, s.cfg.SandboxID)
	if s.cfg.EnvID != "" {
		req.Header.Set(HeaderEnv, s.cfg.EnvID)
	}
	if s.cfg.Upstream.AuthValueEnv != "" {
		if v := os.Getenv(s.cfg.Upstream.AuthValueEnv); v != "" {
			req.Header.Set("Authorization", "Bearer "+v)
		}
	}
}

// mintCanary produces a token unique to this process.
func mintCanary() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail in practice, and a proxy that cannot
		// identify itself is worse than one that will not start.
		panic("veris-proxy: cannot generate a canary token: " + err.Error())
	}
	return "cnry_" + hex.EncodeToString(buf)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func joinPath(base, rest string) string {
	base = strings.TrimSuffix(base, "/")
	if rest == "" {
		return base
	}
	if !strings.HasPrefix(rest, "/") {
		rest = "/" + rest
	}
	return base + rest
}

// blockResponse is what the code under test sees when it tries to reach a host
// with no mapping. It is written to be read by a developer staring at a failed
// test, so it names the host and says exactly what to do about it.
func (s *Server) blockResponse(req *http.Request, host string) *http.Response {
	body, _ := json.MarshalIndent(map[string]any{
		"error":   "veris_unmapped_host",
		"message": fmt.Sprintf("%s is not mapped to a Veris simulated service, and the proxy is in strict mode.", host),
		"host":    host,
		"remedy": []string{
			fmt.Sprintf("Add %s to a service's hosts in veris.yaml, then re-run `veris proxy sync`.", host),
			"Or add it to allow_passthrough if it is genuinely not a simulated dependency.",
		},
		"sandbox_id":     s.cfg.SandboxID,
		"known_services": s.serviceNames(),
	}, "", "  ")

	return newJSONResponse(req, http.StatusBadGateway, body)
}

func (s *Server) controlResponse(req *http.Request) *http.Response {
	if req.URL.Path == CanaryPath {
		s.canaryHits.Add(1)
	}
	body, _ := json.MarshalIndent(s.state(req.URL.Path), "", "  ")
	return newJSONResponse(req, http.StatusOK, body)
}

// serveDirect handles requests sent straight to the proxy's listen address
// rather than through it.
func (s *Server) serveDirect(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case CanaryPath:
		s.canaryHits.Add(1)
	case StatusPath:
	default:
		http.Error(w, "veris-proxy: configure this address as an HTTP proxy, "+
			"or request "+StatusPath+" for status", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = writeJSONBody(w, s.state(req.URL.Path))
}

func writeJSONBody(w io.Writer, body any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

// state is the payload for both control endpoints. The canary token is the
// part that matters: a test asserting on it proves it is talking to the proxy
// started for *this* run, not a stale one left over from an earlier run with a
// different sandbox.
func (s *Server) state(path string) map[string]any {
	out := map[string]any{
		"ok":             true,
		"veris_proxy":    true,
		"version":        s.version,
		"sandbox_id":     s.cfg.SandboxID,
		"mode":           string(s.cfg.Mode),
		"pid":            os.Getpid(),
		"ca_fingerprint": s.ca.Fingerprint(),
		"uptime_seconds": int(time.Since(s.started).Seconds()),
		"services":       s.serviceNames(),
	}
	if s.cfg.EnvID != "" {
		out["env_id"] = s.cfg.EnvID
	}
	if s.cfg.CanaryToken != "" {
		out["canary_token"] = s.cfg.CanaryToken
	}
	if path == StatusPath {
		out["counters"] = map[string]int64{
			"intercepted":  s.intercepted.Load(),
			"blocked":      s.blocked.Load(),
			"passthrough":  s.passedvia.Load(),
			"canary_hits":  s.canaryHits.Load(),
			"cached_certs": int64(s.ca.CacheLen()),
		}
		// The receipt too, so a caller outside this process can enforce a
		// --require-service. `run` reads it in-process when it owns the proxy;
		// when the proxy is in another container it has to come over the wire.
		out["receipt"] = s.receiptSnapshot()
		// And the inbound one, for exactly the same reason: with the proxy in
		// another container, `run --image` can only enforce --require-callback
		// over the wire.
		if in := s.inbound.Load(); in != nil {
			out["inbound_receipt"] = (*in).Receipt()
		}
	}
	return out
}

func (s *Server) serviceNames() []string {
	names := make([]string, 0, len(s.cfg.Services))
	for _, svc := range s.cfg.Services {
		names = append(names, svc.Name)
	}
	return names
}

func newJSONResponse(req *http.Request, status int, body []byte) *http.Response {
	resp := goproxy.NewResponse(req, "application/json", status, string(body))
	resp.Header.Set("X-Veris-Proxy", "1")
	return resp
}

// certStore adapts our CA onto goproxy's cache interface. goproxy passes a
// generator we deliberately ignore, because its default signer produces a bare
// leaf with no chain.
type certStore struct{ ca *ca.CA }

func (c certStore) Fetch(hostname string, _ func() (*tls.Certificate, error)) (*tls.Certificate, error) {
	return c.ca.Leaf(hostname)
}

// slogAdapter lets goproxy's Printf-style logging land in our structured log at
// debug level, where it stays out of the way unless someone is debugging.
type slogAdapter struct{ log *slog.Logger }

func (a slogAdapter) Printf(format string, v ...any) {
	a.log.Debug(strings.TrimSpace(fmt.Sprintf(format, v...)), "source", "goproxy")
}
