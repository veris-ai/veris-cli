package vncrelay

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"time"
)

// version38 is the only protocol version this package offers either side.
const version38 = "RFB 003.008\n"

// Security types (RFC 6143 §7.1.2).
const (
	securityNone    = 1
	securityVNCAuth = 2
)

// The deadlines on each handshake step. A viewer showing a password prompt
// waits on a person, so its response gets the longer one.
const (
	stepTimeout     = 30 * time.Second
	passwordTimeout = 120 * time.Second
)

// maxReason bounds a failure reason read from the relay: the protocol sends
// a length first, and a length that large is a broken peer, not a message.
const maxReason = 4096

// ErrWrongPassword is a viewer that answered the challenge with anything
// other than the one-time password. The viewer has been told so.
var ErrWrongPassword = errors.New("wrong password")

// UpstreamHandshake completes the client side of an RFB 3.8 handshake with
// the desktop relay, which must offer security type None. It returns once
// the relay's SecurityResult is OK; from there the relay expects the
// viewer's ClientInit, which the viewer itself sends.
func UpstreamHandshake(c net.Conn) error {
	defer c.SetDeadline(time.Time{})
	step := func() { _ = c.SetDeadline(time.Now().Add(stepTimeout)) }

	step()
	var v [12]byte
	if _, err := io.ReadFull(c, v[:]); err != nil {
		return fmt.Errorf("read the relay's protocol version: %w", err)
	}
	if major, minor, ok := parseVersion(v); !ok || major != 3 || minor < 8 {
		return fmt.Errorf("the relay speaks %q, not RFB 3.8", v[:])
	}
	step()
	if _, err := io.WriteString(c, version38); err != nil {
		return fmt.Errorf("send the protocol version: %w", err)
	}

	step()
	var count [1]byte
	if _, err := io.ReadFull(c, count[:]); err != nil {
		return fmt.Errorf("read the relay's security types: %w", err)
	}
	if count[0] == 0 {
		return fmt.Errorf("the relay refused the connection: %s", readReason(c))
	}
	types := make([]byte, count[0])
	if _, err := io.ReadFull(c, types); err != nil {
		return fmt.Errorf("read the relay's security types: %w", err)
	}
	if !slices.Contains(types, securityNone) {
		return fmt.Errorf("the relay offers security types %v, and not None (1)", types)
	}
	step()
	if _, err := c.Write([]byte{securityNone}); err != nil {
		return fmt.Errorf("choose security type None: %w", err)
	}

	step()
	result, err := readU32(c)
	if err != nil {
		return fmt.Errorf("read the relay's security result: %w", err)
	}
	if result != 0 {
		return fmt.Errorf("the relay refused the connection: %s", readReason(c))
	}
	return nil
}

// ViewerHandshake completes the server side of the handshake with a local
// viewer: it offers RFB 3.8, follows the viewer down to 3.7 or 3.3 when it
// answers with one, and requires VNC Authentication against password. The
// challenge is drawn from r (crypto/rand.Reader when nil). A wrong answer is
// reported to the viewer and returned as ErrWrongPassword.
//
// None is never offered: Apple's Screen Sharing answers a non-Apple server
// with 3.3 and then stalls on a None it was given no way to decline.
func ViewerHandshake(c net.Conn, password string, r io.Reader) error {
	defer c.SetDeadline(time.Time{})
	step := func(d time.Duration) { _ = c.SetDeadline(time.Now().Add(d)) }
	if r == nil {
		r = rand.Reader
	}

	step(stepTimeout)
	if _, err := io.WriteString(c, version38); err != nil {
		return fmt.Errorf("send the protocol version: %w", err)
	}
	var v [12]byte
	if _, err := io.ReadFull(c, v[:]); err != nil {
		return fmt.Errorf("read the viewer's protocol version: %w", err)
	}
	major, minor, ok := parseVersion(v)
	if !ok || major != 3 {
		return fmt.Errorf("the viewer speaks %q, not RFB 3.x", v[:])
	}

	// Before 3.7 the server names the one security type; from 3.7 it offers
	// a list and the viewer picks.
	step(stepTimeout)
	if minor < 7 {
		if err := writeU32(c, securityVNCAuth); err != nil {
			return fmt.Errorf("require VNC Authentication: %w", err)
		}
	} else {
		if _, err := c.Write([]byte{1, securityVNCAuth}); err != nil {
			return fmt.Errorf("offer VNC Authentication: %w", err)
		}
		var chosen [1]byte
		if _, err := io.ReadFull(c, chosen[:]); err != nil {
			return fmt.Errorf("read the viewer's security type: %w", err)
		}
		if chosen[0] != securityVNCAuth {
			return fmt.Errorf("the viewer chose security type %d, not VNC Authentication (2)", chosen[0])
		}
	}

	var challenge, response [ChallengeSize]byte
	if _, err := io.ReadFull(r, challenge[:]); err != nil {
		return fmt.Errorf("draw a challenge: %w", err)
	}
	step(stepTimeout)
	if _, err := c.Write(challenge[:]); err != nil {
		return fmt.Errorf("send the challenge: %w", err)
	}
	step(passwordTimeout)
	if _, err := io.ReadFull(c, response[:]); err != nil {
		return fmt.Errorf("read the viewer's password: %w", err)
	}

	step(stepTimeout)
	if checkAuthResponse(challenge, response, password) {
		if err := writeU32(c, 0); err != nil {
			return fmt.Errorf("send the security result: %w", err)
		}
		return nil
	}
	// 3.7 has no reason on a failed result; 3.8 has one, and so does the 3.3
	// failure Apple's viewer reads.
	_ = writeU32(c, 1)
	if minor != 7 {
		reason := ErrWrongPassword.Error()
		_ = writeU32(c, uint32(len(reason)))
		_, _ = io.WriteString(c, reason)
	}
	return ErrWrongPassword
}

// parseVersion reads "RFB xxx.yyy\n".
func parseVersion(v [12]byte) (major, minor int, ok bool) {
	s := string(v[:])
	if s[:4] != "RFB " || s[7] != '.' || s[11] != '\n' {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(s[4:7])
	minor, err2 := strconv.Atoi(s[8:11])
	return major, minor, err1 == nil && err2 == nil
}

// readReason reads the u32-length-prefixed reason that follows a refusal,
// or says why it could not.
func readReason(c io.Reader) string {
	n, err := readU32(c)
	if err != nil {
		return "no reason given"
	}
	if n > maxReason {
		return fmt.Sprintf("a reason of %d bytes, not read", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return "no reason given"
	}
	return string(b)
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

func writeU32(w io.Writer, v uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}
