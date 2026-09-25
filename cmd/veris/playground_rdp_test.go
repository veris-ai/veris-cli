package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/playground"
	"github.com/veris-ai/veris-cli/internal/ui"
)

const (
	fakeRDPTicket   = "rdptkt_secret_do_not_print"
	fakeRDPPassword = "Pw_secret_do_not_print"
	fakeRDPUser     = `.\vp-abc123`
)

// --app auto picks the app the client OS has built in from what the
// session offers; an explicit app is taken as asked when the session offers
// it or says nothing, and refused, naming the offer, when it does not.
func TestChooseProtocol(t *testing.T) {
	windowsDesktop := []string{"dcv", "rfb", "rdp"}
	mac := []string{"rfb"}
	cases := []struct {
		app, goos string
		offered   []string
		want, err string
	}{
		{app: "auto", goos: "darwin", offered: windowsDesktop, want: "rfb"},
		{app: "auto", goos: "linux", offered: windowsDesktop, want: "rfb"},
		{app: "auto", goos: "windows", offered: windowsDesktop, want: "rdp"},
		{app: "auto", goos: "freebsd", offered: windowsDesktop, want: "rfb"},
		{app: "auto", goos: "darwin", offered: []string{"dcv", "rdp"}, want: "dcv"},
		{app: "auto", goos: "linux", offered: []string{"dcv", "rdp"}, want: "rdp"},
		{app: "auto", goos: "windows", offered: []string{"dcv", "rfb"}, want: "dcv"},
		{app: "auto", goos: "windows", offered: mac, want: "rfb"},
		{app: "auto", goos: "windows", offered: []string{"spice"}, want: "spice"},
		{app: "auto", goos: "darwin", offered: nil, want: ""},
		{app: "vnc", goos: "windows", offered: windowsDesktop, want: "rfb"},
		{app: "dcv", goos: "darwin", offered: windowsDesktop, want: "dcv"},
		{app: "rdp", goos: "darwin", offered: nil, want: "rdp"},
		{app: "rdp", goos: "darwin", offered: mac, err: "cannot be opened with --app rdp; it offers vnc"},
		{app: "dcv", goos: "linux", offered: []string{"rfb", "rdp"}, err: "it offers vnc, rdp"},
	}
	for _, c := range cases {
		got, err := chooseProtocol(c.app, c.goos, c.offered)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("chooseProtocol(%s, %s, %q) = %q, %v; want an error naming %q", c.app, c.goos, c.offered, got, err, c.err)
		case c.err == "" && (err != nil || got != c.want):
			t.Errorf("chooseProtocol(%s, %s, %q) = %q, %v; want %q", c.app, c.goos, c.offered, got, err, c.want)
		}
	}
}

// The .rdp file points at the relay, signs in as the session's user without
// a prompt, and does not insist on the self-signed certificate.
func TestRDPConnectionFile(t *testing.T) {
	want := "full address:s:127.0.0.1:53389\r\n" +
		"username:s:.\\vp-abc123\r\n" +
		"prompt for credentials:i:0\r\n" +
		"authentication level:i:0\r\n" +
		"screen mode id:i:1\r\n" +
		"dynamic resolution:i:1\r\n" +
		"smart sizing:i:1\r\n" +
		"redirectclipboard:i:1\r\n" +
		"audiomode:i:0\r\n"
	if got := rdpConnectionFile(53389, fakeRDPUser); got != want {
		t.Errorf("rdpConnectionFile =\n%q\nwant\n%q", got, want)
	}
}

// launched records what the Remote Desktop launch seams were asked to do.
type launched struct {
	mu     sync.Mutex
	tools  []string // "name args… <stdin"
	starts []string // "name args…"
	files  []string // the contents of each connection file opened
	onPath map[string]bool
}

// stub replaces the launch seams for the test, as clientOS goos.
func (l *launched) stub(t *testing.T, goos string) {
	t.Helper()
	clientOS = goos
	lookPath = func(name string) (string, error) {
		if l.onPath[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	startApp = func(name string, args ...string) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.starts = append(l.starts, strings.Join(append([]string{name}, args...), " "))
		if name == "mstsc" || name == "remmina" {
			l.readFile(t, args[len(args)-1])
		}
		return nil
	}
	runTool = func(stdin, name string, args ...string) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.tools = append(l.tools, strings.Join(append([]string{name}, args...), " ")+" <"+stdin)
		return nil
	}
	openConnectionFile = func(path string) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.readFile(t, path)
		return nil
	}
	t.Cleanup(func() {
		clientOS = runtime.GOOS
		lookPath, startApp, runTool = defaultLookPath, defaultStartApp, defaultRunTool
		openConnectionFile = openWithDefaultApp
	})
}

func (l *launched) readFile(t *testing.T, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("read the connection file: %v", err)
	}
	if info, err := os.Stat(path); err == nil && runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("connection file mode %v, want 0600", info.Mode().Perm())
	}
	l.files = append(l.files, string(b))
}

// The launch seams as the package has them, for a test to put back.
var defaultLookPath, defaultStartApp, defaultRunTool = lookPath, startApp, runTool

// Each client OS opens its own Remote Desktop client, handed the password
// the way that client takes it, and prints the password only where the
// person has to type or paste it.
func TestOpenRDPPerClientOS(t *testing.T) {
	cases := []struct {
		name, goos    string
		onPath        []string
		tools, starts []string
		opened        bool
		printed       bool
		cleanupTools  []string
	}{
		{
			name: "macOS opens Windows App and copies the password", goos: "darwin",
			tools: []string{"pbcopy <" + fakeRDPPassword}, opened: true, printed: true,
		},
		{
			name: "Windows stores the credential for mstsc and removes it after", goos: "windows",
			tools:        []string{"cmdkey /generic:TERMSRV/127.0.0.1 /user:" + fakeRDPUser + " /pass:" + fakeRDPPassword + " <"},
			starts:       []string{"mstsc FILE"},
			cleanupTools: []string{"cmdkey /delete:TERMSRV/127.0.0.1 <"},
		},
		{
			name: "Linux prefers FreeRDP 3", goos: "linux", onPath: []string{"xfreerdp3", "xfreerdp", "remmina"},
			starts: []string{"xfreerdp3 /v:127.0.0.1:53389 /u:" + fakeRDPUser + " /p:" + fakeRDPPassword + " /cert:ignore /dynamic-resolution +clipboard"},
		},
		{
			name: "Linux falls back to Remmina", goos: "linux", onPath: []string{"remmina"},
			starts: []string{"remmina -c FILE"}, printed: true,
		},
		{name: "Linux with no client says what to install", goos: "linux", printed: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := &launched{onPath: map[string]bool{}}
			for _, p := range c.onPath {
				l.onPath[p] = true
			}
			l.stub(t, c.goos)
			file, remove, err := writeConnectionFile("playground.rdp", rdpConnectionFile(53389, fakeRDPUser))
			if err != nil {
				t.Fatal(err)
			}
			defer remove()
			var out syncBuffer
			s := &screen{u: ui.New(&out, nil)}
			cleanup := s.openRDP(file, 53389, fakeRDPUser, fakeRDPPassword)
			fix := func(xs []string) string { return strings.ReplaceAll(strings.Join(xs, "|"), "FILE", file) }
			if got := strings.Join(l.tools, "|"); got != fix(c.tools) {
				t.Errorf("tools run %q, want %q", got, fix(c.tools))
			}
			if got := strings.Join(l.starts, "|"); got != fix(c.starts) {
				t.Errorf("apps started %q, want %q", got, fix(c.starts))
			}
			if c.opened != (len(l.files) == 1 && len(l.starts) == 0) {
				t.Errorf("connection file opened %d times, want opened=%v", len(l.files), c.opened)
			}
			if got := strings.Contains(out.String(), fakeRDPPassword); got != c.printed {
				t.Errorf("password printed = %v, want %v:\n%s", got, c.printed, out.String())
			}
			l.tools = nil
			cleanup()
			if got := strings.Join(l.tools, "|"); got != fix(c.cleanupTools) {
				t.Errorf("cleanup ran %q, want %q", got, fix(c.cleanupTools))
			}
			if c.name == "Linux with no client says what to install" && !strings.Contains(out.String(), "Install Remmina or FreeRDP") {
				t.Errorf("no install hint:\n%s", out.String())
			}
		})
	}
}

// fakeRDPBench is the bench API for a Windows session offering dcv, rfb and
// rdp: one rdp ticket, then 409s, and the RDP relay, which echoes.
type fakeRDPBench struct {
	t       *testing.T
	tickets atomic.Int32

	mu        sync.Mutex
	queries   []string
	protocols []string
}

func (f *fakeRDPBench) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/world-ssh/connect", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"world_id": "wld_1", "session_id": "ses_1", "token": fakeToken, "protocols": []string{"dcv", "rfb", "rdp"},
		})
	})
	mux.HandleFunc("POST /v1/world-ssh/screen-ticket", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		f.mu.Unlock()
		if f.tickets.Add(1) > 1 {
			writeJSON(w, http.StatusConflict, map[string]string{"detail": "the Windows machine was stopped"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"protocol": "rdp", "ticket": fakeRDPTicket, "url": "ws://" + r.Host + "/rdp", "subprotocol": "veris-rdp",
			"expires_at": "2026-09-24T12:00:00Z", "username": fakeRDPUser, "password": fakeRDPPassword,
		})
	})
	mux.HandleFunc("GET /rdp", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.protocols = strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",")
		f.mu.Unlock()
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"veris-rdp"}})
		if err != nil {
			f.t.Errorf("accept: %v", err)
			return
		}
		c := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		defer c.Close()
		_, _ = io.Copy(c, c)
	})
	return mux
}

var rdpListeningRe = regexp.MustCompile(`127\.0\.0\.1:(\d+) \(Remote Desktop\)`)

// On Windows, auto opens a Windows desktop in Remote Desktop: the rdp
// ticket is asked for, mstsc is started on a .rdp file for the relay with
// the credential stored for it, the client's raw bytes cross the relay, and
// when the session ends the command exits 0 and removes the credential.
// No secret is printed.
func TestPlaygroundScreenOpensAWindowsDesktopInRemoteDesktop(t *testing.T) {
	l := &launched{onPath: map[string]bool{}}
	l.stub(t, "windows")
	f := &fakeRDPBench{t: t}
	api := httptest.NewServer(f.handler())
	defer api.Close()
	stderr, done := runPlayground(t, "--api", api.URL, "--code", "vpc_test")

	var port string
	for deadline := time.Now().Add(10 * time.Second); port == "" && time.Now().Before(deadline); {
		if m := rdpListeningRe.FindStringSubmatch(stderr.String()); m != nil {
			port = m[1]
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
	l.mu.Lock()
	if len(l.files) != 1 || !strings.Contains(l.files[0], "full address:s:127.0.0.1:"+port+"\r\n") ||
		!strings.Contains(l.files[0], "username:s:"+fakeRDPUser+"\r\n") {
		t.Errorf("mstsc was not given a .rdp file for port %s: %q", port, l.files)
	}
	l.mu.Unlock()

	c, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	x224 := []byte{0x03, 0x00, 0x00, 0x13, 0x0e, 0xe0}
	if _, err := c.Write(x224); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(x224))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != string(x224) {
		t.Fatalf("client read %x, %v; want its bytes echoed by the desktop", got, err)
	}
	_ = c.Close()

	// The machine is stopped: the next connection's ticket is refused.
	c2, err := net.Dial("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("command = %v, want a clean exit when the session ends\n%s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the command did not exit when the session ended:\n%s", stderr.String())
	}

	out := stderr.String()
	for _, want := range []string{
		"Username: " + fakeRDPUser,
		"the browser view shows the Windows sign-in screen",
		"Remote Desktop connection 1 connected",
		"The Playground session has ended: the Windows machine was stopped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{fakeToken, fakeRDPTicket, fakeRDPPassword} {
		if strings.Contains(out, secret) {
			t.Errorf("stderr printed a secret %q:\n%s", secret, out)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.tools) != 2 || !strings.HasPrefix(l.tools[0], "cmdkey /generic:TERMSRV/127.0.0.1") || l.tools[1] != "cmdkey /delete:TERMSRV/127.0.0.1 <" {
		t.Errorf("cmdkey calls %q, want the credential stored then removed", l.tools)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) != 2 || f.queries[0] != "protocol=rdp" || f.queries[1] != "protocol=rdp" {
		t.Errorf("screen-ticket queries %q, want protocol=rdp on each", f.queries)
	}
	if len(f.protocols) != 2 || strings.TrimSpace(f.protocols[0]) != "veris-rdp" || strings.TrimSpace(f.protocols[1]) != fakeRDPTicket {
		t.Errorf("relay dialled with subprotocols %q, want [veris-rdp %s]", f.protocols, fakeRDPTicket)
	}
}

// An app the session does not offer is refused before any ticket is asked
// for, naming what it does offer.
func TestPlaygroundScreenRefusesAnAppTheSessionDoesNotOffer(t *testing.T) {
	var tickets atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/world-ssh/connect", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"token": fakeToken, "protocols": []string{"rfb"}})
	})
	mux.HandleFunc("POST /v1/world-ssh/screen-ticket", func(w http.ResponseWriter, r *http.Request) {
		tickets.Add(1)
		writeJSON(w, http.StatusConflict, map[string]string{"detail": "not offered"})
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	stderr, done := runPlayground(t, "--api", api.URL, "--code", "vpc_test", "--app", "rdp")
	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not exit")
	}
	var p printedError
	if !errors.As(err, &p) || p.code != 1 {
		t.Fatalf("command = %v, want an already-reported exit 1", err)
	}
	if out := stderr.String(); !strings.Contains(out, "cannot be opened with --app rdp; it offers vnc") {
		t.Errorf("stderr does not name the offer:\n%s", out)
	}
	if tickets.Load() != 0 {
		t.Error("a screen ticket was asked for an app the session does not offer")
	}
}

// --app takes only the apps there are.
func TestPlaygroundScreenRefusesAnUnknownApp(t *testing.T) {
	_, done := runPlayground(t, "--api", "https://bench.example", "--code", "vpc_test", "--app", "spice")
	var usage *cli.UsageError
	if err := <-done; !errors.As(err, &usage) || !strings.Contains(err.Error(), "--app") {
		t.Errorf("command = %v, want a usage error naming --app", err)
	}
}

// A VNC viewer is opened as the client OS has one: Screen Sharing logged
// in on macOS, the vnc:// handler on Linux with a viewer suggested when
// there is none, and on Windows, which has none, Remote Desktop suggested
// when the session offers it.
func TestAnnounceViewerPerClientOS(t *testing.T) {
	cases := []struct {
		goos      string
		openErr   error
		offered   []string
		wantOpen  string
		wantLines []string
	}{
		{goos: "darwin", wantOpen: "vnc://:pw@localhost:5901", wantLines: []string{"Opening Screen Sharing"}},
		{goos: "linux", wantOpen: "vnc://127.0.0.1:5901", wantLines: []string{"Opening your VNC viewer"}},
		{goos: "linux", openErr: errors.New("no handler"), wantOpen: "vnc://127.0.0.1:5901",
			wantLines: []string{"Could not open a VNC viewer: no handler", "Remmina", "vncviewer 127.0.0.1::5901"}},
		{goos: "windows", offered: []string{"dcv", "rfb", "rdp"}, wantLines: []string{"no VNC viewer built in", "127.0.0.1:5901", "--app rdp"}},
	}
	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			var opened string
			openViewer = func(u string) error { opened = u; return c.openErr }
			clientOS = c.goos
			t.Cleanup(func() { openViewer, clientOS = openWithDefaultApp, runtime.GOOS })
			var out syncBuffer
			s := &screen{u: ui.New(&out, nil), session: &playground.Session{Protocols: c.offered}}
			s.announceViewer(5901, "pw", false)
			if opened != c.wantOpen {
				t.Errorf("opened %q, want %q", opened, c.wantOpen)
			}
			for _, want := range c.wantLines {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}
