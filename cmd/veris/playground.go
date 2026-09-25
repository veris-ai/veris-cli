package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/playground"
	"github.com/veris-ai/veris-cli/internal/tcprelay"
	"github.com/veris-ai/veris-cli/internal/ui"
	"github.com/veris-ai/veris-cli/internal/vncrelay"
)

// defaultScreenPort is where the Playground's screen is offered unless
// --port says otherwise: VNC display :1, clear of a Mac's own Screen
// Sharing on 5900.
const defaultScreenPort = 5901

// openViewer hands a vnc:// URL to the desktop; a variable so a test never
// launches Screen Sharing.
var openViewer = openWithDefaultApp

// clientOS is the OS this veris runs on, which decides the app a desktop is
// opened in; a variable so a test can be any OS.
var clientOS = runtime.GOOS

// The --app values, and the desktop protocol each opens.
var appProtocols = map[string]string{
	"vnc": playground.ProtocolRFB,
	"rdp": playground.ProtocolRDP,
	"dcv": playground.ProtocolDCV,
}

// preferredProtocols is the order --app auto tries a session's protocols in
// on each client OS: the app the OS has built in first.
var preferredProtocols = map[string][]string{
	"darwin":  {playground.ProtocolRFB, playground.ProtocolDCV, playground.ProtocolRDP},
	"linux":   {playground.ProtocolRFB, playground.ProtocolRDP, playground.ProtocolDCV},
	"windows": {playground.ProtocolRDP, playground.ProtocolDCV, playground.ProtocolRFB},
}

// playgroundContext is the command's lifetime: until Ctrl-C or SIGTERM.
// A variable so a test can end the command itself.
var playgroundContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// screenOptions are `playground screen`'s flags.
type screenOptions struct {
	code   string
	api    string
	app    string
	port   portFlag
	noOpen bool
}

// portFlag is --port, remembering whether it was given: a Mac's screen is
// offered on defaultScreenPort unless it says otherwise, a Windows
// desktop on a free port.
type portFlag struct {
	n   int
	set bool
}

func (p *portFlag) String() string { return strconv.Itoa(p.n) }

func (p *portFlag) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil {
		return errors.New("not a number")
	}
	p.n, p.set = n, true
	return nil
}

// playgroundCommand is `veris playground`: a bench Playground's cloud Mac
// or Windows desktop, reached from this machine. It is the one command group
// that does not use the login: the bench API is a different service from the
// control plane, and the console's one-time connect code is the whole
// credential.
func playgroundCommand() *cli.Command {
	o := screenOptions{app: "auto", port: portFlag{n: defaultScreenPort}}
	screen := &cli.Command{
		Name:    "screen",
		Summary: "Open a Playground's screen in a VNC viewer, Remote Desktop or the Amazon DCV client",
		Usage:   "veris playground screen --code CODE [--api URL] [--app auto|vnc|rdp|dcv] [--port N] [--no-open]",
		Help: `Copy the command from the Playground's Screen Share tab in the bench
console; it carries a one-time connect code and the bench API to redeem it
at. The code needs no login and no SSH key, and works once.

--app picks the app the desktop opens in. A Mac offers vnc; a Windows
desktop offers dcv, vnc and rdp. auto, the default, picks the one this
machine has built in: vnc on macOS and Linux, rdp (Remote Desktop) on
Windows.

vnc offers the screen on 127.0.0.1 only, behind a one-time password printed
here. On macOS Screen Sharing is opened already logged in; on Linux the
desktop's vnc:// handler (Remmina, say) is opened; elsewhere, or with
--no-open, point a VNC viewer at the address printed.

rdp offers Windows' Remote Desktop on 127.0.0.1 only, and opens it signed
in as the Playground's user: Windows App on macOS, Remote Desktop
Connection on Windows, FreeRDP or Remmina on Linux. The password is printed
where the client has to be given it. While Remote Desktop is connected the
browser view shows the Windows sign-in screen.

dcv offers the desktop on 127.0.0.1 only, over TLS with a certificate made
for this run, to the Amazon DCV client (free, https://www.amazondcv.com/).
A connection file is written and opened in it; with --no-open, or when it
cannot be opened, open that file in the DCV client yourself.

Each client connection gets its own connection to the Playground. The
command runs until Ctrl-C or until the Playground session ends.

--code defaults to $VERIS_PLAYGROUND_CODE and --api to $VERIS_BENCH_API.
--port falls back to a free port when the one asked for is taken; rdp and
dcv are on a free port unless --port is given.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&o.code, "code", "", "the one-time connect `CODE` from the Screen Share tab")
			fs.StringVar(&o.api, "api", "", "the bench API's base `URL`")
			fs.StringVar(&o.app, "app", o.app, "the `APP` to open the desktop in: auto, vnc, rdp or dcv")
			fs.Var(&o.port, "port", "the local `PORT` to offer the screen on")
			fs.BoolVar(&o.noOpen, "no-open", false, "print where to connect instead of opening the viewer")
		},
	}
	screen.Run = func(ctx *cli.Context, args []string) error {
		if err := noPositionals(ctx, args); err != nil {
			return err
		}
		if o.code == "" {
			o.code = os.Getenv("VERIS_PLAYGROUND_CODE")
		}
		if o.api == "" {
			o.api = os.Getenv("VERIS_BENCH_API")
		}
		usage := func(msg string) error {
			fmt.Fprint(ctx.Stderr, cli.Help(ctx.Path, screen, ctx.Globals))
			return &cli.UsageError{Msg: msg, Cmd: screen}
		}
		switch {
		case o.code == "":
			return usage("--code is required: copy the command from the Playground's Screen Share tab")
		case o.api == "":
			return usage("--api is required (or set VERIS_BENCH_API): copy the command from the Playground's Screen Share tab")
		case o.app != "auto" && appProtocols[o.app] == "":
			return usage(fmt.Sprintf("--app %q is not one of auto, vnc, rdp, dcv", o.app))
		case o.port.n < 0 || o.port.n > 65535:
			return usage(fmt.Sprintf("--port %d is not a port", o.port.n))
		}
		if u, err := url.Parse(o.api); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return usage(fmt.Sprintf("--api %q is not an http(s) URL", o.api))
		}
		return playgroundScreen(ctx, o)
	}
	return &cli.Command{
		Name:    "playground",
		Summary: "A bench Playground's cloud Mac or Windows desktop, from this machine: screen",
		Usage:   "veris playground screen --code CODE [--api URL] [--app auto|vnc|rdp|dcv] [--port N] [--no-open]",
		Sub:     []*cli.Command{screen},
	}
}

// firstTicketFresh is how long the ticket asked for up front, to learn the
// desktop's protocol, is handed to the first VNC or Remote Desktop
// connection: well inside the bench API's minute, so it is still good when
// the connection's dial reaches the relay.
const firstTicketFresh = 30 * time.Second

// playgroundScreen redeems the connect code, picks the protocol --app and
// the session's offer settle on, asks for a first ticket for it, offers the
// desktop on a local port and relays until the session ends or the user
// stops it. A session that ends is the normal way out and exits 0.
func playgroundScreen(ctx *cli.Context, o screenOptions) error {
	u := ui.New(ctx.Stderr, os.Stdin)
	// Viewers come and go on their own goroutines; their lines must not
	// interleave mid-write.
	u.Out = &lockedWriter{w: u.Out}
	if ctx.Globals != nil {
		u.Quiet = ctx.Globals.Quiet
	}
	bg, stop := playgroundContext()
	defer stop()

	client := playground.New(o.api, "veris/"+version)
	session, err := client.Redeem(bg, o.code)
	if errors.Is(err, playground.ErrCodeRejected) {
		u.Fail("The connect code was not accepted: %s", detailOf(err))
		fmt.Fprintln(u.Out, "  Copy a fresh command from the Playground's Screen Share tab.")
		return printed(1)
	}
	if err != nil {
		return fail(u, "redeem", "the connect code", err)
	}
	protocol, err := chooseProtocol(o.app, clientOS, session.Protocols)
	if err != nil {
		u.Fail("%v", err)
		return printed(1)
	}
	first, err := client.ScreenTicket(bg, session.Token, protocol)
	fetched := time.Now()
	if err != nil {
		return screenEnded(u, err)
	}
	if protocol != "" && first.Protocol != protocol {
		u.Fail("The bench API opened %s when %s was asked for; it may be too old for --app.",
			appName(first.Protocol), appName(protocol))
		return printed(1)
	}
	s := &screen{u: u, bg: bg, client: client, session: session, opts: o, protocol: protocol}
	switch first.Protocol {
	case playground.ProtocolDCV:
		return s.serveDCV(first)
	case playground.ProtocolRDP:
		return s.serveRDP(first, fetched)
	}
	return s.serveRFB(first, fetched)
}

// chooseProtocol is the protocol to ask the bench API for: the one --app
// names, or for auto the first of the session's offer in goos's order of
// preference. A session whose offer is unknown (an older bench API) gets
// "" for auto, which is its primary. An app the session does not offer is
// an error naming what it does.
func chooseProtocol(app, goos string, offered []string) (string, error) {
	if app != "auto" {
		protocol := appProtocols[app]
		if offered != nil && !slices.Contains(offered, protocol) {
			return "", fmt.Errorf("this Playground session cannot be opened with --app %s; it offers %s", app, appNames(offered))
		}
		return protocol, nil
	}
	if len(offered) == 0 {
		return "", nil
	}
	order, ok := preferredProtocols[goos]
	if !ok {
		order = preferredProtocols["linux"]
	}
	for _, p := range order {
		if slices.Contains(offered, p) {
			return p, nil
		}
	}
	return offered[0], nil
}

// appName is the --app value that opens protocol.
func appName(protocol string) string {
	for app, p := range appProtocols {
		if p == protocol {
			return app
		}
	}
	return protocol
}

// appNames lists protocols as --app values: "vnc, rdp".
func appNames(protocols []string) string {
	names := make([]string, len(protocols))
	for i, p := range protocols {
		names[i] = appName(p)
	}
	return strings.Join(names, ", ")
}

// screen is one `playground screen` run past the redeemed code.
type screen struct {
	u       *ui.UI
	bg      context.Context
	client  *playground.Client
	session *playground.Session
	opts    screenOptions
	// protocol is what every ticket is asked for: "" when the bench API
	// did not say what the session offers, so its primary.
	protocol string
}

// offers reports whether the session is known to offer protocol.
func (s *screen) offers(protocol string) bool {
	return slices.Contains(s.session.Protocols, protocol)
}

// serveRFB offers the screen to VNC viewers, each one's RFB stream relayed
// over a WebSocket of its own.
func (s *screen) serveRFB(first *playground.Ticket, fetched time.Time) error {
	u, o := s.u, s.opts
	password, err := vncrelay.NewPassword(nil)
	if err != nil {
		return err
	}
	ln, port, err := s.listen(defaultScreenPort)
	if err != nil {
		return err
	}
	u.Success("Playground screen on localhost:%d", port)
	// Written past Quiet: without it a viewer that prompts cannot connect.
	fmt.Fprintf(u.Out, "  One-time password: %s\n", password)
	s.announceViewer(port, password, o.noOpen)
	u.Info("Press Ctrl-C to stop.")

	onOpen, onClose := connectionEvents(u, "Viewer")
	srv := &vncrelay.Server{
		Password: password,
		Dial:     s.ticketDial(first, fetched),
		OnOpen:   onOpen,
		OnClose:  onClose,
	}
	if err := srv.Serve(s.bg, ln); err != nil {
		return screenEnded(u, err)
	}
	u.Info("Stopped.")
	return nil
}

// ticketDial opens the desktop relay's WebSocket for one VNC or Remote
// Desktop connection. The first connection is given first, the ticket that
// named the protocol, while it is fresh; every other one asks for its own. A
// refusal after which no ticket can be issued stops the relay.
func (s *screen) ticketDial(first *playground.Ticket, fetched time.Time) func(context.Context) (net.Conn, error) {
	var firstOnce sync.Once
	return func(ctx context.Context) (net.Conn, error) {
		var t *playground.Ticket
		firstOnce.Do(func() {
			if time.Since(fetched) < firstTicketFresh {
				t = first
			}
		})
		if t == nil {
			var err error
			t, err = s.client.ScreenTicket(ctx, s.session.Token, s.protocol)
			if sessionOver(err) {
				return nil, &tcprelay.StopError{Err: err}
			}
			if err != nil {
				return nil, err
			}
			if t.Protocol != first.Protocol {
				return nil, fmt.Errorf("the bench API switched this session's desktop to %q", t.Protocol)
			}
		}
		return s.client.DialDesktop(ctx, t)
	}
}

// connectionEvents are a relay's OnOpen and OnClose, telling the terminal
// "<noun> 1 connected" and how it ended.
func connectionEvents(u *ui.UI, noun string) (func(int), func(int, error)) {
	onOpen := func(id int) { u.Info("%s %d connected", noun, id) }
	onClose := func(id int, err error) {
		var stop *tcprelay.StopError
		switch {
		case errors.As(err, &stop):
			// Said once, when Serve returns it.
		case err != nil:
			u.Warn("%s %d disconnected: %v", noun, id, err)
		default:
			u.Info("%s %d disconnected", noun, id)
		}
	}
	return onOpen, onClose
}

// listen listens on 127.0.0.1 where --port asks, or on def when it does
// not (0: a free port), saying so when that port is taken and another is
// used instead.
func (s *screen) listen(def int) (net.Listener, int, error) {
	want := def
	if s.opts.port.set {
		want = s.opts.port.n
	}
	ln, err := listenLocal(want)
	if err != nil {
		return nil, 0, fail(s.u, "listen", "on 127.0.0.1", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if want != 0 && port != want {
		s.u.Warn("Port %d is not available; using %d instead", want, port)
	}
	return ln, port, nil
}

// sessionOver reports a ticket refusal after which no later ticket can be
// issued either.
func sessionOver(err error) bool {
	return errors.Is(err, playground.ErrSessionEnded) || errors.Is(err, playground.ErrTokenRejected)
}

// screenEnded says why the screen stopped, and is the command's result: a
// session that ended exits 0, a session no longer valid exits 1 with the way
// to a fresh one, anything else is a failure to serve.
func screenEnded(u *ui.UI, err error) error {
	switch {
	case errors.Is(err, playground.ErrSessionEnded):
		u.Info("The Playground session has ended: %s", detailOf(err))
		return nil
	case errors.Is(err, playground.ErrTokenRejected):
		u.Fail("The Playground session is no longer valid: %s", detailOf(err))
		fmt.Fprintln(u.Out, "  Copy a fresh command from the Playground's Screen Share tab.")
		return printed(1)
	}
	return fail(u, "serve", "the Playground screen", err)
}

// announceViewer opens a VNC viewer, or says where to point one. On macOS
// Screen Sharing is opened already logged in: the password in the URL is
// what lets it skip its prompt; it is one-time and good only on this
// machine's loopback. On Linux the desktop's vnc:// handler is opened.
// Windows has no VNC viewer built in, so it is told of Remote Desktop when
// the session offers it.
func (s *screen) announceViewer(port int, password string, noOpen bool) {
	u := s.u
	addr := "localhost:" + strconv.Itoa(port)
	switch clientOS {
	case "darwin":
		if !noOpen {
			err := openViewer("vnc://:" + password + "@" + addr)
			if err == nil {
				u.Info("Opening Screen Sharing…")
				return
			}
			u.Warn("Could not open Screen Sharing: %v", err)
		}
		u.Info("Connect a VNC viewer (Screen Sharing, RealVNC Viewer, TigerVNC) to %s with the password above.", addr)
	case "windows":
		u.Info("Windows has no VNC viewer built in: connect one (TigerVNC, RealVNC Viewer) to 127.0.0.1:%d with the password above.", port)
		if s.offers(playground.ProtocolRDP) {
			u.Info("Or use Remote Desktop: copy a fresh command from the Screen Share tab and add --app rdp.")
		}
	default:
		if !noOpen {
			err := openViewer("vnc://127.0.0.1:" + strconv.Itoa(port))
			if err == nil {
				u.Info("Opening your VNC viewer…")
				return
			}
			u.Warn("Could not open a VNC viewer: %v", err)
		}
		u.Info("Connect a VNC viewer to 127.0.0.1:%d with the password above: Remmina, or TigerVNC's vncviewer 127.0.0.1::%d", port, port)
	}
}

// listenLocal listens on 127.0.0.1:port, or on a port the OS picks when
// that one is taken. Port 0 asks for the OS's pick outright.
func listenLocal(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil || port == 0 {
		return ln, err
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// detailOf is the bench API's own words for a refusal, or the error itself.
func detailOf(err error) string {
	var e *playground.Error
	if errors.As(err, &e) && e.Detail != "" {
		return e.Detail
	}
	return err.Error()
}

// lockedWriter serialises writes from goroutines that share one writer.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
