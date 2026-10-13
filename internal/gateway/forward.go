// Package gateway relays HTTP CONNECT tunnels from a loopback listener to the
// Veris egress gateway, adding the gateway's credential on the way.
//
// The gateway is a CONNECT proxy that refuses a tunnel without Basic
// Proxy-Authorization naming the sandbox. A Playground desktop routes its GUI
// apps there through the operating system's proxy settings, and those
// settings -- a host and port, or a PAC file -- have nowhere to put a
// credential. So the apps point at this listener instead, which takes no
// credential, and it opens each tunnel upstream with one.
//
// It is a pure relay: it never terminates TLS and never reads a byte inside a
// tunnel. The upstream URL carries a capability, so it is never logged; only
// the gateway's host:port is.
package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PACPath is the one plain GET served, when a PAC file is configured. macOS
// reads file:// PAC URLs unreliably, so the system setting points here.
const PACPath = "/proxy.pac"

// Defaults for Forwarder's timeouts and the request-head bound.
const (
	DefaultHeadTimeout     = 15 * time.Second
	DefaultDialTimeout     = 10 * time.Second
	DefaultUpstreamTimeout = 15 * time.Second
	// maxHeadBytes bounds a request or response head. A CONNECT head is a
	// few hundred bytes; anything near this is not a proxy client.
	maxHeadBytes = 64 << 10
)

// Upstream is the gateway to tunnel through: where it is and the
// Proxy-Authorization value it requires.
type Upstream struct {
	Addr string // host:port, the only part of the URL that may be logged
	auth string // "Basic ..." -- never logged
}

// ParseUpstream reads an upstream proxy URL of the form
// http://user:pass@host:port. Its errors never quote the URL, which carries
// the credential.
func ParseUpstream(raw string) (Upstream, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		// url.Parse's error quotes its input.
		return Upstream{}, errors.New("the upstream proxy URL does not parse")
	}
	if u.Scheme != "http" {
		return Upstream{}, errors.New("the upstream proxy URL must be http://user:pass@host:port")
	}
	if u.Hostname() == "" {
		return Upstream{}, errors.New("the upstream proxy URL has no host")
	}
	if u.User == nil || u.User.Username() == "" {
		return Upstream{}, errors.New("the upstream proxy URL carries no credential (http://user:pass@host:port)")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	pass, _ := u.User.Password()
	cred := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pass))
	return Upstream{Addr: net.JoinHostPort(u.Hostname(), port), auth: "Basic " + cred}, nil
}

// ReadUpstreamFile reads the upstream proxy URL from path. The file is meant
// to be readable by its owner alone; a looser mode is reported through warn
// rather than refused, since on some systems the mode bits say nothing.
func ReadUpstreamFile(path string, warn func(msg string, args ...any)) (Upstream, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Upstream{}, fmt.Errorf("read the upstream file: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 && warn != nil {
		warn("the upstream file is readable by others; it holds a credential and should be mode 0600",
			"path", path, "mode", fmt.Sprintf("%#o", info.Mode().Perm()))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Upstream{}, fmt.Errorf("read the upstream file: %w", err)
	}
	return ParseUpstream(string(b))
}

// CheckLoopback refuses a listen address that is not on loopback. The
// listener asks for no credential and spends one, so anything that can reach
// it can use the sandbox's gateway.
func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("--listen %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("--listen %q is not a loopback address: the listener takes no "+
		"credential, so it must be reachable from this machine alone (use 127.0.0.1)", addr)
}

// Forwarder serves CONNECT on a listener and opens each tunnel through the
// upstream gateway. The zero timeouts mean the package defaults.
type Forwarder struct {
	Upstream Upstream
	// PACFile, when set, is served at GET /proxy.pac, read afresh per request.
	PACFile string
	Log     *slog.Logger

	HeadTimeout     time.Duration
	DialTimeout     time.Duration
	UpstreamTimeout time.Duration

	mu       sync.Mutex
	ln       net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	shutdown atomic.Bool
}

// Serve accepts on ln until Shutdown, handling each connection in its own
// goroutine. It returns nil after Shutdown and the accept error otherwise.
func (f *Forwarder) Serve(ln net.Listener) error {
	f.mu.Lock()
	f.ln = ln
	if f.conns == nil {
		f.conns = map[net.Conn]struct{}{}
	}
	f.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			if f.shutdown.Load() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if !f.track(c) {
			_ = c.Close()
			continue
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer f.untrack(c)
			f.handle(c)
		}()
	}
}

// Shutdown stops accepting and waits for open tunnels until ctx is done, then
// closes whatever is still open. Tunnels have no idle timeout, so a
// long-poll would otherwise hold shutdown forever.
func (f *Forwarder) Shutdown(ctx context.Context) error {
	f.shutdown.Store(true)
	f.mu.Lock()
	if f.ln != nil {
		_ = f.ln.Close()
	}
	f.mu.Unlock()
	done := make(chan struct{})
	go func() { f.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}
	f.mu.Lock()
	for c := range f.conns {
		_ = c.Close()
	}
	f.mu.Unlock()
	<-done
	return ctx.Err()
}

func (f *Forwarder) track(c net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shutdown.Load() {
		return false
	}
	f.conns[c] = struct{}{}
	return true
}

func (f *Forwarder) untrack(c net.Conn) {
	f.mu.Lock()
	delete(f.conns, c)
	f.mu.Unlock()
}

func (f *Forwarder) logger() *slog.Logger {
	if f.Log == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return f.Log
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// errHeadTooLarge is a request head past maxHeadBytes.
var errHeadTooLarge = errors.New("request head too large")

// headLimit caps the bytes read from the client while its head is parsed,
// and is lifted once it is, so a tunnel is not bounded by it.
type headLimit struct {
	r       io.Reader
	n       int
	limited bool
}

func (h *headLimit) Read(p []byte) (int, error) {
	if !h.limited {
		return h.r.Read(p)
	}
	if h.n <= 0 {
		return 0, errHeadTooLarge
	}
	if len(p) > h.n {
		p = p[:h.n]
	}
	n, err := h.r.Read(p)
	h.n -= n
	return n, err
}

// handle serves one client connection: a CONNECT tunnel, the PAC file, or a
// refusal. It owns c and closes it.
func (f *Forwarder) handle(c net.Conn) {
	defer c.Close()
	log := f.logger()
	_ = c.SetReadDeadline(time.Now().Add(orDefault(f.HeadTimeout, DefaultHeadTimeout)))
	lim := &headLimit{r: c, n: maxHeadBytes, limited: true}
	br := bufio.NewReader(lim)
	req, err := http.ReadRequest(br)
	if err != nil {
		var ne net.Error
		switch {
		case errors.Is(err, io.EOF):
			// Connected and said nothing: a port probe.
		case errors.As(err, &ne) && ne.Timeout():
			writeStatus(c, http.StatusRequestTimeout)
		case errors.Is(err, errHeadTooLarge):
			writeStatus(c, http.StatusRequestHeaderFieldsTooLarge)
		default:
			writeStatus(c, http.StatusBadRequest)
		}
		log.Debug("gateway request refused", "client", c.RemoteAddr().String(), "err", err)
		return
	}
	lim.limited = false
	if req.Method != http.MethodConnect {
		f.serveNonConnect(c, req)
		return
	}
	target := req.Host
	if !validTarget(target) {
		writeStatus(c, http.StatusBadRequest)
		log.Info("tunnel refused", "target", target, "status", http.StatusBadRequest, "reason", "target is not host:port")
		return
	}
	// Bytes the client sent after its head, before any answer: the start of
	// a TLS ClientHello, typically. They belong to the tunnel.
	pipelined, _ := br.Peek(br.Buffered())
	f.tunnel(c, target, pipelined)
}

// serveNonConnect answers GET /proxy.pac when a PAC file is configured and
// 501 to everything else that is not CONNECT.
func (f *Forwarder) serveNonConnect(c net.Conn, req *http.Request) {
	log := f.logger()
	if f.PACFile == "" || req.Method != http.MethodGet || req.URL.IsAbs() || req.URL.Path != PACPath {
		writeStatus(c, http.StatusNotImplemented)
		log.Debug("gateway request refused", "method", req.Method, "status", http.StatusNotImplemented)
		return
	}
	body, err := os.ReadFile(f.PACFile)
	if err != nil {
		writeStatus(c, http.StatusNotFound)
		log.Warn("the PAC file could not be read", "path", f.PACFile, "err", err)
		return
	}
	_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\n"+
		"Content-Type: application/x-ns-proxy-autoconfig\r\n"+
		"Cache-Control: no-store\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n\r\n", len(body))
	_, _ = c.Write(body)
	log.Debug("served the PAC file", "bytes", len(body))
}

// validTarget reports whether a CONNECT target is host:port with a numeric
// port, which is all a tunnel can be opened to.
func validTarget(t string) bool {
	host, port, err := net.SplitHostPort(t)
	if err != nil || host == "" {
		return false
	}
	p, err := strconv.Atoi(port)
	return err == nil && p > 0 && p < 65536
}

// writeStatus writes a bodiless response that ends the connection.
func writeStatus(c net.Conn, code int) {
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		code, http.StatusText(code))
}

// tunnel opens target through the upstream gateway and splices the client
// to it. pipelined is what the client already sent past its CONNECT head.
func (f *Forwarder) tunnel(c net.Conn, target string, pipelined []byte) {
	log := f.logger()
	start := time.Now()
	up, err := (&net.Dialer{Timeout: orDefault(f.DialTimeout, DefaultDialTimeout)}).Dial("tcp", f.Upstream.Addr)
	if err != nil {
		writeStatus(c, http.StatusBadGateway)
		log.Warn("tunnel refused", "target", target, "upstream", f.Upstream.Addr,
			"status", http.StatusBadGateway, "reason", "upstream unreachable", "err", err)
		return
	}
	f.mu.Lock()
	f.conns[up] = struct{}{}
	f.mu.Unlock()
	defer f.untrack(up)
	defer up.Close()

	_ = up.SetDeadline(time.Now().Add(orDefault(f.UpstreamTimeout, DefaultUpstreamTimeout)))
	if _, err := fmt.Fprintf(up, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		target, target, f.Upstream.auth); err != nil {
		writeStatus(c, http.StatusBadGateway)
		log.Warn("tunnel refused", "target", target, "upstream", f.Upstream.Addr,
			"status", http.StatusBadGateway, "reason", "upstream write failed", "err", err)
		return
	}
	ubr := bufio.NewReader(&headLimit{r: up, n: maxHeadBytes, limited: true})
	statusLine, code, err := readResponseHead(ubr)
	if err != nil {
		writeStatus(c, http.StatusBadGateway)
		log.Warn("tunnel refused", "target", target, "upstream", f.Upstream.Addr,
			"status", http.StatusBadGateway, "reason", "upstream answered no valid CONNECT response", "err", err)
		return
	}
	if code < 200 || code > 299 {
		// The status alone: the upstream's headers (a Proxy-Authenticate
		// above all) would have the client prompt for a credential that is
		// not its to give.
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = fmt.Fprintf(c, "%s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", statusLine)
		log.Info("tunnel refused", "target", target, "upstream", f.Upstream.Addr,
			"status", code, "duration", time.Since(start).Round(time.Millisecond))
		return
	}

	// Established: no deadline from here on, as a long-poll or a websocket
	// may sit idle for as long as it likes.
	_ = up.SetDeadline(time.Time{})
	_ = c.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(c, "%s\r\n\r\n", statusLine); err != nil {
		return
	}
	var sentUp, sentDown int64
	if len(pipelined) > 0 {
		n, err := up.Write(pipelined)
		sentUp += int64(n)
		if err != nil {
			return
		}
	}
	// Whatever the upstream sent past its response head is the far end's.
	if early, _ := ubr.Peek(ubr.Buffered()); len(early) > 0 {
		n, err := c.Write(early)
		sentDown += int64(n)
		if err != nil {
			return
		}
	}
	log.Info("tunnel open", "target", target, "upstream", f.Upstream.Addr, "status", code)
	toUp, toDown := splice(c, up)
	log.Info("tunnel closed", "target", target, "upstream", f.Upstream.Addr, "status", code,
		"bytes_up", sentUp+toUp, "bytes_down", sentDown+toDown,
		"duration", time.Since(start).Round(time.Millisecond))
}

// readResponseHead reads the upstream's response to CONNECT up to its blank
// line, returning the status line and code. Headers are read and dropped.
func readResponseHead(br *bufio.Reader) (string, int, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", 0, err
	}
	statusLine := strings.TrimRight(line, "\r\n")
	proto, rest, ok := strings.Cut(statusLine, " ")
	if !ok || !strings.HasPrefix(proto, "HTTP/1.") {
		return "", 0, errors.New("malformed status line")
	}
	codeText, _, _ := strings.Cut(rest, " ")
	code, err := strconv.Atoi(codeText)
	if err != nil || len(codeText) != 3 {
		return "", 0, errors.New("malformed status code")
	}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return "", 0, err
		}
		if strings.TrimRight(h, "\r\n") == "" {
			return statusLine, code, nil
		}
	}
}

// splice copies both ways until both directions end, half-closing each
// side's write when the other side's read reaches EOF, and returns the bytes
// sent client-to-upstream and upstream-to-client.
func splice(client, up net.Conn) (int64, int64) {
	var toUp, toDown int64
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn, n *int64) {
		defer wg.Done()
		copied, err := io.Copy(dst, src)
		*n = copied
		if err != nil {
			// A reset or a closed socket: nothing more will flow either way.
			_ = dst.Close()
			_ = src.Close()
			return
		}
		closeWrite(dst)
	}
	go pipe(up, client, &toUp)
	go pipe(client, up, &toDown)
	wg.Wait()
	return toUp, toDown
}

// closeWrite half-closes c when it supports it, and closes it when not, so
// the far end sees the EOF either way.
func closeWrite(c net.Conn) {
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
		return
	}
	_ = c.Close()
}
