package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/vncrelay"
)

const (
	fakeToken  = "tok_secret_do_not_print"
	fakeTicket = "tkt_secret_do_not_print"
)

// fakeServerInit stands in for the desktop's ServerInit: what a viewer must
// receive once it has sent its ClientInit through the relay.
var fakeServerInit = []byte("\x05\x00\x03\x20fake-server-init")

// syncBuffer is a buffer the command writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeBench is the bench API's world-ssh routes and the desktop relay
// behind them. connectStatus answers the code; the first screen ticket is
// issued and every later one answers ticketStatus.
type fakeBench struct {
	t             *testing.T
	connectStatus int
	ticketStatus  int
	tickets       atomic.Int32

	mu        sync.Mutex
	protocols []string // the Sec-WebSocket-Protocol the relay was dialled with
	auth      []string // the Authorization of each screen-ticket request
	queries   []string // the query string of each screen-ticket request
	agent     string   // the User-Agent of the connect request
}

func (f *fakeBench) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/world-ssh/connect", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Code string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Code != "vpc_test" {
			f.t.Errorf("connect body code %q, %v", body.Code, err)
		}
		f.mu.Lock()
		f.agent = r.Header.Get("User-Agent")
		f.mu.Unlock()
		if f.connectStatus != http.StatusOK {
			writeJSON(w, f.connectStatus, map[string]string{"detail": "connect code not found or already used"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"world_id": "wld_1", "session_id": "ses_1", "token": fakeToken, "expires_at": "2026-09-24T12:00:00Z",
		})
	})
	mux.HandleFunc("POST /v1/world-ssh/screen-ticket", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.queries = append(f.queries, r.URL.RawQuery)
		f.mu.Unlock()
		if f.tickets.Add(1) > 1 {
			writeJSON(w, f.ticketStatus, map[string]string{"detail": "the session was closed from the console"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"ticket":      fakeTicket,
			"url":         "ws://" + r.Host + "/desktop",
			"subprotocol": "veris-desktop",
			"expires_at":  "2026-09-24T12:00:00Z",
		})
	})
	mux.HandleFunc("GET /desktop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.protocols = strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
		f.mu.Unlock()
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"veris-desktop"}})
		if err != nil {
			f.t.Errorf("accept: %v", err)
			return
		}
		c := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		defer c.Close()
		f.speakRFB(c)
	})
	return mux
}

// speakRFB is the relay's side: RFB 3.8 offering only None, then the
// viewer's ClientInit in and a ServerInit out, then whatever follows until
// the viewer leaves.
func (f *fakeBench) speakRFB(c net.Conn) {
	_, _ = io.WriteString(c, "RFB 003.008\n")
	var v [12]byte
	if _, err := io.ReadFull(c, v[:]); err != nil || string(v[:]) != "RFB 003.008\n" {
		f.t.Errorf("relay read version %q, %v", v[:], err)
		return
	}
	_, _ = c.Write([]byte{1, 1})
	var chosen [1]byte
	if _, err := io.ReadFull(c, chosen[:]); err != nil || chosen[0] != 1 {
		f.t.Errorf("relay read security type %v, %v", chosen, err)
		return
	}
	_, _ = c.Write([]byte{0, 0, 0, 0})
	var clientInit [1]byte
	if _, err := io.ReadFull(c, clientInit[:]); err != nil || clientInit[0] != 1 {
		f.t.Errorf("relay read ClientInit %v, %v", clientInit, err)
		return
	}
	_, _ = c.Write(fakeServerInit)
	_, _ = io.Copy(io.Discard, c)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// runPlayground runs `veris playground screen` against api in the
// background and returns its stderr and the command's result.
func runPlayground(t *testing.T, args ...string) (*syncBuffer, <-chan error) {
	t.Helper()
	t.Setenv("VERIS_PLAYGROUND_CODE", "")
	t.Setenv("VERIS_BENCH_API", "")
	var stdout bytes.Buffer
	stderr := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- cli.Execute(root(), &cli.Globals{}, append([]string{"playground", "screen"}, args...), &stdout, stderr)
	}()
	return stderr, done
}

var listeningRe = regexp.MustCompile(`localhost:(\d+)[\s\S]*One-time password: ([A-Za-z0-9]{8})`)

// waitListening reads the port and password the command printed.
func waitListening(t *testing.T, stderr *syncBuffer, done <-chan error) (addr, password string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := listeningRe.FindStringSubmatch(stderr.String()); m != nil {
			return "127.0.0.1:" + m[1], m[2]
		}
		select {
		case err := <-done:
			t.Fatalf("the command ended before listening: %v\n%s", err, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("the command never printed where it listens:\n%s", stderr.String())
	return "", ""
}

// viewer33 is Apple's Screen Sharing as a non-Apple server sees it: it
// answers 3.3, is told VNC Authentication, answers the challenge with
// password, sends ClientInit and reads the ServerInit.
func viewer33(t *testing.T, addr, password string) []byte {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var v [12]byte
	if _, err := io.ReadFull(c, v[:]); err != nil {
		t.Fatalf("viewer read version: %v", err)
	}
	_, _ = io.WriteString(c, "RFB 003.003\n")
	var sec [4]byte
	if _, err := io.ReadFull(c, sec[:]); err != nil || binary.BigEndian.Uint32(sec[:]) != 2 {
		t.Fatalf("viewer read security type %v, %v; want 2", sec, err)
	}
	var challenge [vncrelay.ChallengeSize]byte
	if _, err := io.ReadFull(c, challenge[:]); err != nil {
		t.Fatalf("viewer read challenge: %v", err)
	}
	resp := vncrelay.AuthResponse(challenge, password)
	_, _ = c.Write(resp[:])
	var result [4]byte
	if _, err := io.ReadFull(c, result[:]); err != nil || binary.BigEndian.Uint32(result[:]) != 0 {
		t.Fatalf("viewer read security result %v, %v; want 0", result, err)
	}
	_, _ = c.Write([]byte{1}) // ClientInit, shared
	got := make([]byte, len(fakeServerInit))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("viewer read ServerInit: %v", err)
	}
	return got
}

// A connect code redeems, a Screen Sharing-shaped viewer is let in with the
// one-time password and receives the desktop's ServerInit through the
// relay, and when the session ends the next viewer's ticket is refused and
// the command exits 0 saying so. The token and ticket are never printed. A
// bench API that names no protocols is asked for none.
func TestPlaygroundScreenRelaysAViewerUntilTheSessionEnds(t *testing.T) {
	var opened atomic.Value
	openViewer = func(u string) error { opened.Store(u); return nil }
	clientOS = "darwin"
	t.Cleanup(func() { openViewer, clientOS = openWithDefaultApp, runtime.GOOS })

	f := &fakeBench{t: t, connectStatus: http.StatusOK, ticketStatus: http.StatusConflict}
	api := httptest.NewServer(f.handler())
	defer api.Close()

	// --profile is a global flag: accepted, and read by nothing here.
	stderr, done := runPlayground(t, "--api", api.URL+"/", "--code", "vpc_test", "--port", "0", "--profile", "nobody")
	addr, password := waitListening(t, stderr, done)

	if got := viewer33(t, addr, password); !bytes.Equal(got, fakeServerInit) {
		t.Errorf("viewer received %q, want the relay's ServerInit %q", got, fakeServerInit)
	}

	// The session has ended by the time a second viewer arrives.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("command = %v, want a clean exit when the session ends\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the command did not exit when the session ended:\n%s", stderr.String())
	}

	out := stderr.String()
	for _, want := range []string{"Viewer 1 connected", "The Playground session has ended: the session was closed from the console"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{fakeToken, fakeTicket} {
		if strings.Contains(out, secret) {
			t.Errorf("stderr printed a secret %q:\n%s", secret, out)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.protocols) != 2 || strings.TrimSpace(f.protocols[0]) != "veris-desktop" || strings.TrimSpace(f.protocols[1]) != fakeTicket {
		t.Errorf("relay dialled with subprotocols %q, want [veris-desktop %s]", f.protocols, fakeTicket)
	}
	if len(f.auth) != 2 || f.auth[0] != "Bearer "+fakeToken || f.auth[1] != "Bearer "+fakeToken {
		t.Errorf("screen-ticket Authorization %q, want the session token as Bearer on each", f.auth)
	}
	if strings.Join(f.queries, "") != "" {
		t.Errorf("screen-ticket queries %q, want none for a bench API that names no protocols", f.queries)
	}
	if f.agent != "veris/"+version {
		t.Errorf("User-Agent %q, want veris/%s", f.agent, version)
	}
	port := addr[strings.LastIndex(addr, ":")+1:]
	if got, _ := opened.Load().(string); got != "vnc://:"+password+"@localhost:"+port {
		t.Errorf("opened %q, want Screen Sharing at vnc://:<password>@localhost:%s", got, port)
	}
}

// A code that is unknown, expired or used is refused before anything
// listens, and the user is sent back to the console for a fresh one.
func TestPlaygroundScreenRefusedCodeSaysWhereToGetAFreshOne(t *testing.T) {
	f := &fakeBench{t: t, connectStatus: http.StatusNotFound}
	api := httptest.NewServer(f.handler())
	defer api.Close()

	stderr, done := runPlayground(t, "--api", api.URL, "--code", "vpc_test", "--no-open")
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not exit on a refused code")
	}
	var p printedError
	if !errors.As(err, &p) || p.code != 1 {
		t.Fatalf("command = %v, want an already-reported exit 1", err)
	}
	out := stderr.String()
	for _, want := range []string{"connect code not found or already used", "Copy a fresh command from the Playground's Screen Share tab"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
	if f.tickets.Load() != 0 {
		t.Error("a screen ticket was asked for after the code was refused")
	}
}

// Without a code or an API the command line is incomplete: a usage error,
// not a request that goes anywhere.
func TestPlaygroundScreenNeedsACodeAndAnAPI(t *testing.T) {
	cases := map[string][]string{
		"no code": {"--api", "https://bench.example"},
		"no api":  {"--code", "vpc_test"},
		"bad api": {"--api", "bench.example", "--code", "vpc_test"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, done := runPlayground(t, args...)
			var usage *cli.UsageError
			if err := <-done; !errors.As(err, &usage) {
				t.Errorf("command = %v, want a usage error", err)
			}
		})
	}
}

// A taken port is not a failure: the screen moves to a free one and says so.
func TestListenLocalFallsBackWhenThePortIsTaken(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	taken := busy.Addr().(*net.TCPAddr).Port
	ln, err := listenLocal(taken)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)
	if addr.Port == taken || !addr.IP.IsLoopback() {
		t.Errorf("listened on %s, want another port on loopback", addr)
	}
}
