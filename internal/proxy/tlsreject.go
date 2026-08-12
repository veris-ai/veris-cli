package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// This file makes a refused handshake visible. An SDK that ships its own CA
// bundle -- stripe-python and friends -- rejects the minted leaf during the
// TLS handshake, before any HTTP exists, so the receipt of forwarded requests
// cannot see it: a client that refused our certificate looks identical to a
// client that never called. The transparent listeners therefore run the
// handshake themselves, classify the failure, and aggregate it per SNI host.
//
// The explicit-proxy tier is not covered: goproxy drives the MITM handshake
// inside its own CONNECT loop and discards the error, so observing it there
// would mean reimplementing that loop.

// tlsHandshakeTimeout bounds a handshake so a client that connects and sends
// nothing cannot pin a goroutine open. A timeout is classified as "other",
// never as a trust rejection.
const tlsHandshakeTimeout = 15 * time.Second

// handshakeClass buckets a failed handshake by what it says about CA trust.
type handshakeClass int

const (
	// handshakeOther: timeouts, protocol failures, anything that does not
	// implicate the certificate. Never reported as a trust rejection.
	handshakeOther handshakeClass = iota
	// handshakeAborted: the peer went away (EOF, reset) after the leaf was
	// selected. Consistent with a trust refusal but not proof of one.
	handshakeAborted
	// handshakeTrustRejected: the peer answered the minted certificate with a
	// certificate alert. The client saw the leaf and said no.
	handshakeTrustRejected
)

// classifyHandshake reads a server-side handshake error. A peer's TLS alert
// surfaces as a *net.OpError with Op "remote error" wrapping the alert text.
// TLS 1.3 encrypts alerts sent after the ServerHello, but this side of
// crypto/tls holds the keys, so the alert still arrives as the handshake
// error rather than as opaque bytes.
func classifyHandshake(err error) (handshakeClass, string) {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" && op.Err != nil {
		msg := op.Err.Error()
		switch {
		// Before the other certificate alerts: "unknown certificate
		// authority" contains "unknown certificate".
		case strings.Contains(msg, "unknown certificate authority"):
			return handshakeTrustRejected, "unknown_ca"
		case strings.Contains(msg, "bad certificate"):
			return handshakeTrustRejected, "bad_certificate"
		case strings.Contains(msg, "unknown certificate"),
			strings.Contains(msg, "certificate unknown"):
			return handshakeTrustRejected, "certificate_unknown"
		// The client evaluated the minted leaf and refused it for its
		// content rather than its issuer -- expired means the workload's
		// clock disagrees with the CA's validity window. Still a trust
		// rejection; the reason name carries the actual cause.
		case strings.Contains(msg, "expired certificate"):
			return handshakeTrustRejected, "certificate_expired"
		case strings.Contains(msg, "revoked certificate"):
			return handshakeTrustRejected, "certificate_revoked"
		case strings.Contains(msg, "unsupported certificate"):
			return handshakeTrustRejected, "unsupported_certificate"
		}
		// A remote alert that names no certificate -- handshake_failure,
		// protocol_version -- is a different disagreement.
		return handshakeOther, ""
	}
	// OpenSSL rejecting the certificate under TLS 1.3 does not arrive as a
	// readable alert: the record fails MAC verification here and surfaces as
	// this local error instead. The same client pinned to TLS 1.2 sends a
	// clear unknown_ca, which is how the shape was attributed. Callers only
	// classify failures after leaf selection, where a genuinely corrupted
	// record is vanishingly rare next to a client refusing the minted leaf --
	// and stripe-python's stack is exactly this client, so counting it
	// "other" would silence the diagnostic for the SDKs it exists for.
	if errors.As(err, &op) && op.Op == "local error" && op.Err != nil &&
		strings.Contains(op.Err.Error(), "bad record MAC") {
		return handshakeTrustRejected, "bad_record_mac"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return handshakeAborted, ""
	}
	return handshakeOther, ""
}

// TrustFailure aggregates the failed TLS handshakes for one SNI host.
type TrustFailure struct {
	Host string `json:"host"`
	// Mapped: the host routes to a simulated service, so a rejection here is
	// missing sandbox traffic rather than blocked background noise.
	Mapped bool `json:"mapped"`
	// Rejected counts handshakes the client ended with a certificate alert
	// after the leaf was selected. Reasons holds those alerts by name
	// (unknown_ca, bad_certificate, certificate_unknown).
	Rejected int64            `json:"rejected"`
	Reasons  map[string]int64 `json:"reasons,omitempty"`
	// Aborted counts handshakes that ended in EOF or a reset after the leaf
	// was selected. Consistent with a refusal -- some stacks close instead of
	// alerting -- but the wire cannot prove it.
	Aborted int64 `json:"aborted"`
	// Other is everything else after leaf selection: timeouts, protocol
	// errors. Never reported as a trust rejection.
	Other int64 `json:"other,omitempty"`
}

// trustLog is the thread-safe per-host aggregation. SDK retry loops produce
// many identical failures, so these are counters, not events.
type trustLog struct {
	mu    sync.Mutex
	hosts map[string]*trustCounts
}

type trustCounts struct {
	mapped   bool
	rejected int64
	reasons  map[string]int64
	aborted  int64
	other    int64
}

func (l *trustLog) record(host string, mapped bool, class handshakeClass, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hosts == nil {
		l.hosts = map[string]*trustCounts{}
	}
	c := l.hosts[host]
	if c == nil {
		c = &trustCounts{mapped: mapped, reasons: map[string]int64{}}
		l.hosts[host] = c
	}
	switch class {
	case handshakeTrustRejected:
		c.rejected++
		c.reasons[reason]++
	case handshakeAborted:
		c.aborted++
	default:
		c.other++
	}
}

func (l *trustLog) snapshot() []TrustFailure {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hosts) == 0 {
		return nil
	}
	out := make([]TrustFailure, 0, len(l.hosts))
	for host, c := range l.hosts {
		f := TrustFailure{
			Host: host, Mapped: c.mapped,
			Rejected: c.rejected, Aborted: c.aborted, Other: c.other,
		}
		if len(c.reasons) > 0 {
			f.Reasons = make(map[string]int64, len(c.reasons))
			for name, n := range c.reasons {
				f.Reasons[name] = n
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// receiptSnapshot is the receipt plus the trust-failure ledger, merged at
// snapshot time so the receipt itself keeps counting only what the sandbox
// actually received.
//
// It drains in-flight handshakes first. A connection is counted before its
// handshake starts and the client's dial error can return a beat before the
// server-side goroutine records the rejection — and the run's verdict reads
// the receipt exactly once, so an in-flight recorder missed here would turn
// a refused mapped host into a false pass. The bound covers a handshake that
// is genuinely stuck; it never waits on an idle proxy.
func (s *Server) receiptSnapshot() Receipt {
	deadline := time.Now().Add(2 * time.Second)
	for s.handshakesInFlight.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	out := s.receipt.snapshot()
	out.TrustFailures = s.trustFailures.snapshot()
	return out
}

// recordHandshakeFailure classifies one failed handshake from a transparent
// TLS listener. Only failures after certificate selection are attributed: a
// connection that died earlier never saw the minted leaf, so it says nothing
// about whether the client trusts the CA.
func (s *Server) recordHandshakeFailure(sni string, leafSelected bool, err error) {
	if sni == "" || !leafSelected {
		return
	}
	class, reason := classifyHandshake(err)
	s.trustFailures.record(sni, s.cfg.HostIsMapped(sni), class, reason)
	switch class {
	case handshakeTrustRejected:
		s.log.Warn("client rejected the minted certificate",
			"host", sni, "reason", reason,
			"hint", "the client trusts its own CA bundle; see the Certificates section of the README")
	case handshakeAborted:
		s.log.Debug("TLS handshake abandoned after certificate selection",
			"host", sni, "err", err)
	default:
		s.log.Debug("TLS handshake failed", "host", sni, "err", err)
	}
}

// tlsAcceptListener terminates TLS itself and yields only successfully
// handshaken connections. net/http runs the handshake inside conn.serve and
// discards its error, so a client refusing the minted certificate would be
// invisible there. HTTP/2 survives the move: Serve reads NextProtos off the
// server's TLSConfig to install the h2 handler, and dispatches an
// already-handshaken *tls.Conn through TLSNextProto by negotiated protocol.
type tlsAcceptListener struct {
	inner net.Listener
	cfg   *tls.Config
	fail  func(sni string, leafSelected bool, err error)
	// active counts handshakes accepted but not yet resolved-and-recorded,
	// so the receipt snapshot can drain them before taking a verdict.
	active *atomic.Int64

	// states carries per-connection handshake progress from GetCertificate,
	// which knows the SNI, to the handshake driver, which sees the error.
	// Keyed by the raw net.Conn, which is what ClientHelloInfo.Conn holds.
	states sync.Map
	conns  chan net.Conn
	errs   chan error
	done   chan struct{}
	once   sync.Once
}

// handshakeState is written during HandshakeContext and read after it
// returns, all on one goroutine, so its fields need no lock.
type handshakeState struct {
	sni          string
	leafSelected bool
}

func (s *Server) newTLSAcceptListener(inner net.Listener, base *tls.Config) net.Listener {
	l := &tlsAcceptListener{
		inner:  inner,
		fail:   s.recordHandshakeFailure,
		active: &s.handshakesInFlight,
		conns:  make(chan net.Conn),
		errs:   make(chan error, 1),
		done:   make(chan struct{}),
	}
	// One cloned config for the listener's whole life, so session tickets
	// stay shared across connections exactly as with the base config.
	cfg := base.Clone()
	mint := cfg.GetCertificate
	cfg.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		cert, err := mint(hello)
		if st, ok := l.states.Load(hello.Conn); ok {
			state := st.(*handshakeState)
			state.sni = strings.ToLower(hello.ServerName)
			state.leafSelected = err == nil && cert != nil
		}
		return cert, err
	}
	l.cfg = cfg
	go l.acceptLoop()
	return l
}

// acceptLoop takes TCP connections off the inner listener and handshakes each
// on its own goroutine, so one slow or silent client cannot block the rest.
func (l *tlsAcceptListener) acceptLoop() {
	var delay time.Duration
	for {
		raw, err := l.inner.Accept()
		if err != nil {
			// Temporary failures -- fd exhaustion under an SDK retry storm is
			// the realistic one -- must not kill the listener: Serve would
			// retry its own Accept, which now blocks forever with no producer.
			// Mirror net/http's backoff and keep accepting; only a permanent
			// error (the listener closed) is forwarded.
			var ne net.Error
			if errors.As(err, &ne) && ne.Temporary() {
				if delay == 0 {
					delay = 5 * time.Millisecond
				} else if delay *= 2; delay > time.Second {
					delay = time.Second
				}
				select {
				case <-time.After(delay):
					continue
				case <-l.done:
					return
				}
			}
			select {
			case l.errs <- err:
			default:
			}
			return
		}
		delay = 0
		// Counted on this goroutine, before any handshake byte moves: by the
		// time a client's dial returns, its connection is already in the
		// counter, which is what makes draining the snapshot sound.
		l.active.Add(1)
		go l.handshake(raw)
	}
}

func (l *tlsAcceptListener) handshake(raw net.Conn) {
	state := &handshakeState{}
	l.states.Store(raw, state)
	defer l.states.Delete(raw)

	conn := tls.Server(raw, l.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), tlsHandshakeTimeout)
	err := conn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		writePlaintextHTTPHint(raw, err)
		l.fail(state.sni, state.leafSelected, err)
	}
	// Resolved AND recorded -- decrementing before l.fail would reopen the
	// exact window the counter closes.
	l.active.Add(-1)
	if err != nil {
		_ = conn.Close()
		return
	}
	select {
	case l.conns <- conn:
	case <-l.done:
		_ = conn.Close()
	}
}

// writePlaintextHTTPHint preserves net/http's courtesy answer for a plain
// HTTP request sent to the HTTPS port. The handshake now fails in this
// listener rather than in conn.serve, so without this a misconfigured client
// would see a bare reset where ServeTLS used to explain the mistake.
func writePlaintextHTTPHint(raw net.Conn, err error) {
	var rhe tls.RecordHeaderError
	if !errors.As(err, &rhe) {
		return
	}
	switch string(rhe.RecordHeader[:]) {
	case "GET /", "HEAD ", "POST ", "PUT /", "OPTIO":
		_ = raw.SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = io.WriteString(raw,
			"HTTP/1.0 400 Bad Request\r\n\r\nClient sent an HTTP request to an HTTPS server.\n")
	}
}

func (l *tlsAcceptListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *tlsAcceptListener) Close() error {
	err := l.inner.Close()
	l.once.Do(func() { close(l.done) })
	return err
}

func (l *tlsAcceptListener) Addr() net.Addr { return l.inner.Addr() }
