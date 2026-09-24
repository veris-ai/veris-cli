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
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/playground"
	"github.com/veris-ai/veris-cli/internal/ui"
	"github.com/veris-ai/veris-cli/internal/vncrelay"
)

// defaultScreenPort is where the Playground's screen is offered unless
// --port says otherwise: VNC display :1, clear of a Mac's own Screen
// Sharing on 5900.
const defaultScreenPort = 5901

// openViewer hands a vnc:// URL to the desktop; a variable so a test never
// launches Screen Sharing.
var openViewer = openInBrowser

// playgroundContext is the command's lifetime: until Ctrl-C or SIGTERM.
// A variable so a test can end the command itself.
var playgroundContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// screenOptions are `playground screen`'s flags.
type screenOptions struct {
	code   string
	api    string
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
	o := screenOptions{port: portFlag{n: defaultScreenPort}}
	screen := &cli.Command{
		Name:    "screen",
		Summary: "Open a Playground's screen: a Mac in Screen Sharing, Windows in the Amazon DCV client",
		Usage:   "veris playground screen --code CODE [--api URL] [--port N] [--no-open]",
		Help: `Copy the command from the Playground's Screen Share tab in the bench
console; it carries a one-time connect code and the bench API to redeem it
at. The code needs no login and no SSH key, and works once.

A Mac's screen is offered on 127.0.0.1 only, behind a one-time password
printed here. On macOS Screen Sharing is opened already logged in;
elsewhere, or with --no-open, point a VNC viewer at the address printed.
Each viewer that connects gets its own connection to the Playground.

A Windows desktop is offered on 127.0.0.1 only, over TLS with a certificate
made for this run, to the Amazon DCV client (free, https://www.amazondcv.com/).
A connection file is written and opened in it; with --no-open, or when it
cannot be opened, open that file in the DCV client yourself.

The command runs until Ctrl-C or until the Playground session ends.

--code defaults to $VERIS_PLAYGROUND_CODE and --api to $VERIS_BENCH_API.
--port falls back to a free port when the one asked for is taken; a Windows
desktop is on a free port unless --port is given.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&o.code, "code", "", "the one-time connect `CODE` from the Screen Share tab")
			fs.StringVar(&o.api, "api", "", "the bench API's base `URL`")
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
		Usage:   "veris playground screen --code CODE [--api URL] [--port N] [--no-open]",
		Sub:     []*cli.Command{screen},
	}
}

// firstTicketFresh is how long the ticket asked for up front, to learn the
// desktop's protocol, is handed to a Mac's first viewer: well inside the
// bench API's minute, so it is still good when the viewer's dial reaches
// the relay.
const firstTicketFresh = 30 * time.Second

// playgroundScreen redeems the connect code, asks for a first ticket to
// learn whether the desktop is a Mac's (RFB) or Windows (DCV), offers it on
// a local port and relays until the session ends or the user stops it. A
// session that ends is the normal way out and exits 0.
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
	first, err := client.ScreenTicket(bg, session.Token)
	fetched := time.Now()
	if err != nil {
		return screenEnded(u, err)
	}
	s := &screen{u: u, bg: bg, client: client, session: session, opts: o}
	if first.Protocol == playground.ProtocolDCV {
		return s.serveDCV(first)
	}
	return s.serveRFB(first, fetched)
}

// screen is one `playground screen` run past the redeemed code.
type screen struct {
	u       *ui.UI
	bg      context.Context
	client  *playground.Client
	session *playground.Session
	opts    screenOptions
}

// serveRFB offers a Mac's screen to VNC viewers. The first viewer is given
// first, the ticket that named the protocol, while it is fresh; every other
// viewer asks for its own.
func (s *screen) serveRFB(first *playground.Ticket, fetched time.Time) error {
	u, bg, client, session, o := s.u, s.bg, s.client, s.session, s.opts
	var firstOnce sync.Once
	takeFirst := func() (t *playground.Ticket) {
		firstOnce.Do(func() {
			if time.Since(fetched) < firstTicketFresh {
				t = first
			}
		})
		return t
	}

	password, err := vncrelay.NewPassword(nil)
	if err != nil {
		return err
	}
	ln, err := listenLocal(o.port.n)
	if err != nil {
		return fail(u, "listen", "on 127.0.0.1", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if o.port.n != 0 && port != o.port.n {
		u.Warn("Port %d is not available; using %d instead", o.port.n, port)
	}
	u.Success("Playground screen on localhost:%d", port)
	// Written past Quiet: without it a viewer that prompts cannot connect.
	fmt.Fprintf(u.Out, "  One-time password: %s\n", password)
	announceViewer(u, port, password, o.noOpen)
	u.Info("Press Ctrl-C to stop.")

	srv := &vncrelay.Server{
		Password: password,
		Dial: func(ctx context.Context) (net.Conn, error) {
			t := takeFirst()
			if t == nil {
				var err error
				t, err = client.ScreenTicket(ctx, session.Token)
				if sessionOver(err) {
					return nil, &vncrelay.StopError{Err: err}
				}
				if err != nil {
					return nil, err
				}
			}
			return client.DialDesktop(ctx, t)
		},
		OnOpen: func(id int) { u.Info("Viewer %d connected", id) },
		OnClose: func(id int, err error) {
			var stop *vncrelay.StopError
			switch {
			case errors.As(err, &stop):
				// Said once, below, when Serve returns it.
			case err != nil:
				u.Warn("Viewer %d disconnected: %v", id, err)
			default:
				u.Info("Viewer %d disconnected", id)
			}
		},
	}
	if err := srv.Serve(bg, ln); err != nil {
		return screenEnded(u, err)
	}
	u.Info("Stopped.")
	return nil
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

// announceViewer opens Screen Sharing already logged in on macOS, or says
// where to point a viewer everywhere else. The password in the URL is what
// lets Screen Sharing skip its prompt; it is one-time and good only on
// this machine's loopback.
func announceViewer(u *ui.UI, port int, password string, noOpen bool) {
	addr := "localhost:" + strconv.Itoa(port)
	if runtime.GOOS == "darwin" && !noOpen {
		err := openViewer("vnc://:" + password + "@" + addr)
		if err == nil {
			u.Info("Opening Screen Sharing…")
			return
		}
		u.Warn("Could not open Screen Sharing: %v", err)
	}
	u.Info("Connect a VNC viewer (Screen Sharing, RealVNC Viewer, TigerVNC) to %s with the password above.", addr)
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
