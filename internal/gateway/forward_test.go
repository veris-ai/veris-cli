package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The credential every test's upstream URL carries. Distinctive, so a log
// that leaks any part of it is caught by a substring search.
const (
	testUser = "v1.sbxzq7leakcheck"
	testPass = "pw9leakcheck"
)

var wantAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(testUser+":"+testPass))

// fakeUpstream is a CONNECT proxy that records each request's
// Proxy-Authorization and target, answers with status, sends early right
// after its head, and then echoes until the client half-closes.
type fakeUpstream struct {
	ln     net.Listener
	status string // e.g. "200 Connection established"
	early  string // bytes sent in the same write as the response head

	mu      sync.Mutex
	auths   []string
	targets []string
}

func startUpstream(t *testing.T, status, early string) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &fakeUpstream{ln: ln, status: status, early: early}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go u.serve(c)
		}
	}()
	return u
}

func (u *fakeUpstream) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	u.mu.Lock()
	u.auths = append(u.auths, req.Header.Get("Proxy-Authorization"))
	u.targets = append(u.targets, req.Host)
	u.mu.Unlock()
	head := "HTTP/1.1 " + u.status + "\r\n"
	if !strings.HasPrefix(u.status, "2") {
		head += "Proxy-Authenticate: Basic realm=\"gw\"\r\nContent-Length: 0\r\n"
	}
	_, _ = io.WriteString(c, head+"\r\n"+u.early)
	if !strings.HasPrefix(u.status, "2") {
		return
	}
	_, _ = io.Copy(c, br)
	_ = c.(*net.TCPConn).CloseWrite()
}

func (u *fakeUpstream) url() string {
	return "http://" + testUser + ":" + testPass + "@" + u.ln.Addr().String()
}

func (u *fakeUpstream) seen() (auths, targets []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auths...), append([]string(nil), u.targets...)
}

// syncBuffer is a log sink safe for the forwarder's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startForwarder serves a Forwarder for upstreamURL on a loopback port and
// returns its address and its log.
func startForwarder(t *testing.T, upstreamURL string, mutate func(*Forwarder)) (string, *syncBuffer) {
	t.Helper()
	up, err := ParseUpstream(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	f := &Forwarder{
		Upstream: up,
		Log:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mutate != nil {
		mutate(f)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- f.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.Shutdown(ctx)
		if err := <-served; err != nil {
			t.Errorf("Serve returned %v after Shutdown", err)
		}
		if strings.Contains(logs.String(), testPass) || strings.Contains(logs.String(), testUser) ||
			strings.Contains(logs.String(), strings.TrimPrefix(wantAuth, "Basic ")) {
			t.Errorf("the credential appears in the log:\n%s", logs.String())
		}
	})
	return ln.Addr().String(), logs
}

// exchange sends raw to addr, half-closes, and returns everything read back.
func exchange(t *testing.T, addr, raw string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, raw); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v (got %q)", err, got)
	}
	return string(got)
}

func TestForwardTunnels(t *testing.T) {
	cases := []struct {
		name     string
		status   string // upstream's status
		early    string // upstream bytes past its head
		send     string // client bytes after the CONNECT head, in the same write
		want     string // exactly what the client reads back
		wantLogs []string
	}{
		{
			name:     "pipelined bytes reach the far end and echo back",
			status:   "200 Connection established",
			send:     "\x16\x03\x01client-hello",
			want:     "HTTP/1.1 200 Connection established\r\n\r\n\x16\x03\x01client-hello",
			wantLogs: []string{"tunnel open", "tunnel closed", "bytes_up=15", "bytes_down=15", "target=twin.example:443"},
		},
		{
			name:   "upstream bytes past its head are not lost",
			status: "200 OK",
			early:  "server-first",
			send:   "ping",
			want:   "HTTP/1.1 200 OK\r\n\r\nserver-firstping",
		},
		{
			name:     "407 is relayed without the upstream's challenge",
			status:   "407 Proxy Authentication Required",
			send:     "ignored",
			want:     "HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
			wantLogs: []string{"tunnel refused", "status=407"},
		},
		{
			name:   "403 is relayed",
			status: "403 Forbidden",
			want:   "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := startUpstream(t, tc.status, tc.early)
			addr, logs := startForwarder(t, up.url(), nil)

			got := exchange(t, addr, "CONNECT twin.example:443 HTTP/1.1\r\nHost: twin.example:443\r\n\r\n"+tc.send)
			if got != tc.want {
				t.Fatalf("client read\n%q\nwant\n%q", got, tc.want)
			}
			auths, targets := up.seen()
			if len(auths) != 1 || auths[0] != wantAuth {
				t.Errorf("upstream Proxy-Authorization = %q, want [%q]", auths, wantAuth)
			}
			if len(targets) != 1 || targets[0] != "twin.example:443" {
				t.Errorf("upstream CONNECT target = %q", targets)
			}
			waitForLog(t, logs, tc.wantLogs)
		})
	}
}

// waitForLog waits for every substring to appear: the close line is written
// after the client has already read its EOF.
func waitForLog(t *testing.T, logs *syncBuffer, want []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		missing := ""
		for _, w := range want {
			if !strings.Contains(logs.String(), w) {
				missing = w
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log lacks %q:\n%s", missing, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwardRefusals(t *testing.T) {
	cases := []struct {
		name    string
		request string
		status  int
	}{
		{"absolute-form GET", "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n", http.StatusNotImplemented},
		{"PAC path with no --pac-file", "GET /proxy.pac HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", http.StatusNotImplemented},
		{"POST", "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n", http.StatusNotImplemented},
		{"malformed head", "this is not http\r\n\r\n", http.StatusBadRequest},
		{"CONNECT without a port", "CONNECT twin.example HTTP/1.1\r\nHost: twin.example\r\n\r\n", http.StatusBadRequest},
		{"head too large", "CONNECT twin.example:443 HTTP/1.1\r\nX-Pad: " + strings.Repeat("a", maxHeadBytes) + "\r\n\r\n", http.StatusRequestHeaderFieldsTooLarge},
	}
	up := startUpstream(t, "200 OK", "")
	addr, _ := startForwarder(t, up.url(), nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := exchangeLenient(t, addr, tc.request)
			want := "HTTP/1.1 " + strconv.Itoa(tc.status) + " " + http.StatusText(tc.status) + "\r\n"
			if !strings.HasPrefix(got, want) {
				t.Fatalf("got %q, want a %d", got, tc.status)
			}
		})
	}
	if auths, _ := up.seen(); len(auths) != 0 {
		t.Errorf("a refused request reached the upstream: %q", auths)
	}
}

// exchangeLenient is exchange for a request the forwarder may answer before
// it has read all of it, so the write may fail with a reset.
func exchangeLenient(t *testing.T, addr, raw string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	go func() { _, _ = io.WriteString(c, raw) }()
	got, _ := io.ReadAll(c)
	return string(got)
}

func TestForwardUpstreamDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	addr, logs := startForwarder(t, "http://"+testUser+":"+testPass+"@"+dead, nil)

	got := exchange(t, addr, "CONNECT twin.example:443 HTTP/1.1\r\n\r\n")
	if !strings.HasPrefix(got, "HTTP/1.1 502 Bad Gateway\r\n") {
		t.Fatalf("got %q, want a 502", got)
	}
	waitForLog(t, logs, []string{"upstream unreachable", "upstream=" + dead})
}

func TestForwardHeadTimeout(t *testing.T) {
	up := startUpstream(t, "200 OK", "")
	addr, _ := startForwarder(t, up.url(), func(f *Forwarder) { f.HeadTimeout = 100 * time.Millisecond })
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "CONNECT twin.example:443 HTTP/1.1\r\n") // and never the blank line
	got, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(got), "HTTP/1.1 408 ") {
		t.Fatalf("got %q, want a 408", got)
	}
}

func TestForwardServesPAC(t *testing.T) {
	pac := filepath.Join(t.TempDir(), "proxy.pac")
	write := func(body string) {
		if err := os.WriteFile(pac, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`function FindProxyForURL(u, h) { return "PROXY 127.0.0.1:3128"; }`)
	up := startUpstream(t, "200 OK", "")
	addr, _ := startForwarder(t, up.url(), func(f *Forwarder) { f.PACFile = pac })

	get := func() *http.Response {
		t.Helper()
		resp, err := http.Get("http://" + addr + PACPath)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "PROXY 127.0.0.1:3128") {
		t.Fatalf("GET %s = %d %q", PACPath, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ns-proxy-autoconfig" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q", cc)
	}

	// Rewritten in place: the next request serves the new bytes.
	write(`function FindProxyForURL(u, h) { return "DIRECT"; }`)
	resp = get()
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "DIRECT") {
		t.Errorf("after a rewrite GET %s = %q", PACPath, body)
	}

	// Only that path: anything else is still 501.
	resp, err := http.Get("http://" + addr + "/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("GET /other = %d, want 501", resp.StatusCode)
	}
}

func TestForwardShutdownClosesOpenTunnels(t *testing.T) {
	up := startUpstream(t, "200 OK", "")
	upURL, err := ParseUpstream(up.url())
	if err != nil {
		t.Fatal(err)
	}
	f := &Forwarder{Upstream: upURL}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- f.Serve(ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "CONNECT twin.example:443 HTTP/1.1\r\n\r\n")
	head := make([]byte, len("HTTP/1.1 200 OK\r\n\r\n"))
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := f.Shutdown(ctx); err == nil {
		t.Error("Shutdown returned nil with a tunnel still open past its deadline")
	}
	if err := <-served; err != nil {
		t.Errorf("Serve = %v after Shutdown", err)
	}
	if _, err := io.ReadAll(c); err != nil {
		t.Errorf("the tunnel was not closed cleanly: %v", err)
	}
}

func TestCheckLoopback(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:3128", true},
		{"127.0.0.2:3128", true},
		{"[::1]:3128", true},
		{"localhost:3128", true},
		{"0.0.0.0:3128", false},
		{"[::]:3128", false},
		{":3128", false},
		{"10.0.0.5:3128", false},
		{"gw.example:3128", false},
		{"127.0.0.1", false},
	}
	for _, tc := range cases {
		err := CheckLoopback(tc.addr)
		if (err == nil) != tc.ok {
			t.Errorf("CheckLoopback(%q) = %v, want ok=%v", tc.addr, err, tc.ok)
		}
	}
}

func TestParseUpstream(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantAddr string
		wantErr  bool
	}{
		{"full", "http://" + testUser + ":" + testPass + "@gw.dev.example:8080", "gw.dev.example:8080", false},
		{"whitespace trimmed", "  http://" + testUser + ":" + testPass + "@gw.dev.example:8080\n", "gw.dev.example:8080", false},
		{"default port", "http://" + testUser + ":" + testPass + "@gw.dev.example", "gw.dev.example:80", false},
		{"no credential", "http://gw.dev.example:8080", "", true},
		{"https", "https://" + testUser + ":" + testPass + "@gw.dev.example:8080", "", true},
		{"unparseable", "http://" + testUser + ":" + testPass + "@gw.dev.example:8080/%zz", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, err := ParseUpstream(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				if strings.Contains(err.Error(), testPass) || strings.Contains(err.Error(), testUser) {
					t.Errorf("the error quotes the credential: %v", err)
				}
				return
			}
			if up.Addr != tc.wantAddr {
				t.Errorf("Addr = %q, want %q", up.Addr, tc.wantAddr)
			}
			if up.auth != wantAuth {
				t.Errorf("auth = %q, want %q", up.auth, wantAuth)
			}
		})
	}
}

func TestReadUpstreamFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upstream")
	if err := os.WriteFile(path, []byte("http://"+testUser+":"+testPass+"@gw.dev.example:8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var warned []string
	warn := func(msg string, _ ...any) { warned = append(warned, msg) }
	up, err := ReadUpstreamFile(path, warn)
	if err != nil {
		t.Fatal(err)
	}
	if up.Addr != "gw.dev.example:8080" || len(warned) != 0 {
		t.Errorf("Addr = %q, warnings %q", up.Addr, warned)
	}
	if _, err := ReadUpstreamFile(filepath.Join(dir, "missing"), warn); err == nil {
		t.Error("a missing file was accepted")
	}
}
