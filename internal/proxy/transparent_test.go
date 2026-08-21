package proxy

import (
	"bufio"
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
	running   *Running
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

	running, err := srv.Start(ListenOptions{
		TransparentHTTP:  "127.0.0.1:0",
		TransparentHTTPS: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = running.Shutdown(ctx)
	})

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}

	return &transparentHarness{
		httpsAddr: running.Addr("transparent-https"),
		httpAddr:  running.Addr("transparent-http"),
		roots:     roots,
		requests:  seen,
		running:   running,
	}
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

	// Same status as the explicit-proxy tier: 421, which no HTTP client retries.
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421; body = %s", resp.StatusCode, body)
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

// The certificate is issued for the SNI, so a request that handshakes as one
// host and then claims another in its Host header must be refused rather than
// routed on the Host. Otherwise a client could reach any mapped service
// through a certificate we issued for a different one.
func TestTransparentRefusesAnSNIHostMismatch(t *testing.T) {
	h := newTransparentHarness(t)

	conn, err := tls.Dial("tcp", h.httpsAddr, &tls.Config{
		ServerName: "api.stripe.com",
		RootCAs:    h.roots,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://api.stripe.com/v1/charges", nil)
	req.Host = "api.openai.com"
	if err := req.Write(conn); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error != "veris_sni_host_mismatch" {
		t.Fatalf("error = %q (status %d), want veris_sni_host_mismatch; body = %s",
			e.Error, resp.StatusCode, body)
	}
}

// Hostnames are case-insensitive, so a client sending SNI and Host in
// different cases is making a perfectly ordinary request, not an attack.
func TestTransparentAcceptsAHostThatDiffersFromSNIOnlyInCase(t *testing.T) {
	h := newTransparentHarness(t)

	conn, err := tls.Dial("tcp", h.httpsAddr, &tls.Config{
		ServerName: "api.stripe.com",
		RootCAs:    h.roots,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://api.stripe.com/v1/charges", nil)
	req.Host = "API.Stripe.COM"
	if err := req.Write(conn); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}
}

// The receipt is what proves a run reached the sandbox. The canary only proves
// interception was live before the run started.
func TestTheReceiptRecordsWhatTheRunSent(t *testing.T) {
	h := newTransparentHarness(t)

	if got := h.running.Receipt().Total; got != 0 {
		t.Fatalf("a run that sent nothing has receipt total %d, want 0", got)
	}

	tr := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: h.roots, MinVersion: tls.VersionTLS12},
		DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", h.httpsAddr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	for range 3 {
		resp, err := client.Get("https://api.stripe.com/v1/charges")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// A blocked host is not a hit: the sandbox never saw it.
	resp, err := client.Get("https://api.openai.com/v1/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// A control-plane read reaches the sandbox but is the HARNESS's traffic:
	// it lands beside, never inside, the vendor-surface counts.
	resp, err = client.Get("https://api.stripe.com/veris/data")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	got := h.running.Receipt()
	if got.Total != 3 {
		t.Errorf("total = %d, want 3", got.Total)
	}
	if got.ByService["stripe"] != 3 {
		t.Errorf("by_service[stripe] = %d, want 3", got.ByService["stripe"])
	}
	if got.ByHost["api.openai.com"] != 0 {
		t.Errorf("a blocked host was counted as a hit: %+v", got.ByHost)
	}
	if got.ControlTotal != 1 || got.ByServiceControl["stripe"] != 1 {
		t.Errorf("control-plane read not counted apart: control_total=%d by_service_control=%+v",
			got.ControlTotal, got.ByServiceControl)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("hits = %+v, want the vendor entry plus the control entry", got.Hits)
	}
	if got.Hits[0].Host != "api.stripe.com" || got.Hits[0].Prefix != "/" || got.Hits[0].Control {
		t.Errorf("hits[0] = %+v, want the vendor-surface api.stripe.com entry", got.Hits[0])
	}
	if !got.Hits[1].Control {
		t.Errorf("hits[1] = %+v, want the control-plane entry flagged", got.Hits[1])
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

// HTTP/2 is what the large vendors actually serve, so a client that negotiates
// h2 against real Stripe or Google must negotiate it here too. Otherwise the
// transport under test is not the transport that ships: different multiplexing,
// different header handling, different client-library code paths.
func TestTheTLSListenerNegotiatesHTTP2(t *testing.T) {
	h := newTransparentHarness(t)

	tr := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			RootCAs:    h.roots,
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
		},
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
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s, want HTTP/2 -- ALPN offered h2 but the server "+
			"did not install an h2 handler", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d over h2", resp.StatusCode)
	}
}

// An h2 request must be routed and recorded exactly like an HTTP/1.1 one; the
// receipt and the rewrite must not depend on the transport.
func TestAnHTTP2RequestIsInterceptedAndRecorded(t *testing.T) {
	h := newTransparentHarness(t)

	tr := &http.Transport{
		Proxy:             nil,
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			RootCAs:    h.roots,
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2"},
		},
		DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", h.httpsAddr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	resp, err := client.Get("https://api.stripe.com/v1/charges")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("did not negotiate h2: %s", resp.Proto)
	}

	select {
	case req := <-h.requests:
		if want := "/s/sbx_transparent/stripe/v1/charges"; req.URL.Path != want {
			t.Errorf("upstream path = %q, want %q", req.URL.Path, want)
		}
		if got := req.Header.Get(HeaderOriginalHost); got != "api.stripe.com" {
			t.Errorf("%s = %q", HeaderOriginalHost, got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the h2 request never reached the sandbox")
	}

	if got := h.running.Receipt().ByService["stripe"]; got != 1 {
		t.Errorf("receipt recorded %d stripe hits over h2, want 1", got)
	}
}

// Passthrough has to mean the same thing in both tiers. It did not: the
// transparent path always blocked an unmapped host, so a container behaved as
// strict no matter what the mode said -- and passthrough is the default, so a
// package install or a telemetry call was refused by a proxy configured to let
// it through.
//
// Asserted by the DIFFERENCE between the two modes on one unmapped, unroutable
// host: strict refuses it by name without trying, passthrough tries to reach
// it and reports that it could not. Only the second means the request was
// forwarded rather than blocked.
func TestTransparentHonoursPassthroughMode(t *testing.T) {
	probe := func(mode config.Mode) map[string]any {
		t.Helper()
		h := newTransparentHarness(t)
		h.running.srv.cfg.Mode = mode

		tr := &http.Transport{
			Proxy: nil,
			DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
				return net.Dial("tcp", h.httpAddr)
			},
		}
		client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
		// Unmapped, and not loopback -- loopback is always passed through for
		// its own reasons and would not exercise this.
		resp, err := client.Get("http://unmapped.veris-test.invalid/anything")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		var out map[string]any
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("not JSON: %s", body)
		}
		if got := h.running.Receipt().Total; got != 0 {
			t.Errorf("an unmapped host was recorded as a sandbox hit (%d)", got)
		}
		return out
	}

	if got := probe(config.ModeStrict)["error"]; got != "veris_unmapped_host" {
		t.Errorf("strict: error = %v, want veris_unmapped_host", got)
	}
	// Not blocked: it was sent, and the failure is the destination's absence.
	if got := probe(config.ModePassthrough)["error"]; got != "veris_upstream_unreachable" {
		t.Errorf("passthrough: error = %v, want veris_upstream_unreachable "+
			"(i.e. it tried to forward rather than refusing)", got)
	}
}
