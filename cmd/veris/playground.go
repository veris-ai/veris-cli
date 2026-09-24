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
	port   int
	noOpen bool
}

// playgroundCommand is `veris playground`: a bench Playground's cloud Mac,
// reached from this machine. It is the one command group that does not use
// the login: the bench API is a different service from the control plane,
// and the console's one-time connect code is the whole credential.
func playgroundCommand() *cli.Command {
	var o screenOptions
	screen := &cli.Command{
		Name:    "screen",
		Summary: "Open a Playground Mac's screen in Screen Sharing or any VNC viewer",
		Usage:   "veris playground screen --code CODE [--api URL] [--port N] [--no-open]",
		Help: `Copy the command from the Playground's Screen Share tab in the bench
console; it carries a one-time connect code and the bench API to redeem it
at. The code needs no login and no SSH key, and works once.

The screen is offered on 127.0.0.1 only, behind a one-time password printed
here. On macOS Screen Sharing is opened already logged in; elsewhere, or
with --no-open, point a VNC viewer at the address printed. Each viewer that
connects gets its own connection to the Playground; the command runs until
Ctrl-C or until the Playground session ends.

--code defaults to $VERIS_PLAYGROUND_CODE and --api to $VERIS_BENCH_API.
--port falls back to a free port when the one asked for is taken.`,
		Flags: func(fs *flag.FlagSet) {
			fs.StringVar(&o.code, "code", "", "the one-time connect `CODE` from the Screen Share tab")
			fs.StringVar(&o.api, "api", "", "the bench API's base `URL`")
			fs.IntVar(&o.port, "port", defaultScreenPort, "the local `PORT` to offer the screen on")
			fs.BoolVar(&o.noOpen, "no-open", false, "print where to connect instead of opening Screen Sharing")
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
		case o.port < 0 || o.port > 65535:
			return usage(fmt.Sprintf("--port %d is not a port", o.port))
		}
		if u, err := url.Parse(o.api); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return usage(fmt.Sprintf("--api %q is not an http(s) URL", o.api))
		}
		return playgroundScreen(ctx, o)
	}
	return &cli.Command{
		Name:    "playground",
		Summary: "A bench Playground's cloud Mac, from this machine: screen",
		Usage:   "veris playground screen --code CODE [--api URL] [--port N] [--no-open]",
		Sub:     []*cli.Command{screen},
	}
}

// playgroundScreen redeems the connect code, offers the screen on a local
// port and relays every viewer until the session ends or the user stops it.
// A session that ends is the normal way out and exits 0.
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

	password, err := vncrelay.NewPassword(nil)
	if err != nil {
		return err
	}
	ln, err := listenLocal(o.port)
	if err != nil {
		return fail(u, "listen", "on 127.0.0.1", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if o.port != 0 && port != o.port {
		u.Warn("Port %d is not available; using %d instead", o.port, port)
	}
	u.Success("Playground screen on localhost:%d", port)
	// Written past Quiet: without it a viewer that prompts cannot connect.
	fmt.Fprintf(u.Out, "  One-time password: %s\n", password)
	announceViewer(u, port, password, o.noOpen)
	u.Info("Press Ctrl-C to stop.")

	srv := &vncrelay.Server{
		Password: password,
		Dial: func(ctx context.Context) (net.Conn, error) {
			t, err := client.ScreenTicket(ctx, session.Token)
			if errors.Is(err, playground.ErrSessionEnded) || errors.Is(err, playground.ErrTokenRejected) {
				return nil, &vncrelay.StopError{Err: err}
			}
			if err != nil {
				return nil, err
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
	err = srv.Serve(bg, ln)
	switch {
	case err == nil:
		u.Info("Stopped.")
		return nil
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
