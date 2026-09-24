// Package vncrelay puts a remote desktop that speaks RFB (VNC) on a local
// port, for a VNC viewer on this machine to open.
//
// The two sides are handled differently on purpose. The upstream relay offers
// only security type None, and its handshake is completed here. The viewer's
// handshake is also completed here rather than passed through, because
// Apple's Screen Sharing stalls when a server offers it None: the viewer is
// always asked for VNC Authentication against a one-time password, and once
// both sides are through their handshakes the two byte streams are joined.
package vncrelay

import (
	"crypto/des"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"io"
	"math/big"
)

// ChallengeSize is the length of a VNC Authentication challenge and of the
// viewer's response to it.
const ChallengeSize = 16

// passwordAlphabet is what a one-time password is drawn from: letters and
// digits only, so it survives being typed at a prompt and being placed in a
// vnc:// URL without escaping.
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// PasswordLength is the length of a one-time password. VNC Authentication
// reads at most eight bytes of a password, so a longer one would not be
// stronger, only misleading.
const PasswordLength = 8

// NewPassword draws a one-time password of PasswordLength characters from
// [A-Za-z0-9] using r (crypto/rand.Reader when r is nil).
func NewPassword(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	max := big.NewInt(int64(len(passwordAlphabet)))
	b := make([]byte, PasswordLength)
	for i := range b {
		n, err := rand.Int(r, max)
		if err != nil {
			return "", fmt.Errorf("draw a one-time password: %w", err)
		}
		b[i] = passwordAlphabet[n.Int64()]
	}
	return string(b), nil
}

// AuthResponse is what a viewer that knows password answers to challenge:
// the challenge DES-encrypted in ECB mode, two 8-byte blocks, under a key
// made of the password's first eight bytes (NUL-padded) with each byte's
// bits reversed. The reversal is the protocol's historical quirk, not a
// choice; without it no real viewer's answer matches.
func AuthResponse(challenge [ChallengeSize]byte, password string) [ChallengeSize]byte {
	var key [8]byte
	copy(key[:], password)
	for i, b := range key {
		key[i] = reverseBits(b)
	}
	block, err := des.NewCipher(key[:])
	if err != nil {
		// des.NewCipher fails only on a key that is not 8 bytes long, and
		// this one always is.
		panic(err)
	}
	var out [ChallengeSize]byte
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out
}

// checkAuthResponse reports, in constant time, whether response is the
// right answer to challenge under password.
func checkAuthResponse(challenge, response [ChallengeSize]byte, password string) bool {
	want := AuthResponse(challenge, password)
	return subtle.ConstantTimeCompare(want[:], response[:]) == 1
}

func reverseBits(b byte) byte {
	var r byte
	for i := 0; i < 8; i++ {
		r = r<<1 | b&1
		b >>= 1
	}
	return r
}
