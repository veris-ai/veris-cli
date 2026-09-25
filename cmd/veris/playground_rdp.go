package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/playground"
	"github.com/veris-ai/veris-cli/internal/tcprelay"
)

// rdpCredentialTarget is where Windows' credential manager keeps the
// Playground user's password for Remote Desktop Connection to find.
const rdpCredentialTarget = "TERMSRV/127.0.0.1"

// windowsAppStore is where a Mac gets Microsoft's Remote Desktop client.
const windowsAppStore = `"Windows App" from the Mac App Store`

// The seams a test replaces so no client is launched: lookPath finds a
// program, startApp starts one and leaves it running, runTool runs one to
// completion with stdin.
var (
	lookPath = exec.LookPath
	startApp = func(name string, args ...string) error {
		cmd := exec.Command(name, args...)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	runTool = func(stdin, name string, args ...string) error {
		cmd := exec.Command(name, args...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if msg := strings.TrimSpace(string(out)); err != nil && msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
)

// rdpConnectionFile is a .rdp file pointing a Remote Desktop client at the
// relay on port, signed in as username. The desktop's certificate is made
// for 127.0.0.1 and signed by no one; the tunnel to it is the trust, so the
// client is told not to insist on server authentication.
func rdpConnectionFile(port int, username string) string {
	lines := []string{
		"full address:s:127.0.0.1:" + strconv.Itoa(port),
		"username:s:" + username,
		"prompt for credentials:i:0",
		"authentication level:i:0",
		"screen mode id:i:1",
		"dynamic resolution:i:1",
		"smart sizing:i:1",
		"redirectclipboard:i:1",
		"audiomode:i:0",
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// serveRDP offers a Windows desktop to a Remote Desktop client, each of its
// TCP connections relayed over a WebSocket of its own, and opens the
// client signed in as the session's user unless --no-open.
func (s *screen) serveRDP(first *playground.Ticket, fetched time.Time) error {
	u := s.u
	ln, port, err := s.listen(0)
	if err != nil {
		return err
	}
	file, remove, err := writeConnectionFile("playground.rdp", rdpConnectionFile(port, first.Username))
	if err != nil {
		_ = ln.Close()
		return fail(u, "write", "the Remote Desktop connection file", err)
	}
	defer remove()

	u.Success("Playground Windows desktop on 127.0.0.1:%d (Remote Desktop)", port)
	// Written past Quiet: it is what the client signs in as.
	fmt.Fprintf(u.Out, "  Username: %s\n", first.Username)
	defer s.openRDP(file, port, first.Username, first.Password)()
	u.Info("While Remote Desktop is connected, the browser view shows the Windows sign-in screen; it comes back when Remote Desktop disconnects.")
	u.Info("Press Ctrl-C to stop.")

	onOpen, onClose := connectionEvents(u, "Remote Desktop connection")
	srv := &tcprelay.Server{Dial: s.ticketDial(first, fetched), OnOpen: onOpen, OnClose: onClose}
	if err := srv.Serve(s.bg, ln); err != nil {
		return screenEnded(u, err)
	}
	u.Info("Stopped.")
	return nil
}

// openRDP opens the client this OS has for Remote Desktop, or says how to
// connect one by hand, and returns what undoes it on the way out. The
// password is printed only where the client cannot be handed it.
func (s *screen) openRDP(file string, port int, username, password string) (cleanup func()) {
	u := s.u
	cleanup = func() {}
	printPassword := func() { fmt.Fprintf(u.Out, "  Password: %s\n", password) }
	byHand := func() {
		fmt.Fprintf(u.Out, "  Connection file: %s\n", file)
		u.Info("Open that file in a Remote Desktop client, or connect one to 127.0.0.1:%d as %s with the password above.", port, username)
	}
	if s.opts.noOpen {
		printPassword()
		byHand()
		return cleanup
	}

	switch clientOS {
	case "darwin":
		printPassword()
		if err := runTool(password, "pbcopy"); err == nil {
			u.Info("The password is copied to the clipboard.")
		}
		if err := openConnectionFile(file); err != nil {
			u.Warn("Could not open Remote Desktop: %v", err)
			fmt.Fprintf(u.Out, "  Install %s, then open the file below.\n", windowsAppStore)
			byHand()
			return cleanup
		}
		u.Info("Opening Windows App…")
	case "windows":
		err := runTool("", "cmdkey", "/generic:"+rdpCredentialTarget, "/user:"+username, "/pass:"+password)
		if err != nil {
			u.Warn("Could not store the password for Remote Desktop: %v", err)
			printPassword()
		} else {
			cleanup = func() { _ = runTool("", "cmdkey", "/delete:"+rdpCredentialTarget) }
		}
		if err := startApp("mstsc", file); err != nil {
			u.Warn("Could not open Remote Desktop Connection: %v", err)
			byHand()
			return cleanup
		}
		u.Info("Opening Remote Desktop Connection…")
	default:
		if freerdp := firstOnPath("xfreerdp3", "xfreerdp"); freerdp != "" {
			err := startApp(freerdp, "/v:127.0.0.1:"+strconv.Itoa(port), "/u:"+username, "/p:"+password,
				"/cert:ignore", "/dynamic-resolution", "+clipboard")
			if err == nil {
				u.Info("Opening FreeRDP…")
				return cleanup
			}
			u.Warn("Could not open FreeRDP: %v", err)
		}
		printPassword()
		if firstOnPath("remmina") == "" {
			u.Info("Install Remmina or FreeRDP (xfreerdp) to open Remote Desktop here.")
		} else if err := startApp("remmina", "-c", file); err != nil {
			u.Warn("Could not open Remmina: %v", err)
		} else {
			u.Info("Opening Remmina…")
			return cleanup
		}
		byHand()
	}
	return cleanup
}

// firstOnPath is the first of names found on PATH, or "".
func firstOnPath(names ...string) string {
	for _, name := range names {
		if _, err := lookPath(name); err == nil {
			return name
		}
	}
	return ""
}
