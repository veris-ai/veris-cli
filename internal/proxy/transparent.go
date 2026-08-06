package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Transparent mode serves connections that were redirected by the kernel
// (iptables REDIRECT) rather than sent to us by a client that knows it is
// using a proxy.
//
// This is the mode that makes the container tier work. Nothing in the process
// under test has to cooperate: no HTTP_PROXY, no per-library configuration. It
// covers Java, static Go binaries, Apache HttpClient and aiohttp, all of which
// are unreachable through environment variables.
//
// The destination cannot be read from the request line here, because a
// redirected connection carries no absolute URL. For TLS we take it from the
// SNI in the ClientHello; for plaintext we take it from the Host header.

// ServeTransparent runs the plaintext and TLS transparent listeners until one
// of them fails.
func (s *Server) ServeTransparent(httpAddr, httpsAddr string) error {
	errCh := make(chan error, 2)

	go func() { errCh <- s.serveTransparentHTTP(httpAddr) }()
	go func() { errCh <- s.serveTransparentTLS(httpsAddr) }()

	return <-errCh
}

func (s *Server) serveTransparentHTTP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("transparent http listen on %s: %w", addr, err)
	}
	s.log.Info("transparent http listener", "addr", ln.Addr().String())

	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.forward(w, r, "http") }),
		ReadHeaderTimeout: 30 * time.Second,
	}
	return srv.Serve(ln)
}

func (s *Server) serveTransparentTLS(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("transparent tls listen on %s: %w", addr, err)
	}
	s.log.Info("transparent tls listener", "addr", ln.Addr().String())
	return s.serveTransparentTLSOn(ln)
}

// serveTransparentTLSOn accepts on an already-bound listener.
func (s *Server) serveTransparentTLSOn(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			s.log.Warn("transparent accept failed", "err", err)
			continue
		}
		go s.handleTransparentTLS(conn)
	}
}

func (s *Server) handleTransparentTLS(raw net.Conn) {
	defer raw.Close()

	// GetCertificate gives us the SNI, so we do not need to peek the
	// ClientHello by hand: crypto/tls hands it to us during the handshake.
	var sni string
	tlsConn := tls.Server(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			sni = hello.ServerName
			if sni == "" {
				// A client that omits SNI gives us nothing to route on. This
				// is rare outside of very old clients and raw-IP calls.
				return nil, errors.New("no SNI: cannot determine the intended destination")
			}
			return s.ca.Leaf(sni)
		},
	})

	if err := tlsConn.Handshake(); err != nil {
		s.log.Debug("transparent tls handshake failed", "sni", sni, "err", err)
		return
	}
	defer tlsConn.Close()

	// Now it is ordinary HTTP over a socket we control.
	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF {
				s.log.Debug("transparent read request", "sni", sni, "err", err)
			}
			return
		}
		// A redirected request has no absolute URL; reconstruct the authority
		// from the SNI, falling back to the Host header.
		if req.Host == "" {
			req.Host = sni
		}
		req.URL.Scheme = "https"
		req.URL.Host = req.Host

		rec := &connResponseWriter{conn: tlsConn, header: http.Header{}, req: req}
		s.forward(rec, req, "https")
		if err := rec.finish(); err != nil {
			return
		}
		if req.Close || rec.closed {
			return
		}
	}
}

// forward applies the same routing decision as the explicit-proxy path, then
// performs the request itself.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, scheme string) {
	if r.URL.Path == CanaryPath || r.URL.Path == StatusPath {
		if r.URL.Path == CanaryPath {
			s.canaryHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.state(r.URL.Path))
		return
	}

	host := hostOnly(firstNonEmpty(r.Host, r.URL.Host))

	if s.cfg.IsPassthrough(host) {
		s.passedvia.Add(1)
		// Passthrough in transparent mode would mean re-dialling the original
		// destination, which the kernel has already redirected away from us.
		// Rather than guess, refuse clearly: the container runner is expected
		// to exclude passthrough hosts from the redirect rules instead.
		s.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "veris_passthrough_in_transparent_mode",
			"message": fmt.Sprintf(
				"%s is configured for passthrough, but it was redirected to the proxy anyway.", host),
			"remedy": "Exclude this host from the iptables redirect rules rather than from proxy config.",
		})
		return
	}

	target, ok := s.cfg.Resolve(host)
	if !ok {
		s.blocked.Add(1)
		s.log.Error("blocked unmapped host", "host", host, "method", r.Method, "path", r.URL.Path, "mode", "transparent")
		s.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":          "veris_unmapped_host",
			"message":        fmt.Sprintf("%s is not mapped to a Veris simulated service.", host),
			"host":           host,
			"sandbox_id":     s.cfg.SandboxID,
			"known_services": s.serviceNames(),
		})
		return
	}

	outbound := r.Clone(r.Context())
	outbound.RequestURI = ""
	outbound.URL.Scheme = scheme
	outbound.URL.Host = host
	rewriteTo(outbound, target)
	s.annotate(outbound)

	s.intercepted.Add(1)
	s.log.Info("intercepted",
		"service", target.Service, "host", host, "method", r.Method,
		"path", outbound.URL.Path, "upstream", target.Upstream.Host, "mode", "transparent")

	resp, err := s.upstreamClient().Do(outbound)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":   "veris_upstream_unreachable",
			"message": fmt.Sprintf("could not reach the Veris sandbox for service %q: %v", target.Service, err),
		})
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Veris-Proxy", "1")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) upstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// Never chain through an ambient proxy: in the container the
			// environment deliberately points at us.
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: s.cfg.Upstream.InsecureSkipVerify, //nolint:gosec // operator opt-in
			},
		},
		// Redirects are the upstream's business, not ours; passing them back
		// keeps the client's own redirect policy in charge.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       120 * time.Second,
	}
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Veris-Proxy", "1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// connResponseWriter adapts a raw TLS connection to http.ResponseWriter, which
// is what lets the transparent TLS path reuse the same handler as the
// plaintext one.
type connResponseWriter struct {
	conn    net.Conn
	req     *http.Request
	header  http.Header
	status  int
	body    []byte
	written bool
	closed  bool
}

func (c *connResponseWriter) Header() http.Header { return c.header }

func (c *connResponseWriter) WriteHeader(status int) {
	if !c.written {
		c.status = status
		c.written = true
	}
}

func (c *connResponseWriter) Write(p []byte) (int, error) {
	if !c.written {
		c.WriteHeader(http.StatusOK)
	}
	c.body = append(c.body, p...)
	return len(p), nil
}

// finish serialises the buffered response onto the connection. Buffering keeps
// this simple and is fine for API traffic, which is what a dependency sandbox
// serves; it would need streaming for large payloads.
func (c *connResponseWriter) finish() error {
	if !c.written {
		c.WriteHeader(http.StatusOK)
	}
	resp := &http.Response{
		StatusCode:    c.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        c.header,
		Body:          io.NopCloser(bytes.NewReader(c.body)),
		ContentLength: int64(len(c.body)),
		Request:       c.req,
	}
	return resp.Write(c.conn)
}
