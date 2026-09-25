package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/dcvrelay"
	"github.com/veris-ai/veris-cli/internal/direct"
	"github.com/veris-ai/veris-cli/internal/playground"
	"github.com/veris-ai/veris-cli/internal/ui"
)

// dcvDownload is where the Amazon DCV client is had, free, for every
// desktop OS.
const dcvDownload = "https://www.amazondcv.com/"

// dcvResponseHeaderTimeout bounds how long the bench relay takes to start
// answering a resource request; the body itself (a download) may take longer.
const dcvResponseHeaderTimeout = time.Minute

// openConnectionFileTimeout is how long the OS's opener is given to say it
// has no application for a .dcv file; one still running past it has handed
// the file over.
const openConnectionFileTimeout = 10 * time.Second

// openConnectionFile hands a connection file (.dcv, .rdp) to the app
// registered for it; a variable so a test never launches a client.
var openConnectionFile = openWithDefaultApp

// serveDCV offers a Windows desktop to the Amazon DCV client: a TLS relay on
// loopback, a connection file pointing the client at it, opened in the
// client unless --no-open.
func (s *screen) serveDCV(first *playground.Ticket) error {
	u, o := s.u, s.opts
	cert, fingerprint, err := dcvrelay.NewCertificate(time.Now())
	if err != nil {
		return err
	}
	ln, port, err := s.listen(0)
	if err != nil {
		return err
	}
	file, remove, err := writeConnectionFile("playground.dcv", dcvrelay.ConnectionFile{Port: port}.Render())
	if err != nil {
		_ = ln.Close()
		return fail(u, "write", "the DCV connection file", err)
	}
	defer remove()

	u.Success("Playground Windows desktop on 127.0.0.1:%d (Amazon DCV)", port)
	// Written past Quiet: it is what the DCV client's certificate prompt shows.
	fmt.Fprintf(u.Out, "  Certificate SHA-256: %s\n", fingerprint)
	announceDCV(u, file, port, o.noOpen)
	u.Info("Press Ctrl-C to stop.")

	srv := s.dcvServer(first)
	if err := srv.Serve(s.bg, ln, cert); err != nil {
		return screenEnded(u, err)
	}
	u.Info("Stopped.")
	return nil
}

// dcvServer is the relay for this session: tickets from the bench API, the
// client's comings and goings on the terminal.
func (s *screen) dcvServer(first *playground.Ticket) *dcvrelay.Server {
	u := s.u
	transport := direct.Transport()
	if t, ok := transport.(*http.Transport); ok {
		t.ResponseHeaderTimeout = dcvResponseHeaderTimeout
	}
	return &dcvrelay.Server{
		First: dcvTicket(first),
		Ticket: func(ctx context.Context) (dcvrelay.Ticket, error) {
			t, err := s.client.ScreenTicket(ctx, s.session.Token, s.protocol)
			if sessionOver(err) {
				return dcvrelay.Ticket{}, &dcvrelay.StopError{Err: err}
			}
			if err != nil {
				return dcvrelay.Ticket{}, err
			}
			if t.Protocol != playground.ProtocolDCV {
				return dcvrelay.Ticket{}, fmt.Errorf("the bench API switched this session's desktop to %q", t.Protocol)
			}
			return dcvTicket(t), nil
		},
		Transport: transport,
		UserAgent: "veris/" + version,
		OnOpen: func(_ int, path string) {
			if isDCVChannel(path, "auth") {
				u.Info("DCV client connected")
			}
		},
		OnClose: func(_ int, path string, err error) {
			switch {
			case err != nil:
				u.Warn("DCV %s connection ended: %v", strings.TrimPrefix(path, "/"), err)
			case isDCVChannel(path, "ws"):
				u.Info("DCV client disconnected")
			}
		},
		OnRefuse: func(method, path string) {
			u.Warn("The DCV client asked %s %s, which the Playground relay does not carry", method, path)
		},
	}
}

// isDCVChannel reports whether path is the socket named name: "/auth" for
// authentication, "/ws" for the main channel whose end is the client's.
func isDCVChannel(path, name string) bool {
	return path == "/"+name || strings.HasSuffix(path, "/"+name)
}

// dcvTicket is a DCV ticket as the relay holds it. An expiry the bench API
// wrote in a form this cannot read is left zero, which the relay treats as
// short-lived.
func dcvTicket(t *playground.Ticket) dcvrelay.Ticket {
	exp, _ := time.Parse(time.RFC3339, t.ExpiresAt)
	return dcvrelay.Ticket{BaseURL: t.BaseURL, ExpiresAt: exp}
}

// announceDCV opens the connection file in the DCV client, or says how to
// connect it by hand: the file, or the address and session, and where to
// get the client when it is missing.
func announceDCV(u *ui.UI, file string, port int, noOpen bool) {
	if !noOpen {
		err := openConnectionFile(file)
		if err == nil {
			u.Info("Opening the Amazon DCV client…")
			return
		}
		u.Warn("Could not open the Amazon DCV client: %v", err)
		fmt.Fprintf(u.Out, "  Get it free at %s, then open the file below.\n", dcvDownload)
	}
	fmt.Fprintf(u.Out, "  Connection file: %s\n", file)
	u.Info("Open that file in the Amazon DCV client, or connect it to 127.0.0.1:%d#%s and choose Trust & Connect.",
		port, dcvrelay.ConsoleSession)
}

// writeConnectionFile writes body, readable by this user only, as name in a
// directory of its own, and returns its path and what removes it.
func writeConnectionFile(name, body string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "veris-playground-")
	if err != nil {
		return "", nil, err
	}
	remove := func() { _ = os.RemoveAll(dir) }
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		remove()
		return "", nil, err
	}
	return file, remove, nil
}

// openWithDefaultApp opens path with the application the OS has for it and
// reports an opener that exits failing, as macOS's open does for a file
// type no application claims.
func openWithDefaultApp(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return errors.New(msg)
			}
		}
		return err
	case <-time.After(openConnectionFileTimeout):
		return nil
	}
}
