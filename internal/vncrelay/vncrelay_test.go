package vncrelay

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// counting is the challenge the known-good vectors were produced with:
// bytes 00 01 02 … 0f.
func counting() [ChallengeSize]byte {
	var c [ChallengeSize]byte
	for i := range c {
		c[i] = byte(i)
	}
	return c
}

// The vectors come from the implementation Apple's Screen Sharing accepted;
// a response that differs from them is one no real viewer would match.
func TestAuthResponseMatchesTheVectorsAppleAccepted(t *testing.T) {
	cases := map[string]string{
		"password": "b866924125c8eebb9debc1db61c538e2",
		"Ab3dE9xZ": "cc728831519308f953b0fd31c16e53af",
		"pw":       "858600d9af143c9e6541d3dd92a835d0",
	}
	for pw, want := range cases {
		got := AuthResponse(counting(), pw)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("AuthResponse(%q) = %x, want %s", pw, got, want)
		}
		if !checkAuthResponse(counting(), got, pw) {
			t.Errorf("checkAuthResponse refused the right answer for %q", pw)
		}
	}
	// Only the first eight bytes of a password count, as every viewer
	// truncates it the same way.
	if AuthResponse(counting(), "password") != AuthResponse(counting(), "password-and-more") {
		t.Error("a password past eight bytes changed the response")
	}
}

func TestNewPasswordIsEightLettersOrDigits(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		pw, err := NewPassword(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != PasswordLength || strings.Trim(pw, passwordAlphabet) != "" {
			t.Fatalf("password %q is not %d characters of [A-Za-z0-9]", pw, PasswordLength)
		}
		seen[pw] = true
	}
	if len(seen) < 20 {
		t.Errorf("20 draws gave %d distinct passwords", len(seen))
	}
}

// viewer plays a VNC viewer that answers with version and password. It
// returns the security type the server named (3.3) or offered (3.7+), the
// SecurityResult, and the failure reason when one followed.
func viewer(t *testing.T, c net.Conn, version, password string) (secType byte, result uint32, reason string, err error) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var v [12]byte
	if _, err = io.ReadFull(c, v[:]); err != nil {
		return
	}
	if string(v[:]) != "RFB 003.008\n" {
		t.Errorf("server offered %q", v[:])
	}
	if _, err = io.WriteString(c, version); err != nil {
		return
	}
	legacy := version < "RFB 003.007\n"
	if legacy {
		var n uint32
		if n, err = readU32(c); err != nil {
			return
		}
		secType = byte(n)
	} else {
		var types [2]byte
		if _, err = io.ReadFull(c, types[:]); err != nil {
			return
		}
		if types[0] != 1 {
			t.Errorf("server offered %d security types, want exactly 1", types[0])
		}
		secType = types[1]
		if _, err = c.Write([]byte{secType}); err != nil {
			return
		}
	}
	var challenge [ChallengeSize]byte
	if _, err = io.ReadFull(c, challenge[:]); err != nil {
		return
	}
	resp := AuthResponse(challenge, password)
	if _, err = c.Write(resp[:]); err != nil {
		return
	}
	if result, err = readU32(c); err != nil || result == 0 || version == "RFB 003.007\n" {
		return
	}
	reason = readReason(c)
	return
}

func TestViewerHandshake(t *testing.T) {
	cases := []struct {
		name, version, password string
		wantErr                 error
		wantResult              uint32
		wantReason              string
	}{
		{"3.3 right password", "RFB 003.003\n", "Ab3dE9xZ", nil, 0, ""},
		{"3.3 wrong password", "RFB 003.003\n", "nope", ErrWrongPassword, 1, "wrong password"},
		{"3.8 right password", "RFB 003.008\n", "Ab3dE9xZ", nil, 0, ""},
		{"3.8 wrong password", "RFB 003.008\n", "nope", ErrWrongPassword, 1, "wrong password"},
		{"3.7 wrong password", "RFB 003.007\n", "nope", ErrWrongPassword, 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()
			challenge := counting()
			done := make(chan error, 1)
			go func() { done <- ViewerHandshake(server, "Ab3dE9xZ", bytes.NewReader(challenge[:])) }()

			secType, result, reason, err := viewer(t, client, tc.version, tc.password)
			if err != nil {
				t.Fatalf("viewer: %v", err)
			}
			if secType != securityVNCAuth {
				t.Errorf("security type %d, want VNC Authentication (2)", secType)
			}
			if result != tc.wantResult || reason != tc.wantReason {
				t.Errorf("result %d %q, want %d %q", result, reason, tc.wantResult, tc.wantReason)
			}
			if err := <-done; !errors.Is(err, tc.wantErr) {
				t.Errorf("ViewerHandshake = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// A 3.8 viewer that declines VNC Authentication is not let in some other
// way.
func TestViewerHandshakeRefusesAnotherSecurityType(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- ViewerHandshake(server, "pw", nil) }()

	var buf [14]byte
	if _, err := io.ReadFull(client, buf[:12]); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(client, "RFB 003.008\n")
	if _, err := io.ReadFull(client, buf[12:14]); err != nil {
		t.Fatal(err)
	}
	_, _ = client.Write([]byte{securityNone})
	if err := <-done; err == nil || !strings.Contains(err.Error(), "not VNC Authentication") {
		t.Errorf("ViewerHandshake = %v, want a refusal of security type 1", err)
	}
}

// relay plays the upstream desktop relay's side of the handshake: it
// offers types and answers result.
func relay(t *testing.T, c net.Conn, types []byte, result uint32) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "RFB 003.008\n")
	var v [12]byte
	if _, err := io.ReadFull(c, v[:]); err != nil {
		return
	}
	if string(v[:]) != "RFB 003.008\n" {
		t.Errorf("client answered %q", v[:])
	}
	_, _ = c.Write(append([]byte{byte(len(types))}, types...))
	var chosen [1]byte
	if _, err := io.ReadFull(c, chosen[:]); err != nil {
		return
	}
	if chosen[0] != securityNone {
		t.Errorf("client chose security type %d, want None (1)", chosen[0])
	}
	_ = writeU32(c, result)
	if result != 0 {
		_ = writeU32(c, uint32(len("busy")))
		_, _ = io.WriteString(c, "busy")
	}
}

func TestUpstreamHandshake(t *testing.T) {
	cases := []struct {
		name    string
		types   []byte
		result  uint32
		wantErr string
	}{
		{"None offered", []byte{securityNone}, 0, ""},
		{"None among others", []byte{securityVNCAuth, securityNone}, 0, ""},
		{"None not offered", []byte{securityVNCAuth}, 0, "not None"},
		{"result refused", []byte{securityNone}, 1, "busy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, fake := net.Pipe()
			defer up.Close()
			defer fake.Close()
			go relay(t, fake, tc.types, tc.result)
			err := UpstreamHandshake(up)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("UpstreamHandshake = %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("UpstreamHandshake = %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}

// Serve end to end over loopback: a 3.3 viewer is authenticated, the
// relay's handshake is completed, the viewer's ClientInit reaches the relay
// and the ServerInit comes back; a Dial that stops the session ends Serve.
func TestServeRelaysAViewerAndStopsWhenTheSessionEnds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverInit := []byte("fake-server-init")
	ended := errors.New("session ended")
	var dials atomic.Int32
	srv := &Server{
		Password: "Ab3dE9xZ",
		Dial: func(context.Context) (net.Conn, error) {
			if dials.Add(1) > 1 {
				return nil, &StopError{Err: ended}
			}
			up, fake := net.Pipe()
			go func() {
				defer fake.Close()
				relay(t, fake, []byte{securityNone}, 0)
				var clientInit [1]byte
				if _, err := io.ReadFull(fake, clientInit[:]); err != nil || clientInit[0] != 1 {
					t.Errorf("ClientInit %v, %v", clientInit, err)
				}
				_, _ = fake.Write(serverInit)
			}()
			return up, nil
		},
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(context.Background(), ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, result, _, err := viewer(t, c, "RFB 003.003\n", "Ab3dE9xZ"); err != nil || result != 0 {
		t.Fatalf("viewer: result %d, %v", result, err)
	}
	_, _ = c.Write([]byte{1})
	got := make([]byte, len(serverInit))
	if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, serverInit) {
		t.Fatalf("viewer read %q, %v; want %q", got, err, serverInit)
	}
	c.Close()

	c2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	select {
	case err := <-served:
		if !errors.Is(err, ended) {
			t.Errorf("Serve = %v, want the session's end", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop when the session ended")
	}
	if _, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		t.Error("the listener is still open after the session ended")
	}
}
