package proxy

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/veris-ai/veris-proxy/internal/config"
	"github.com/veris-ai/veris-proxy/internal/hostport"
)

// This file serves connections the kernel redirected to us (iptables REDIRECT)
// rather than ones a client chose to send. There is no absolute URL in the
// request line and no CONNECT to read the authority from, and nothing in the
// process under test cooperates -- which is what covers Java, static Go
// binaries, Apache HttpClient and aiohttp.
//
// For TLS the destination comes from the SNI, which is authoritative: it is
// what we issued the certificate for. For plaintext it comes from the Host
// header, which is all there is.

// interceptMode labels the path a request took, for logs and the receipt.
const modeTransparent = "transparent"

// serverTLSConfig is shared by every TLS listener, so session tickets and the
// certificate cache are shared rather than rebuilt per connection.
func (s *Server) serverTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == "" {
				// Without SNI there is nothing to route on and nothing to name
				// in a certificate. Old clients and raw-IP calls land here.
				return nil, errors.New("no SNI: cannot determine the intended destination")
			}
			return s.ca.Leaf(hello.ServerName)
		},
		// Offer h2 first. Google, Stripe and most large vendors serve HTTP/2,
		// so a client that negotiated h2 against the real API and HTTP/1.1
		// here is exercising a different transport than it does in production
		// -- different multiplexing, different header handling, and a
		// different set of client-library code paths.
		NextProtos: []string{"h2", "http/1.1"},
	}
}

// interceptHandler serves one of the non-proxy listeners.
func (s *Server) interceptHandler(isTLS bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		// Hostnames are case-insensitive, so normalize before comparing or
		// routing: a client may send SNI and Host in different cases.
		host := strings.ToLower(hostport.StripPort(r.Host))

		if isTLS {
			scheme = "https"
			sni := ""
			if r.TLS != nil {
				sni = strings.ToLower(r.TLS.ServerName)
			}
			if sni == "" {
				s.writeJSON(w, http.StatusBadGateway, map[string]any{
					"error":   "veris_no_sni",
					"message": "the TLS handshake carried no SNI, so the intended destination is unknown",
				})
				return
			}
			// The certificate was issued for the SNI, so routing on anything
			// else would let a client handshake for one host and be routed as
			// another.
			if host != "" && host != sni {
				s.log.Warn("refused SNI/Host mismatch", "sni", sni, "host", host)
				s.writeJSON(w, http.StatusBadGateway, map[string]any{
					"error": "veris_sni_host_mismatch",
					"message": fmt.Sprintf(
						"the TLS handshake was for %s but the request claims Host %s", sni, host),
				})
				return
			}
			host = sni
		}

		if host == "" {
			s.writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":   "veris_no_host",
				"message": "the request carried no Host header, so the intended destination is unknown",
			})
			return
		}
		s.forward(w, r, scheme, host)
	})
}

// forward applies the same routing decision as the explicit-proxy path, then
// performs the request itself.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, scheme, host string) {
	if r.URL.Path == CanaryPath || r.URL.Path == StatusPath {
		if r.URL.Path == CanaryPath {
			s.canaryHits.Add(1)
		}
		s.writeJSON(w, http.StatusOK, s.state(r.URL.Path))
		return
	}

	if s.cfg.IsPassthrough(host) {
		s.passedvia.Add(1)
		// Reaching the real host would mean re-dialling a destination we can no
		// longer address: the kernel redirected it away from us, or DNS points
		// the name at us. Guessing would be worse than saying so.
		s.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "veris_passthrough_unreachable",
			"message": fmt.Sprintf(
				"%s is configured for passthrough, but it reached the proxy anyway.", host),
			"remedy": "Exclude this host from the iptables redirect rules rather " +
				"than from proxy config: the redirect happens before DNS.",
		})
		return
	}

	outbound := r.Clone(r.Context())
	outbound.RequestURI = ""
	outbound.URL.Scheme = scheme
	outbound.URL.Host = host
	outbound.Host = host

	target, ok := s.cfg.Resolve(host, r.URL.Path)
	switch {
	case ok:
		s.recordIntercept(host, target, modeTransparent, r.Method, r.URL.Path)
		rewriteTo(outbound, target)
		s.annotate(outbound)

	case s.cfg.Mode == config.ModePassthrough:
		// Forward to the REAL host, unrewritten. This works here for the same
		// reason the redirect does not loop: the proxy's own egress is exempt
		// by uid, so it can still reach a destination the kernel redirected
		// away from everything else.
		//
		// Without this the container tier behaved as strict no matter what the
		// mode said, so a package install or a telemetry call was refused by a
		// proxy configured to let it through.
		s.blocked.Add(1)
		s.log.Warn("unmapped host forwarded to the real internet",
			"host", host, "path", r.URL.Path,
			"hint", "mode=passthrough; set mode=strict before trusting a green run")

	default:
		s.blocked.Add(1)
		s.log.Error("blocked unmapped host",
			"host", host, "method", r.Method, "path", r.URL.Path, "mode", modeTransparent)
		s.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":          "veris_unmapped_host",
			"message":        fmt.Sprintf("%s is not mapped to a Veris simulated service.", host),
			"host":           host,
			"sandbox_id":     s.cfg.SandboxID,
			"known_services": s.serviceNames(),
		})
		return
	}

	resp, err := s.upstream.Do(outbound)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":   "veris_upstream_unreachable",
			"message": fmt.Sprintf("could not reach %s: %v", outbound.URL.Host, err),
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
	// Streamed rather than buffered, so a large download or an SSE stream is
	// not held whole in memory.
	_, _ = io.Copy(w, resp.Body)
}

// newUpstreamClient is built ONCE per server, not per request: a client
// constructed per request gets a fresh connection pool every time, so nothing
// is ever reused and HTTP/2 -- whose whole benefit is multiplexing over one
// connection -- degrades to a new handshake per call.
func newUpstreamClient(insecure bool) *http.Client {
	transport := &http.Transport{
		// Never chain through an ambient proxy: in the container the
		// environment deliberately points at us.
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: insecure, //nolint:gosec // operator opt-in
		},
		// Setting TLSClientConfig by hand disables net/http's automatic HTTP/2
		// upgrade, so it has to be asked for explicitly. Without this the
		// sandbox leg is HTTP/1.1 even when the client's leg negotiated h2.
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{
		Transport: transport,
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
	_ = writeJSONBody(w, body)
}
