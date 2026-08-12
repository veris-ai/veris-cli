package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A trust failure is recorded on the handshake goroutine after the client's
// own dial has already returned, so assertions poll the receipt briefly.
func waitForTrustFailure(t *testing.T, h *transparentHarness, host string,
	cond func(TrustFailure) bool,
) TrustFailure {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range h.running.Receipt().TrustFailures {
			if f.Host == host && cond(f) {
				return f
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no trust failure for %s matched in time; ledger: %+v",
		host, h.running.Receipt().TrustFailures)
	return TrustFailure{}
}

// An SDK that ships its own CA bundle -- stripe-python and friends -- refuses
// the minted leaf during the handshake, before any HTTP exists. That refusal
// arrives here as the client's certificate alert and must be recorded against
// the SNI host at high confidence: without it, a client that refused our
// certificate is indistinguishable from a client that never called.
func TestAClientTrustingOnlyItsOwnBundleIsRecordedAsRejected(t *testing.T) {
	h := newTransparentHarness(t)

	// One dial pinned to TLS 1.2 and two at the default 1.3: in 1.2 the
	// client's alert arrives in cleartext, in 1.3 it arrives encrypted after
	// the ServerHello, and both must classify identically.
	empty := x509.NewCertPool()
	for _, maxVersion := range []uint16{tls.VersionTLS12, 0, 0} {
		conn, err := tls.Dial("tcp", h.httpsAddr, &tls.Config{
			ServerName: "api.stripe.com",
			RootCAs:    empty,
			MinVersion: tls.VersionTLS12,
			MaxVersion: maxVersion,
		})
		if err == nil {
			conn.Close()
			t.Fatal("a client with no roots accepted the minted certificate")
		}
	}

	got := waitForTrustFailure(t, h, "api.stripe.com", func(f TrustFailure) bool {
		return f.Rejected == 3
	})
	if !got.Mapped {
		t.Error("api.stripe.com is a mapped host, recorded as mapped=false")
	}
	if got.Aborted != 0 {
		t.Errorf("a certificate alert was also counted as an abort: %+v", got)
	}
	var reasons int64
	for _, n := range got.Reasons {
		reasons += n
	}
	if reasons != 3 {
		t.Errorf("every rejection must carry its alert reason: %+v", got.Reasons)
	}
}

// A client that trusts the CA and completes its request leaves no entry: the
// ledger records refusals of the minted certificate, not traffic.
func TestATrustedClientLeavesNoTrustFailure(t *testing.T) {
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
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	if got := h.running.Receipt().TrustFailures; len(got) != 0 {
		t.Fatalf("a completed request recorded trust failures: %+v", got)
	}
}

// closingConn lets exactly one write through -- the ClientHello -- and then
// closes the socket underneath the client.
type closingConn struct {
	net.Conn
	once sync.Once
}

func (c *closingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.once.Do(func() { _ = c.Conn.Close() })
	return n, err
}

// Some stacks close the connection instead of alerting when they dislike a
// certificate, and a client that crashed mid-handshake looks exactly the same
// on the wire. Both land as an abort -- never as a confirmed rejection.
func TestAnAbruptCloseAfterClientHelloIsAbortedNotRejected(t *testing.T) {
	h := newTransparentHarness(t)

	raw, err := net.Dial("tcp", h.httpsAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := tls.Client(&closingConn{Conn: raw}, &tls.Config{
		ServerName: "api.stripe.com",
		// Never reached: the socket closes right after the ClientHello, so no
		// certificate is ever verified.
		InsecureSkipVerify: true, //nolint:gosec // see above
		MinVersion:         tls.VersionTLS12,
	})
	if err := client.Handshake(); err == nil {
		t.Fatal("the handshake survived its own socket closing")
	}

	got := waitForTrustFailure(t, h, "api.stripe.com", func(f TrustFailure) bool {
		return f.Aborted >= 1
	})
	if got.Rejected != 0 {
		t.Errorf("an abrupt close was recorded as a confirmed rejection: %+v", got)
	}
}

// The docker tier reads the receipt over the status endpoint, so the trust
// ledger has to travel in it: with the proxy in another container, this JSON
// is the only place the failure can be seen from outside.
func TestTheStatusEndpointCarriesTrustFailures(t *testing.T) {
	h := newTransparentHarness(t)

	if _, err := tls.Dial("tcp", h.httpsAddr, &tls.Config{
		ServerName: "api.stripe.com",
		RootCAs:    x509.NewCertPool(),
		MinVersion: tls.VersionTLS12,
	}); err == nil {
		t.Fatal("a client with no roots accepted the minted certificate")
	}
	waitForTrustFailure(t, h, "api.stripe.com", func(f TrustFailure) bool {
		return f.Rejected == 1
	})

	tr := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: h.roots, MinVersion: tls.VersionTLS12},
		DialContext: func(_ netContext, _, _ string) (net.Conn, error) {
			return net.Dial("tcp", h.httpsAddr)
		},
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.stripe.com" + StatusPath)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()

	var state struct {
		Receipt struct {
			TrustFailures []TrustFailure `json:"tls_trust_failures"`
		} `json:"receipt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(state.Receipt.TrustFailures) != 1 ||
		state.Receipt.TrustFailures[0].Host != "api.stripe.com" ||
		state.Receipt.TrustFailures[0].Rejected != 1 {
		t.Fatalf("status receipt = %+v, want one api.stripe.com rejection",
			state.Receipt.TrustFailures)
	}
}

// The alert names that implicate the certificate's content rather than its
// issuer -- expired is the workload-clock-skew case -- classify as rejections
// with their own reason names; a non-certificate alert stays out entirely.
func TestCertificateContentAlertsClassifyAsRejections(t *testing.T) {
	for text, want := range map[string]string{
		"tls: expired certificate":     "certificate_expired",
		"tls: revoked certificate":     "certificate_revoked",
		"tls: unsupported certificate": "unsupported_certificate",
	} {
		class, reason := classifyHandshake(&net.OpError{
			Op: "remote error", Err: errors.New(text)})
		if class != handshakeTrustRejected || reason != want {
			t.Errorf("%q -> (%v, %q), want (rejected, %q)", text, class, reason, want)
		}
	}
	class, _ := classifyHandshake(&net.OpError{
		Op: "remote error", Err: errors.New("tls: handshake failure")})
	if class != handshakeOther {
		t.Errorf("a non-certificate alert classified as %v, want other", class)
	}
}

// flakyListener yields temporary errors before delegating, the shape Accept
// takes under fd exhaustion.
type flakyListener struct {
	net.Listener
	tempErrs int32
}

type tempError struct{}

func (tempError) Error() string   { return "accept: too many open files" }
func (tempError) Timeout() bool   { return false }
func (tempError) Temporary() bool { return true }

func (f *flakyListener) Accept() (net.Conn, error) {
	if atomic.AddInt32(&f.tempErrs, -1) >= 0 {
		return nil, tempError{}
	}
	return f.Listener.Accept()
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.test"},
		DNSNames:     []string{"example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A temporary Accept error -- fd exhaustion under an SDK retry storm is the
// realistic case -- must not kill the accept loop: Serve used to retry its
// own Accept against the live TCP listener and recover, and this listener
// has to preserve that.
func TestATemporaryAcceptErrorDoesNotKillTheListener(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cert := selfSignedCert(t)
	l := &tlsAcceptListener{
		inner:  &flakyListener{Listener: inner, tempErrs: 3},
		active: new(atomic.Int64),
		cfg: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
		fail:  func(string, bool, error) {},
		conns: make(chan net.Conn),
		errs:  make(chan error, 1),
		done:  make(chan struct{}),
	}
	go l.acceptLoop()
	defer l.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := l.Accept(); err == nil {
			accepted <- c
		}
	}()

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	conn, err := tls.Dial("tcp", inner.Addr().String(), &tls.Config{
		ServerName: "example.test", RootCAs: pool, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial through the recovering listener: %v", err)
	}
	defer conn.Close()

	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the accept loop never recovered from a temporary error")
	}
}

// Plain HTTP to the HTTPS port used to get net/http's explanatory 400; the
// handshake now fails in the accept listener, which must preserve it.
func TestPlaintextHTTPToTheHTTPSPortGetsTheCourtesy400(t *testing.T) {
	h := newTransparentHarness(t)
	conn, err := net.Dial("tcp", h.httpsAddr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.0\r\nHost: api.stripe.com\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); !strings.Contains(got, "400 Bad Request") ||
		!strings.Contains(got, "HTTP request to an HTTPS server") {
		t.Fatalf("plaintext probe answered %q, want the courtesy 400", got)
	}
}

// The run's verdict reads the receipt exactly once, immediately after the
// workload exits -- and a dial's certificate error returns a beat before the
// server-side goroutine records it. The snapshot drains in-flight handshakes,
// so the rejection must be visible with no polling at all.
func TestTheSnapshotWaitsForInFlightHandshakes(t *testing.T) {
	h := newTransparentHarness(t)
	empty := x509.NewCertPool()
	for i := int64(1); i <= 10; i++ {
		if _, err := tls.Dial("tcp", h.httpsAddr, &tls.Config{
			ServerName: "api.stripe.com",
			RootCAs:    empty,
			MinVersion: tls.VersionTLS12,
		}); err == nil {
			t.Fatal("a client with no roots accepted the minted certificate")
		}
		var got int64
		for _, f := range h.running.Receipt().TrustFailures {
			if f.Host == "api.stripe.com" {
				got = f.Rejected
			}
		}
		if got != i {
			t.Fatalf("after dial %d the snapshot shows %d rejections; the drain missed one", i, got)
		}
	}
}
