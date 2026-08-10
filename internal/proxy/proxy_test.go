package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/ca"
	"github.com/veris-ai/veris-proxy/internal/config"
)

// harness wires a fake sandbox origin, a proxy, and a client configured the way
// code under test would be.
type harness struct {
	origin   *httptest.Server
	proxyURL string
	client   *http.Client
	requests chan *http.Request
	roots    *x509.CertPool
}

func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	seen := make(chan *http.Request, 16)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		select {
		case seen <- clone:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"origin": "sandbox",
			"path":   r.URL.Path,
			"host":   r.Host,
		})
	}))
	t.Cleanup(origin.Close)

	cfg := &config.Config{
		Version:   1,
		Listen:    "127.0.0.1:0",
		Mode:      config.ModeStrict,
		SandboxID: "sbx_test",
		EnvID:     "env_test",
		Upstream:  config.Upstream{BaseURL: origin.URL},
		Services: []config.Service{
			{Name: "stripe", Hosts: []string{"api.stripe.com", "*.stripe.com"}},
		},
		CanaryToken: "canary-abc123",
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}

	authority, err := ca.Load(t.TempDir())
	if err != nil {
		t.Fatalf("ca.Load: %v", err)
	}

	srv := New(cfg, authority, slog.New(slog.DiscardHandler), "test")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = http.Serve(ln, srv.Handler()) }()

	proxyURL := "http://" + ln.Addr().String()
	pu, _ := url.Parse(proxyURL)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(pu),
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
	}

	return &harness{origin: origin, proxyURL: proxyURL, client: client, requests: seen, roots: roots}
}

func (h *harness) get(t *testing.T, rawurl string) (*http.Response, []byte) {
	t.Helper()
	resp, err := h.client.Get(rawurl)
	if err != nil {
		t.Fatalf("GET %s: %v", rawurl, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// The headline behaviour: HTTPS to a real hostname is intercepted, the TLS
// handshake succeeds against the Veris CA, and the request lands on the
// sandbox instead of the real API.
func TestInterceptsHTTPSAndRewritesToSandbox(t *testing.T) {
	h := newHarness(t, nil)

	resp, body := h.get(t, "https://api.stripe.com/v1/charges?limit=3")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("origin response was not JSON: %s", body)
	}
	if got["origin"] != "sandbox" {
		t.Fatalf("request did not reach the sandbox origin: %v", got)
	}

	select {
	case req := <-h.requests:
		if want := "/s/sbx_test/stripe/v1/charges"; req.URL.Path != want {
			t.Errorf("upstream path = %q, want %q", req.URL.Path, want)
		}
		if got := req.Header.Get(HeaderOriginalHost); got != "api.stripe.com" {
			t.Errorf("%s = %q, want api.stripe.com", HeaderOriginalHost, got)
		}
		if got := req.Header.Get(HeaderService); got != "stripe" {
			t.Errorf("%s = %q, want stripe", HeaderService, got)
		}
		if got := req.Header.Get(HeaderSandbox); got != "sbx_test" {
			t.Errorf("%s = %q, want sbx_test", HeaderSandbox, got)
		}
		if req.URL.RawQuery != "limit=3" {
			t.Errorf("query string lost in rewrite: %q", req.URL.RawQuery)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("origin never received the request")
	}
}

// The property the whole product depends on: an unmapped host must fail, not
// quietly reach the real internet. A green test run against real Stripe would
// be worse than a red one.
func TestStrictModeBlocksUnmappedHost(t *testing.T) {
	h := newHarness(t, nil)

	resp, body := h.get(t, "https://api.openai.com/v1/models")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", resp.StatusCode, body)
	}

	var e struct {
		Error  string   `json:"error"`
		Host   string   `json:"host"`
		Remedy []string `json:"remedy"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("block response was not JSON: %s", body)
	}
	if e.Error != "veris_unmapped_host" {
		t.Errorf("error = %q, want veris_unmapped_host", e.Error)
	}
	if e.Host != "api.openai.com" {
		t.Errorf("host = %q, want api.openai.com", e.Host)
	}
	if len(e.Remedy) == 0 {
		t.Error("block response must tell the developer what to do about it")
	}
}

func TestWildcardHostIsIntercepted(t *testing.T) {
	h := newHarness(t, nil)

	resp, body := h.get(t, "https://files.stripe.com/v1/files")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	select {
	case req := <-h.requests:
		if want := "/s/sbx_test/stripe/v1/files"; req.URL.Path != want {
			t.Errorf("path = %q, want %q", req.URL.Path, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("origin never received the wildcard-matched request")
	}
}

func TestPlainHTTPIsAlsoIntercepted(t *testing.T) {
	h := newHarness(t, nil)

	resp, body := h.get(t, "http://api.stripe.com/v1/charges")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if len(body) == 0 {
		t.Fatal("empty body from origin")
	}
}

func TestCanaryIsServedThroughTheProxy(t *testing.T) {
	h := newHarness(t, nil)

	// Deliberately requested on an unmapped host: the canary must answer
	// regardless of routing, since its whole job is to prove the proxy is in
	// the path at all.
	resp, body := h.get(t, "https://api.example.invalid"+CanaryPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}

	var state struct {
		VerisProxy  bool   `json:"veris_proxy"`
		SandboxID   string `json:"sandbox_id"`
		CanaryToken string `json:"canary_token"`
		Mode        string `json:"mode"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("canary response was not JSON: %s", body)
	}
	if !state.VerisProxy {
		t.Error("canary must identify itself as a Veris proxy")
	}
	if state.CanaryToken != "canary-abc123" {
		t.Errorf("canary_token = %q, want canary-abc123", state.CanaryToken)
	}
	if state.SandboxID != "sbx_test" {
		t.Errorf("sandbox_id = %q", state.SandboxID)
	}
	if state.Mode != "strict" {
		t.Errorf("mode = %q, want strict", state.Mode)
	}
}

func TestStatusReportsCounters(t *testing.T) {
	h := newHarness(t, nil)

	h.get(t, "https://api.stripe.com/one")
	h.get(t, "https://api.openai.com/two")

	resp, body := h.get(t, "https://anything.invalid"+StatusPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var state struct {
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatalf("status response was not JSON: %s", body)
	}
	if state.Counters["intercepted"] != 1 {
		t.Errorf("intercepted = %d, want 1", state.Counters["intercepted"])
	}
	if state.Counters["blocked"] != 1 {
		t.Errorf("blocked = %d, want 1", state.Counters["blocked"])
	}
}

// A test that starts its own service and calls it over loopback must not be
// routed to the sandbox.
func TestLoopbackIsPassedThroughUnmodified(t *testing.T) {
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "service-under-test")
	}))
	defer local.Close()

	h := newHarness(t, nil)
	resp, body := h.get(t, local.URL+"/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if string(body) != "service-under-test" {
		t.Fatalf("loopback response = %q; the proxy interfered with it", body)
	}
}

func TestPassthroughModeForwardsUnmappedHosts(t *testing.T) {
	// In passthrough mode an unmapped host should not be blocked. We point it
	// at a loopback origin so the test stays hermetic.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "real-internet")
	}))
	defer other.Close()

	h := newHarness(t, func(c *config.Config) { c.Mode = config.ModePassthrough })

	resp, body := h.get(t, other.URL+"/x")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if string(body) != "real-internet" {
		t.Fatalf("body = %q", body)
	}
}

// Requesting the status endpoint directly, rather than through the proxy, is
// how a health check or a container readiness probe reaches it.
func TestStatusIsReachableWithoutProxyConfiguration(t *testing.T) {
	h := newHarness(t, nil)

	resp, err := http.Get(h.proxyURL + StatusPath)
	if err != nil {
		t.Fatalf("direct GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var state map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if state["veris_proxy"] != true {
		t.Fatal("direct status request must identify the proxy")
	}
}

func TestDirectRequestToUnknownPathExplainsItself(t *testing.T) {
	h := newHarness(t, nil)

	resp, err := http.Get(h.proxyURL + "/")
	if err != nil {
		t.Fatalf("direct GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// Someone who curls the proxy address directly should be told why it did
	// not work, not handed a bare 400.
	if len(body) == 0 {
		t.Fatal("expected an explanatory message")
	}
}

// The explicit-proxy tier tunnels through CONNECT, which is a different code
// path from the transparent listeners: goproxy terminates the tunnel's TLS
// itself. It leaves h2 off unless asked, so this proves the ask took.
func TestTheCONNECTTunnelNegotiatesHTTP2(t *testing.T) {
	h := newHarness(t, nil)

	proxyURL, err := url.Parse(h.proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{
		Proxy:             http.ProxyURL(proxyURL),
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			RootCAs:    h.roots,
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2", "http/1.1"},
		},
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}

	resp, err := client.Get("https://api.stripe.com/v1/charges")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.ProtoMajor != 2 {
		t.Fatalf("negotiated %s through CONNECT, want HTTP/2", resp.Proto)
	}
}
