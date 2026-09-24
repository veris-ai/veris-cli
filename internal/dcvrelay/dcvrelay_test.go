package dcvrelay

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBench is the bench API's DCV relay: every path under /dcv/<ticket>/,
// admitted while the ticket is one it issued and has not revoked.
type fakeBench struct {
	t *testing.T

	mu        sync.Mutex
	valid     map[string]bool
	sockets   []string // path?query of each socket admitted
	protocols [][]string
	posts     []string // method path, body, content-type of each request
}

func newFakeBench(t *testing.T) (*fakeBench, *httptest.Server) {
	f := &fakeBench{t: t, valid: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeBench) admit(ticket string) { f.mu.Lock(); f.valid[ticket] = true; f.mu.Unlock() }
func (f *fakeBench) revoke(ticket string) {
	f.mu.Lock()
	delete(f.valid, ticket)
	f.mu.Unlock()
}

func (f *fakeBench) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/dcv/")
	ticket, path, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	admitted := ok && f.valid[ticket]
	f.mu.Unlock()
	if !admitted {
		http.Error(w, "ticket not admitted", http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		f.echoSocket(w, r, path)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.posts = append(f.posts, r.Method+" "+path+" "+string(body)+" "+r.Header.Get("Content-Type"))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Bench-Internal", "not for the client")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(append([]byte("reply:"), body...))
}

// echoSocket accepts "dcv" and sends every message back with its type.
func (f *fakeBench) echoSocket(w http.ResponseWriter, r *http.Request, path string) {
	f.mu.Lock()
	q := path
	if r.URL.RawQuery != "" {
		q += "?" + r.URL.RawQuery
	}
	f.sockets = append(f.sockets, q)
	f.protocols = append(f.protocols, strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ", "))
	f.mu.Unlock()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}})
	if err != nil {
		f.t.Errorf("bench accept: %v", err)
		return
	}
	ws.SetReadLimit(-1)
	for {
		typ, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		if string(data) == "close-4409" {
			_ = ws.Close(4409, "another viewer took the desktop")
			return
		}
		if err := ws.Write(r.Context(), typ, data); err != nil {
			return
		}
	}
}

// startRelay serves a relay whose tickets are issued in order from names,
// each admitted by the bench as it is issued, and returns its address and
// a client that trusts only its certificate.
type relayRun struct {
	addr   string
	client *http.Client
	issued atomic.Int32
	done   chan error
	cancel context.CancelFunc
	closes chan error
}

func startRelay(t *testing.T, f *fakeBench, bench *httptest.Server, names ...string) *relayRun {
	t.Helper()
	cert, fp, err := NewCertificate(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 32*3-1 {
		t.Errorf("fingerprint %q is not 32 colon-separated bytes", fp)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	run := &relayRun{addr: ln.Addr().String(), done: make(chan error, 1), closes: make(chan error, 16)}
	srv := &Server{
		Ticket: func(context.Context) (Ticket, error) {
			n := int(run.issued.Add(1))
			if n > len(names) {
				return Ticket{}, &StopError{Err: errors.New("session ended")}
			}
			f.admit(names[n-1])
			return Ticket{BaseURL: bench.URL + "/dcv/" + names[n-1] + "/", ExpiresAt: time.Now().Add(10 * time.Minute)}, nil
		},
		UserAgent: "veris/test",
		OnClose:   func(_ int, _ string, err error) { run.closes <- err },
	}
	ctx, cancel := context.WithCancel(context.Background())
	run.cancel = cancel
	go func() { run.done <- srv.Serve(ctx, ln, cert) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.done:
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after its context ended")
		}
	})

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	run.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return run
}

// dialClient opens a socket the way the DCV client does: TLS, subprotocol "dcv".
func (r *relayRun) dialClient(t *testing.T, path string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss://"+r.addr+path, &websocket.DialOptions{
		HTTPClient:   r.client,
		Subprotocols: []string{Subprotocol},
	})
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	if ws.Subprotocol() != Subprotocol {
		t.Errorf("negotiated %q, want %q", ws.Subprotocol(), Subprotocol)
	}
	ws.SetReadLimit(-1)
	return ws
}

// Text stays text and binary stays binary, both ways, on a socket whose path
// and query reach the bench under the ticket's base URL, offering "dcv".
func TestRelaysFramesWithTheirTypes(t *testing.T) {
	f, bench := newFakeBench(t)
	run := startRelay(t, f, bench, "tkt1")
	ws := run.dialClient(t, "/auth?sessionId=console")
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	big := bytes.Repeat([]byte{0xAB}, 200<<10) // past the library's 32 KiB default
	for _, m := range []struct {
		typ  websocket.MessageType
		data []byte
	}{{websocket.MessageText, []byte(`{"type":"auth"}`)}, {websocket.MessageBinary, []byte{0, 1, 2, 255}}, {websocket.MessageBinary, big}} {
		if err := ws.Write(ctx, m.typ, m.data); err != nil {
			t.Fatal(err)
		}
		typ, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != m.typ || !bytes.Equal(data, m.data) {
			t.Errorf("echo: type %v len %d, want type %v len %d", typ, len(data), m.typ, len(m.data))
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sockets) != 1 || f.sockets[0] != "auth?sessionId=console" {
		t.Errorf("bench saw sockets %q, want [auth?sessionId=console]", f.sockets)
	}
	if len(f.protocols) != 1 || len(f.protocols[0]) != 1 || f.protocols[0][0] != Subprotocol {
		t.Errorf("bench was offered %q, want [dcv]", f.protocols)
	}
}

// A close the desktop sends reaches the client with its code and reason,
// and is reported.
func TestForwardsTheDesktopsClose(t *testing.T) {
	f, bench := newFakeBench(t)
	run := startRelay(t, f, bench, "tkt1")
	ws := run.dialClient(t, "/ws")
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte("close-4409")); err != nil {
		t.Fatal(err)
	}
	_, _, err := ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != 4409 || ce.Reason != "another viewer took the desktop" {
		t.Errorf("client read %v, want the desktop's 4409 and its reason", err)
	}
	select {
	case got := <-run.closes:
		if got == nil || !strings.Contains(got.Error(), "4409") {
			t.Errorf("OnClose = %v, want the 4409", got)
		}
	case <-time.After(10 * time.Second):
		t.Error("OnClose was not called")
	}
}

// A resource POST goes through with its body and content type, and its
// answer comes back with its status, body and only DCV's headers; DELETE too.
func TestForwardsResourceRequests(t *testing.T) {
	f, bench := newFakeBench(t)
	run := startRelay(t, f, bench, "tkt1")
	resp, err := run.client.Post("https://"+run.addr+"/resource/cursor/c2Vz/1/7?x=1", "application/x-www-form-urlencoded", strings.NewReader("token=abc"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(body) != "reply:token=abc" || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("POST answered %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Bench-Internal") != "" {
		t.Error("a header outside DCV's set was passed back")
	}
	req, _ := http.NewRequest(http.MethodDelete, "https://"+run.addr+"/resource/cursor/c2Vz/1/7", nil)
	resp, err = run.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// The bench relay serves no GET; the relay refuses one without asking.
	resp, err = run.client.Get("https://" + run.addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET answered %d, want 405", resp.StatusCode)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{
		"POST resource/cursor/c2Vz/1/7 token=abc application/x-www-form-urlencoded",
		"DELETE resource/cursor/c2Vz/1/7  ",
	}
	if strings.Join(f.posts, "|") != strings.Join(want, "|") {
		t.Errorf("bench saw %q, want %q", f.posts, want)
	}
}

// A ticket the bench no longer admits is replaced, and the socket opens
// under the fresh one; when no fresh one can be issued the relay stops with
// the session's end.
func TestReplacesARefusedTicketAndStopsWhenTheSessionEnds(t *testing.T) {
	f, bench := newFakeBench(t)
	run := startRelay(t, f, bench, "tkt1", "tkt2")
	run.dialClient(t, "/auth").CloseNow()
	f.revoke("tkt1")
	run.dialClient(t, "/ws").CloseNow()
	f.mu.Lock()
	sockets := append([]string(nil), f.sockets...)
	f.mu.Unlock()
	if strings.Join(sockets, ",") != "auth,ws" || run.issued.Load() != 2 {
		t.Errorf("bench sockets %q after %d tickets, want auth then ws on a second ticket", sockets, run.issued.Load())
	}

	f.revoke("tkt2")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "wss://"+run.addr+"/ws", &websocket.DialOptions{HTTPClient: run.client})
	if err == nil {
		t.Fatal("a socket opened after the session ended")
	}
	select {
	case err := <-run.done:
		var stop *StopError
		if !errors.As(err, &stop) {
			t.Errorf("Serve = %v, want the StopError", err)
		}
		run.done <- err // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop when the session ended")
	}
}

// The ticket is reused until a refresh before its expiry, replaced then,
// and a ticket with no expiry or a short life is still held a while.
func TestTicketsRenewBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	fetches := 0
	tk := &tickets{
		refresh: time.Minute,
		now:     func() time.Time { return now },
		fetch: func(context.Context) (Ticket, error) {
			fetches++
			return Ticket{BaseURL: "https://b/" + string(rune('0'+fetches)) + "/", ExpiresAt: now.Add(10 * time.Minute)}, nil
		},
	}
	ctx := context.Background()
	first, _ := tk.current(ctx)
	now = now.Add(8 * time.Minute)
	same, _ := tk.current(ctx)
	now = now.Add(90 * time.Second) // 30 s before expiry
	fresh, _ := tk.current(ctx)
	if first != "https://b/1/" || same != first || fresh != "https://b/2/" {
		t.Errorf("tickets %q %q %q, want 1 reused then 2", first, same, fresh)
	}

	tk.seed(Ticket{BaseURL: "https://b/short/", ExpiresAt: now.Add(2 * time.Second)})
	if d := tk.due(); d < minHold {
		t.Errorf("a two-second ticket is due in %v; want it held at least %v", d, minHold)
	}
	tk.seed(Ticket{BaseURL: "https://b/none/"})
	if d := tk.due(); d != time.Minute {
		t.Errorf("a ticket with no expiry is due in %v, want a minute", d)
	}
	tk.invalidate("https://b/none/")
	if d := tk.due(); d != 0 {
		t.Errorf("an invalidated ticket is due in %v, want now", d)
	}
}

// The connection file is what the DCV docs describe, points at the relay
// over WebSockets, accepts the run's certificate and holds no secret.
func TestConnectionFileRenders(t *testing.T) {
	got := ConnectionFile{Port: 54321}.Render()
	want := `[version]
format=1.0

[connect]
host=127.0.0.1
port=54321
webport=54321
sessionid=console
transport=websocket
proxytype=NONE
certificatevalidationpolicy=accept-untrusted
`
	if got != want {
		t.Errorf("connection file:\n%s\nwant:\n%s", got, want)
	}
}

// The certificate verifies for 127.0.0.1 and localhost.
func TestCertificateNamesLoopback(t *testing.T) {
	cert, _, err := NewCertificate(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"127.0.0.1", "localhost"} {
		if err := cert.Leaf.VerifyHostname(host); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
}
