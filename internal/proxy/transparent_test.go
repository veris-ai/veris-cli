package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/ca"
	"github.com/veris-ai/veris-proxy/internal/config"
)

// Transparent mode is what makes the container tier work, so it is worth
// testing the way it is actually used: a client that has been given no proxy
// configuration whatsoever, whose connection the kernel redirected.
//
// We simulate the kernel redirect by dialling the transparent listener
// directly while still presenting the original hostname in SNI, which is
// exactly what the socket looks like after an iptables REDIRECT.
type transparentHarness struct {
	httpsAddr string
	httpAddr  string
	roots     *x509.CertPool
	requests  chan *http.Request
}

func newTransparentHarness(t *testing.T) *transparentHarness {
	t.Helper()

	seen := make(chan *http.Request, 16)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		select {
		case seen <- clone:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"origin": "sandbox", "path": r.URL.Path})
	}))
	t.Cleanup(origin.Close)

	cfg := &config.Config{
		Version:   1,
		Listen:    "127.0.0.1:0",
		Mode:      config.ModeStrict,
		SandboxID: "sbx_transparent",
		Upstream:  config.Upstream{BaseURL: origin.URL},
		Services:  []config.Service{{Name: "stripe", Hosts: []string{"api.stripe.com"}}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}

	authority, err := ca.Load(t.TempDir())
	if err != nil {
		t.Fatalf("ca.Load: %v", err)
	}
	srv := New(cfg, authority, slog.New(slog.DiscardHandler), "test")

	httpsLn := mustListen(t)
	httpLn := mustListen(t)
	go srv.serveTransparentTLSOn(httpsLn)
	go func() {
		_ = (&http.Server{
			Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { srv.forward(w, r, "http") }),
			ReadHeaderTimeout: 5 * time.Second,
		}).Serve(httpLn)
	}()

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}

	return &transparentHarness{
		httpsAddr: httpsLn.Addr().String(),
		httpAddr:  httpLn.Addr().String(),
		roots:     roots,
		requests:  seen,
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// netContext is the context type http.Transport.DialContext expects. Aliased
// here to keep the dial hooks below readable.
type netContext = context.Context

func TestTransparentTLSInterceptsWithoutProxyConfig(t *testing.T) {
	h := newTransparentHarness(t)

	tr := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: h.roots, MinVersion: tls.VersionTLS12},
		DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", h.httpsAddr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get("https://api.stripe.com/v1/charges")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	select {
	case req := <-h.requests:
		if want := "/s/sbx_transparent/stripe/v1/charges"; req.URL.Path != want {
			t.Errorf("upstream path = %q, want %q", req.URL.Path, want)
		}
		if got := req.Header.Get(HeaderOriginalHost); got != "api.stripe.com" {
			t.Errorf("%s = %q, want api.stripe.com", HeaderOriginalHost, got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("origin never received the redirected request")
	}
}

func TestTransparentBlocksUnmappedHost(t *testing.T) {
	h := newTransparentHarness(t)

	tr := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: h.roots, MinVersion: tls.VersionTLS12},
		DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", h.httpsAddr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get("https://api.openai.com/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", resp.StatusCode, body)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	if e.Error != "veris_unmapped_host" {
		t.Errorf("error = %q, want veris_unmapped_host", e.Error)
	}
}

func TestTransparentPlaintextIntercepts(t *testing.T) {
	h := newTransparentHarness(t)

	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", h.httpAddr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get("http://api.stripe.com/v1/charges")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	select {
	case req := <-h.requests:
		if want := "/s/sbx_transparent/stripe/v1/charges"; req.URL.Path != want {
			t.Errorf("upstream path = %q, want %q", req.URL.Path, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("origin never received the plaintext request")
	}
}

func TestTransparentCanaryIsReachable(t *testing.T) {
	h := newTransparentHarness(t)

	tr := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: h.roots, MinVersion: tls.VersionTLS12},
		DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", h.httpsAddr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get("https://api.stripe.com" + CanaryPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var state struct {
		VerisProxy bool   `json:"veris_proxy"`
		SandboxID  string `json:"sandbox_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !state.VerisProxy || state.SandboxID != "sbx_transparent" {
		t.Fatalf("canary unusable in transparent mode: %+v", state)
	}
}
