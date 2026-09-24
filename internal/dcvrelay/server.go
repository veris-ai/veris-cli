package dcvrelay

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Subprotocol is the WebSocket subprotocol every DCV socket offers, and the
// bench relay echoes.
const Subprotocol = "dcv"

// dialTimeout bounds opening one socket to the bench relay, TLS and the
// upgrade together.
const dialTimeout = 30 * time.Second

// readHeaderTimeout bounds how long a loopback client takes to send a
// request's headers.
const readHeaderTimeout = 30 * time.Second

// What of a resource request reaches the bench relay, and what of its answer
// comes back: the headers DCV's resource exchange uses, and no others.
var (
	requestHeaders  = []string{"Accept", "Content-Type", "Dcv-Extension-Data", "Range"}
	responseHeaders = []string{"Accept-Ranges", "Cache-Control", "Content-Disposition", "Content-Length", "Content-Range", "Content-Type"}
)

// Server relays a DCV client on loopback to the bench API's DCV relay: each
// WebSocket the client opens (auth, the main channel, one per data channel)
// becomes a WebSocket under the current ticket's base URL, frame for frame
// with each frame's type kept, and each POST or DELETE (DCV's resource
// requests) is forwarded with its body. The bench relay serves nothing else,
// so any other request is refused here and reported through OnRefuse.
type Server struct {
	// First, when its BaseURL is set, is a ticket already in hand, used until
	// it comes due for replacement.
	First Ticket
	// Ticket fetches a fresh ticket. Wrap the error in a *StopError when the
	// session has ended and no later ticket could be issued either.
	Ticket func(ctx context.Context) (Ticket, error)
	// Transport reaches the bench relay; http.DefaultTransport when nil.
	Transport http.RoundTripper
	// UserAgent names the binary and version to the bench relay.
	UserAgent string
	// Refresh is how long before its expiry a ticket is replaced; a minute
	// when zero.
	Refresh time.Duration
	// OnOpen, when set, is told a socket was relayed; id numbers sockets
	// from 1 and path is the client's request path, such as "/auth".
	OnOpen func(id int, path string)
	// OnClose, when set, is told a socket ended, and why: nil when either side
	// closed it normally or the relay is stopping, otherwise what ended it. A
	// socket that could not be opened is reported here without an OnOpen.
	OnClose func(id int, path string, err error)
	// OnRefuse, when set, is told of a request the relay cannot carry.
	OnRefuse func(method, path string)
}

// Serve serves TLS with cert on ln until ctx is done or the session ends,
// and closes ln either way. It returns nil when ctx ended it, the
// *StopError when the session did, and a listener failure otherwise; every
// connection is closed before it returns.
func (s *Server) Serve(ctx context.Context, ln net.Listener, cert tls.Certificate) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tk := &tickets{fetch: s.Ticket, refresh: cmp.Or(s.Refresh, defaultRefresh), now: time.Now}
	if s.First.BaseURL != "" {
		tk.seed(s.First)
	}
	r := &relay{s: s, tickets: tk, stop: cancel}
	hs := &http.Server{
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
		// A client that refuses the certificate, or probes and hangs up, is
		// not worth a line on the terminal.
		ErrorLog:    log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		// WebSockets upgrade over HTTP/1.1 only.
		NextProtos: []string{"http/1.1"},
	})

	var bg sync.WaitGroup
	bg.Add(1)
	go func() {
		defer bg.Done()
		if err := tk.keepFresh(ctx); err != nil {
			cancel(err)
		}
	}()
	served := make(chan error, 1)
	go func() { served <- hs.Serve(tlsLn) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-served:
		cancel(nil)
	}
	_ = hs.Close()
	r.drain()
	bg.Wait()

	var stop *StopError
	if errors.As(context.Cause(ctx), &stop) {
		return stop
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// relay is one Serve's handler.
type relay struct {
	s       *Server
	tickets *tickets
	stop    context.CancelCauseFunc
	ids     atomic.Int64

	mu      sync.Mutex
	closing bool
	active  sync.WaitGroup
}

// enter admits a request unless the relay is stopping; the caller calls
// active.Done when it admitted one.
func (r *relay) enter() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.active.Add(1)
	return true
}

// drain waits for every admitted request, sockets included, to end. Their
// contexts derive from Serve's, which is done by now.
func (r *relay) drain() {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.active.Wait()
}

func (r *relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !r.enter() {
		http.Error(w, "the Playground screen is stopping", http.StatusServiceUnavailable)
		return
	}
	defer r.active.Done()
	switch {
	case isUpgrade(req):
		r.socket(w, req)
	case req.Method == http.MethodPost || req.Method == http.MethodDelete:
		r.forward(w, req)
	default:
		if r.s.OnRefuse != nil {
			r.s.OnRefuse(req.Method, req.URL.Path)
		}
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "the Playground desktop relay carries WebSockets, POST and DELETE only", http.StatusMethodNotAllowed)
	}
}

// base is the current ticket's base URL. A session that has ended stops
// Serve as well as failing this request.
func (r *relay) base(ctx context.Context) (string, error) {
	base, err := r.tickets.current(ctx)
	var stop *StopError
	if errors.As(err, &stop) {
		r.stop(stop)
	}
	return base, err
}

func (r *relay) transport() http.RoundTripper {
	if r.s.Transport != nil {
		return r.s.Transport
	}
	return http.DefaultTransport
}

// target is req's path, without its leading "/", and query under base.
func target(base string, req *http.Request) string {
	u := base + strings.TrimPrefix(req.URL.EscapedPath(), "/")
	if req.URL.RawQuery != "" {
		u += "?" + req.URL.RawQuery
	}
	return u
}

// isUpgrade reports a WebSocket opening handshake.
func isUpgrade(req *http.Request) bool {
	return req.Method == http.MethodGet && strings.EqualFold(req.Header.Get("Upgrade"), "websocket")
}

// rejected is a bench relay answer that means the ticket is no longer
// admitted, so the next attempt should use a fresh one.
func rejected(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}
