// Package ca manages the Veris local certificate authority used to intercept
// TLS traffic from the code under test.
//
// The CA private key never leaves the machine it is generated on. Leaf
// certificates for intercepted hosts are minted on demand and cached in
// memory for the lifetime of the process.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// caValidity is deliberately short. A Veris CA is a testing artifact, not
	// long-lived infrastructure, and a short life limits the blast radius if a
	// developer machine is compromised.
	caValidity = 825 * 24 * time.Hour

	// leafValidity only needs to outlive a test run.
	leafValidity = 30 * 24 * time.Hour

	// skew back-dates NotBefore so a client whose clock is slightly behind
	// still accepts a freshly minted leaf.
	skew = 5 * time.Minute

	certFile = "veris-ca.pem"
	keyFile  = "veris-ca-key.pem"
)

// CA mints leaf certificates for intercepted hosts.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	dir     string
	certPEM []byte

	mu    sync.RWMutex
	cache map[string]*tls.Certificate
}

// Load reads the CA from dir, generating one if it does not exist yet.
// Generation is idempotent: an existing, still-valid CA is always reused so
// that a CA already trusted by the developer's toolchain keeps working.
func Load(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create ca dir: %w", err)
	}

	certPath := filepath.Join(dir, certFile)
	keyPath := filepath.Join(dir, keyFile)

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)

	if certErr == nil && keyErr == nil {
		c, err := parse(certPEM, keyPEM)
		if err == nil && time.Now().Before(c.cert.NotAfter) {
			c.dir = dir
			c.certPEM = certPEM
			c.cache = map[string]*tls.Certificate{}
			return c, nil
		}
		// Fall through and regenerate. An unparseable or expired CA is not
		// worth preserving, and silently continuing with it would produce
		// confusing TLS errors much later.
	}

	return generate(dir)
}

func generate(dir string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ca key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Veris Local CA",
			Organization: []string{"Veris"},
			// The OU makes the CA identifiable in a keychain listing, which
			// matters when a developer wants to remove it later.
			OrganizationalUnit: []string{"Veris dependency sandbox interception"},
		},
		NotBefore:             now.Add(-skew),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create ca cert: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse generated ca: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ca key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := writeFileAtomic(filepath.Join(dir, certFile), certPEM, 0o644); err != nil {
		return nil, err
	}
	// 0600: the CA key can mint a certificate for any host, so it is the most
	// sensitive artifact this tool produces.
	if err := writeFileAtomic(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}

	return &CA{
		cert:    cert,
		key:     key,
		dir:     dir,
		certPEM: certPEM,
		cache:   map[string]*tls.Certificate{},
	}, nil
}

func parse(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("ca cert is not valid PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ca cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("ca key is not valid PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse ca key: %w", err)
	}

	return &CA{cert: cert, key: key}, nil
}

// CertPEM returns the PEM-encoded CA certificate, suitable for writing to a
// trust bundle.
func (c *CA) CertPEM() []byte { return c.certPEM }

// CertPath returns the on-disk path of the CA certificate. This is the value
// that goes into NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE and friends.
func (c *CA) CertPath() string { return filepath.Join(c.dir, certFile) }

// Dir returns the directory holding the CA material.
func (c *CA) Dir() string { return c.dir }

// NotAfter reports when the CA expires.
func (c *CA) NotAfter() time.Time { return c.cert.NotAfter }

// Fingerprint returns a short, stable identifier for this CA. The CLI prints
// it so a developer can tell whether the CA their toolchain trusts is the same
// one the running proxy is using.
func (c *CA) Fingerprint() string {
	sum := sha256Sum(c.cert.Raw)
	return fmt.Sprintf("%x", sum[:8])
}

// Leaf returns a certificate valid for host, minting and caching one if needed.
//
// host may carry a port; it is stripped. Both the cached key and the
// certificate SAN are based on the bare hostname, so api.stripe.com:443 and
// api.stripe.com share a single entry.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	name := stripPort(host)

	c.mu.RLock()
	cached, ok := c.cache[name]
	c.mu.RUnlock()
	if ok {
		return cached, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check: another goroutine may have minted this while we waited.
	if cached, ok := c.cache[name]; ok {
		return cached, nil
	}

	leaf, err := c.mint(name)
	if err != nil {
		return nil, err
	}
	c.cache[name] = leaf
	return leaf, nil
}

func (c *CA) mint(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name, Organization: []string{"Veris"}},
		NotBefore:    now.Add(-skew),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// A certificate with a CN but no SAN is rejected by every modern client, so
	// the SAN is the part that actually matters here.
	if ip := net.ParseIP(name); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{name}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("sign leaf for %s: %w", name, err)
	}

	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse leaf for %s: %w", name, err)
	}

	// Serving leaf + CA, not just the leaf. Clients that do not already have
	// the CA in the presented chain (notably Node and anything on OpenSSL)
	// fail with UNABLE_TO_VERIFY_LEAF_SIGNATURE otherwise.
	return &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
		Leaf:        parsed,
	}, nil
}

// CacheLen reports how many leaves are currently cached. Used by the status
// endpoint.
func (c *CA) CacheLen() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	return serial, nil
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// writeFileAtomic writes via a temp file and rename so a concurrent reader
// never observes a half-written CA.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return os.Rename(tmpName, path)
}
