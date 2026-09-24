// Package dcvrelay offers a Playground's Windows desktop to a native Amazon
// DCV client on this machine's loopback, and relays everything the client
// asks of it to the bench API's DCV relay under a ticketed base URL.
//
// The DCV client only speaks TLS, so the loopback side is served with a
// certificate made in memory for this run; the connection file tells the
// client to accept it. Nothing is written to disk but that file.
package dcvrelay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// certLifetime covers a command left running for weeks; the key never
// leaves this process, so a long life costs nothing.
const certLifetime = 90 * 24 * time.Hour

// NewCertificate makes a self-signed ECDSA P-256 certificate for 127.0.0.1
// and localhost, valid from an hour before now, and returns it with its
// SHA-256 fingerprint as colon-separated hex, the form DCV's certificate
// prompt shows.
func NewCertificate(now time.Time) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate the loopback TLS key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("generate the loopback certificate's serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "127.0.0.1", Organization: []string{"veris playground screen"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("sign the loopback certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	return cert, Fingerprint(der), nil
}

// Fingerprint is the SHA-256 of a DER certificate as colon-separated
// upper-case hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
