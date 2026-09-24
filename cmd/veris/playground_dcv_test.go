package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/veris-ai/veris-cli/internal/dcvrelay"
)

const fakeDCVTicket = "dcvtkt_secret_do_not_print"

// fakeWindowsBench is the bench API for a Windows session: one DCV ticket,
// then 409s, and the DCV relay under the ticket's base URL, which echoes.
type fakeWindowsBench struct {
	t       *testing.T
	tickets atomic.Int32
	revoked atomic.Bool

	mu    sync.Mutex
	paths []string
}

func (f *fakeWindowsBench) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/world-ssh/connect", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"world_id": "wld_1", "session_id": "ses_1", "token": fakeToken})
	})
	mux.HandleFunc("POST /v1/world-ssh/screen-ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+fakeToken {
			f.t.Errorf("screen-ticket Authorization %q", r.Header.Get("Authorization"))
		}
		if f.tickets.Add(1) > 1 {
			writeJSON(w, http.StatusConflict, map[string]string{"detail": "the Windows machine was stopped"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"protocol":   "dcv",
			"base_url":   "http://" + r.Host + "/v1/worlds/wld_1/ssh/ses_1/dcv/" + fakeDCVTicket + "/",
			"expires_at": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /v1/worlds/wld_1/ssh/ses_1/dcv/{ticket}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("ticket") != fakeDCVTicket || f.revoked.Load() {
			http.Error(w, "ticket expired", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.paths = append(f.paths, r.PathValue("path"))
		f.mu.Unlock()
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"dcv"}})
		if err != nil {
			return
		}
		for {
			typ, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			_ = ws.Write(r.Context(), typ, data)
		}
	})
	return mux
}

var dcvListeningRe = regexp.MustCompile(`127\.0\.0\.1:(\d+) \(Amazon DCV\)[\s\S]*Certificate SHA-256: ([0-9A-F:]{95})`)

// A Windows session's ticket turns the command into a DCV relay: a
// connection file for the relay is opened, a DCV-shaped client is served
// over TLS with the printed certificate and has its frames echoed by the
// desktop, and when the ticket is refused and no fresh one can be issued
// the command exits 0 saying the session ended. No secret is printed.
func TestPlaygroundScreenOpensAWindowsDesktopInDCV(t *testing.T) {
	var file atomic.Value
	openConnectionFile = func(path string) error {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read the connection file: %v", err)
		}
		file.Store(string(b))
		return nil
	}
	t.Cleanup(func() { openConnectionFile = openWithDefaultApp })

	f := &fakeWindowsBench{t: t}
	api := httptest.NewServer(f.handler())
	defer api.Close()
	stderr, done := runPlayground(t, "--api", api.URL, "--code", "vpc_test")

	var port, fingerprint string
	for deadline := time.Now().Add(10 * time.Second); port == "" && time.Now().Before(deadline); {
		if m := dcvListeningRe.FindStringSubmatch(stderr.String()); m != nil {
			port, fingerprint = m[1], m[2]
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the command ended before listening: %v\n%s", err, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if port == "" {
		t.Fatalf("the command never printed where it listens:\n%s", stderr.String())
	}
	if got, _ := file.Load().(string); !strings.Contains(got, "port="+port+"\n") || !strings.Contains(got, "certificatevalidationpolicy=accept-untrusted") {
		t.Errorf("connection file does not point at port %s:\n%s", port, got)
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // pinned below, as the DCV client's prompt would
		VerifyConnection: func(cs tls.ConnectionState) error {
			if got := dcvrelay.Fingerprint(cs.PeerCertificates[0].Raw); got != fingerprint {
				t.Errorf("served certificate %s, printed %s", got, fingerprint)
			}
			return nil
		},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss://127.0.0.1:"+port+"/auth", &websocket.DialOptions{HTTPClient: client, Subprotocols: []string{"dcv"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if typ, data, err := ws.Read(ctx); err != nil || typ != websocket.MessageText || string(data) != "hello" {
		t.Errorf("echo = %v %q %v", typ, data, err)
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")

	// The machine is stopped: its ticket is refused, and no fresh one is issued.
	f.revoked.Store(true)
	if _, _, err := websocket.Dial(ctx, "wss://127.0.0.1:"+port+"/ws", &websocket.DialOptions{HTTPClient: client}); err == nil {
		t.Error("a socket opened after the session ended")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("command = %v, want a clean exit when the session ends\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the command did not exit when the session ended:\n%s", stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{"DCV client connected", "The Playground session has ended: the Windows machine was stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{fakeToken, fakeDCVTicket} {
		if strings.Contains(out, secret) {
			t.Errorf("stderr printed a secret %q:\n%s", secret, out)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.paths, ",") != "auth" {
		t.Errorf("desktop relay saw %q, want [auth]", f.paths)
	}
}
