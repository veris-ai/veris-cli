package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// ListenOptions selects which listeners to bind. An empty address disables
// that listener.
type ListenOptions struct {
	// Proxy is the explicit-proxy address (HTTP_PROXY points here). Empty
	// falls back to the config's listen address.
	Proxy string

	// TransparentHTTP and TransparentHTTPS receive kernel-redirected
	// connections, which carry no absolute URL.
	TransparentHTTP  string
	TransparentHTTPS string
}

// Running is a started proxy: every requested listener is bound and accepting.
type Running struct {
	srv *Server

	servers []*http.Server
	addrs   map[string]string

	errCh   chan error
	closing chan struct{}
	once    sync.Once
	shutErr error
	wg      sync.WaitGroup
}

// Start binds every requested listener before returning, so a caller can start
// a child process knowing interception is already live. If any bind fails, the
// listeners already bound are closed and the error names the address.
func (s *Server) Start(opts ListenOptions) (*Running, error) {
	proxyAddr := opts.Proxy
	if proxyAddr == "" {
		proxyAddr = s.cfg.Listen
	}

	type spec struct {
		kind    string
		addr    string
		handler http.Handler
		tlsCfg  *tls.Config
	}
	specs := []spec{{kind: "proxy", addr: proxyAddr, handler: s.handler}}
	add := func(kind, addr string, isTLS bool) {
		if addr == "" {
			return
		}
		sp := spec{kind: kind, addr: addr, handler: s.interceptHandler(isTLS)}
		if isTLS {
			sp.tlsCfg = s.serverTLSConfig()
		}
		specs = append(specs, sp)
	}
	add("transparent-http", opts.TransparentHTTP, false)
	add("transparent-https", opts.TransparentHTTPS, true)

	r := &Running{
		srv:     s,
		addrs:   map[string]string{},
		errCh:   make(chan error, len(specs)),
		closing: make(chan struct{}),
	}

	// Every bind happens before anything starts accepting. Serving from a
	// listener that bound while a later one is still being tried would leave
	// live connections behind a Start that reported failure, and the caller
	// has no handle to shut them down with.
	listeners := make([]net.Listener, 0, len(specs))
	for _, sp := range specs {
		ln, err := net.Listen("tcp", sp.addr)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			return nil, fmt.Errorf("listen for %s on %s: %w", sp.kind, sp.addr, err)
		}
		listeners = append(listeners, ln)
		r.addrs[sp.kind] = ln.Addr().String()
	}

	for i, sp := range specs {
		ln := listeners[i]
		hs := &http.Server{
			Handler: sp.handler,
			// Interception targets are ordinary REST APIs; a request slower
			// than this is a hung upstream, not a legitimate slow call.
			ReadHeaderTimeout: 30 * time.Second,
			// Kept even though the accept layer below handshakes for itself:
			// Serve only installs net/http's HTTP/2 handler when the server's
			// own TLSConfig offers "h2". Without it, every ALPN-negotiated h2
			// connection silently falls back to HTTP/1.1.
			TLSConfig: sp.tlsCfg,
		}
		r.servers = append(r.servers, hs)

		if sp.tlsCfg != nil {
			// The handshake runs in this listener, not inside net/http, which
			// discards handshake errors -- a client refusing the minted
			// certificate would be indistinguishable from one that never
			// called. net/http still negotiates h2: an already-handshaken
			// *tls.Conn arriving through plain Serve is dispatched through
			// TLSNextProto by its negotiated protocol.
			ln = s.newTLSAcceptListener(ln, sp.tlsCfg)
		}

		r.wg.Add(1)
		go func(hs *http.Server, ln net.Listener) {
			defer r.wg.Done()
			err := hs.Serve(ln)
			// Shutdown and Close both surface as ErrServerClosed; that is a
			// clean stop, not a failure worth waking the caller for.
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case r.errCh <- err:
				default:
				}
			}
		}(hs, ln)
	}
	return r, nil
}

// Addr returns the bound address for a listener kind, which is how a caller
// that asked for port 0 learns the real port.
func (r *Running) Addr(kind string) string { return r.addrs[kind] }

// ProxyURL is the address to point HTTP_PROXY at.
func (r *Running) ProxyURL() string { return "http://" + r.addrs["proxy"] }

// Wait blocks until a listener fails or Shutdown is called. A clean shutdown
// returns nil.
func (r *Running) Wait() error {
	select {
	case err := <-r.errCh:
		return err
	case <-r.closing:
		// A listener failure and a shutdown can become ready in the same
		// instant. Prefer the failure: it is the one that explains anything.
		select {
		case err := <-r.errCh:
			return err
		default:
			return nil
		}
	}
}

// Shutdown stops accepting, gives in-flight requests until ctx expires to
// finish, then force-closes. It is safe to call more than once, and every
// caller gets the same answer.
func (r *Running) Shutdown(ctx context.Context) error {
	r.once.Do(func() {
		// `closing` is closed LAST, after every server has drained. Closing it
		// first released Wait immediately, so the process returned from main
		// while requests were still in flight -- the five-second grace was
		// advertised and never actually taken.
		defer close(r.closing)
		for _, hs := range r.servers {
			// Every server that did not drain in time is force-closed, not
			// just the first. A shared context expires for all of them at
			// once, so stopping at the first error would leave the rest of the
			// connections live after Shutdown returned.
			if e := hs.Shutdown(ctx); e != nil {
				_ = hs.Close()
				if r.shutErr == nil {
					r.shutErr = e
				}
			}
		}
		r.wg.Wait()
	})
	// A second caller waits for the first to finish rather than reporting a
	// success the drain has not reached yet. Closed already when this goroutine
	// is the one that ran the shutdown.
	<-r.closing
	return r.shutErr
}

// Receipt snapshots what this run sent to the sandbox. Reading it in-process
// rather than over the status endpoint matters: there is no "could not reach
// the proxy" state to confuse with "the run sent nothing".
func (r *Running) Receipt() Receipt { return r.srv.receiptSnapshot() }
