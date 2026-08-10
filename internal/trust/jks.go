package trust

// A JKS writer, for trusted-certificate entries only.
//
// The JVM reads no PEM CA variable of any kind, so covering Java means handing
// it a keystore. Shelling out to `keytool` cannot do that here: the proxy runs
// in its own minimal container, the JDK lives in the WORKLOAD's container, and
// the two never share a filesystem beyond the mount this file is written into.
// A container tier that depended on the proxy image carrying a JDK would ship a
// JVM to every user in order to write four kilobytes.
//
// The format is small enough to write directly. It has been stable since JDK
// 1.2 and is still read by current JDKs through keystore compatibility mode,
// which sniffs the magic rather than trusting the file extension.
//
//	u4  0xFEEDFEED          magic
//	u4  2                   version
//	u4  n                   entry count
//	  per entry:
//	  u4  2                 tag: trusted certificate
//	  UTF alias             u2 length, then modified UTF-8
//	  u8  creation date     milliseconds since the epoch
//	  UTF "X.509"           certificate encoding
//	  u4  length            certificate DER length
//	  ..  DER
//	20  SHA-1 trailer
//
// The trailer is SHA-1 over the password as UTF-16BE, then the ASCII string
// below, then everything written above it. It authenticates the file against
// tampering; it is not encryption, and a truststore holds nothing secret.

import (
	"crypto/sha1" //nolint:gosec // the JKS format specifies SHA-1; not a security choice
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
)

const (
	jksMagic   = 0xFEEDFEED
	jksVersion = 2
	// jksTagTrustedCert is the entry kind that holds a certificate and no key.
	jksTagTrustedCert = 2
	// jksSalt is written into the digest verbatim by every JDK. It is not
	// configurable and not a secret; it is part of the format.
	jksSalt = "Mighty Aphrodite"
)

// jksCreationDate is fixed rather than the wall clock, so the same inputs
// produce the same bytes. Java only ever displays this field; nothing verifies
// it. A changing timestamp would make every start rewrite a file whose content
// is otherwise identical, which is noise in a shared mount two containers are
// both watching.
const jksCreationDate int64 = 1_600_000_000_000 // 2020-09-13T12:26:40Z

// buildJKS returns a JKS holding one trusted-certificate entry per DER given.
func buildJKS(ders [][]byte, password string) ([]byte, error) {
	if len(ders) == 0 {
		return nil, errors.New("a truststore with no certificates would trust nothing")
	}

	// Every byte written is also fed to the digest, so the writer and the hash
	// cannot drift apart.
	var out sink
	out.h = sha1.New() //nolint:gosec // format-mandated
	out.h.Write(utf16BE(password))
	out.h.Write([]byte(jksSalt))

	out.u4(jksMagic)
	out.u4(jksVersion)
	out.u4(uint32(len(ders)))

	for i, der := range ders {
		out.u4(jksTagTrustedCert)
		// Aliases must be unique; Java silently keeps the last entry under a
		// repeated one, which would collapse a hundred roots into one.
		out.utf(fmt.Sprintf("veris-%d", i))
		out.u8(uint64(jksCreationDate))
		out.utf("X.509")
		out.u4(uint32(len(der)))
		out.raw(der)
	}
	if out.err != nil {
		return nil, out.err
	}

	// The trailer is not itself hashed.
	return append(out.buf, out.h.Sum(nil)...), nil
}

// sink writes to a buffer and a hash at once.
type sink struct {
	buf []byte
	h   hash.Hash
	err error
}

func (s *sink) raw(b []byte) {
	s.buf = append(s.buf, b...)
	if _, err := s.h.Write(b); err != nil && s.err == nil {
		s.err = err
	}
}

func (s *sink) u4(v uint32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	s.raw(b[:])
}

func (s *sink) u8(v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	s.raw(b[:])
}

// utf writes Java's modified UTF-8: a u2 byte length, then the bytes. Aliases
// here are ASCII, where modified UTF-8 and UTF-8 agree, so the encoding is a
// straight copy -- but the LENGTH is in bytes, not runes, and getting that
// wrong desynchronises every entry after it.
func (s *sink) utf(str string) {
	if len(str) > 0xFFFF && s.err == nil {
		s.err = fmt.Errorf("alias %q is too long for a JKS string", str[:32])
		return
	}
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(len(str)))
	s.raw(b[:])
	s.raw([]byte(str))
}

// utf16BE is how Java feeds the password into the digest: each char as two
// big-endian bytes, with no length prefix and no terminator.
func utf16BE(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		// Keystore passwords are ASCII in every real deployment; a rune outside
		// the BMP would need a surrogate pair, and Java's own encoder produces
		// one. Encoding it as U+FFFD would silently change the password.
		if r > 0xFFFF {
			r = 0xFFFD
		}
		out = append(out, byte(r>>8), byte(r))
	}
	return out
}
