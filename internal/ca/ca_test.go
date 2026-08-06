package ca

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"testing"
)

func TestLoadGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()

	first, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Reuse matters: if a second run minted a fresh CA, every trust store the
	// developer already configured would silently stop working.
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("reloading must reuse the existing CA, not generate a new one")
	}
}

func TestKeyIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	info, err := os.Stat(c.Dir() + "/" + keyFile)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("CA key mode = %o, want 600: it can mint a certificate for any host", perm)
	}
}

func TestLeafServesFullChain(t *testing.T) {
	c := mustCA(t)

	leaf, err := c.Leaf("api.stripe.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	// Leaf alone is the classic MITM bug: Node and anything on OpenSSL reject
	// it with UNABLE_TO_VERIFY_LEAF_SIGNATURE.
	if len(leaf.Certificate) != 2 {
		t.Fatalf("chain length = %d, want 2 (leaf + CA)", len(leaf.Certificate))
	}
}

func TestLeafVerifiesAgainstCA(t *testing.T) {
	c := mustCA(t)

	leaf, err := c.Leaf("api.stripe.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(c.CertPEM()) {
		t.Fatal("CA PEM did not parse")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		DNSName:   "api.stripe.com",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("leaf does not verify against its own CA: %v", err)
	}
}

func TestLeafHasSAN(t *testing.T) {
	c := mustCA(t)
	leaf, err := c.Leaf("api.stripe.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	// A CN without a SAN is rejected by every modern client.
	if len(leaf.Leaf.DNSNames) != 1 || leaf.Leaf.DNSNames[0] != "api.stripe.com" {
		t.Fatalf("DNSNames = %v, want [api.stripe.com]", leaf.Leaf.DNSNames)
	}
}

func TestLeafCachesAcrossPortVariants(t *testing.T) {
	c := mustCA(t)

	a, err := c.Leaf("api.stripe.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	b, err := c.Leaf("api.stripe.com:443")
	if err != nil {
		t.Fatalf("Leaf with port: %v", err)
	}
	if a != b {
		t.Fatal("host and host:port must share one cache entry")
	}
	if got := c.CacheLen(); got != 1 {
		t.Fatalf("cache size = %d, want 1", got)
	}
}

func TestLeafForIPAddress(t *testing.T) {
	c := mustCA(t)
	leaf, err := c.Leaf("10.0.0.5")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	if len(leaf.Leaf.IPAddresses) != 1 {
		t.Fatalf("an IP host must produce an IP SAN, got DNSNames=%v", leaf.Leaf.DNSNames)
	}
}

func TestLeafIsUsableAsTLSCertificate(t *testing.T) {
	c := mustCA(t)
	leaf, err := c.Leaf("api.stripe.com")
	if err != nil {
		t.Fatalf("Leaf: %v", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{*leaf}, MinVersion: tls.VersionTLS12}
	if len(cfg.Certificates) != 1 {
		t.Fatal("certificate did not survive being put into a tls.Config")
	}
}

func TestCARegeneratesWhenCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.WriteFile(dir+"/"+certFile, []byte("not a certificate"), 0o644); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	// Continuing with an unparseable CA would surface much later as a
	// confusing TLS handshake failure.
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load must recover from a corrupt CA, got %v", err)
	}
}

func mustCA(t *testing.T) *CA {
	t.Helper()
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}
