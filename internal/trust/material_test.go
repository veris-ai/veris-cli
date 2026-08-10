package trust

import (
	"crypto/x509"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCert = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----
`

// A client whose CA variable REPLACES its roots and is handed only the Veris
// certificate trusts the intercepted hosts and rejects the entire rest of the
// internet -- in passthrough mode, every unmapped vendor and package index.
func TestTheBundleCarriesThePublicRootsAsWellAsOurs(t *testing.T) {
	dir := t.TempDir()
	roots := filepath.Join(dir, "system-roots.pem")
	if err := os.WriteFile(roots, []byte(testCert), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERIS_SYSTEM_CA_BUNDLE", roots)

	m, err := Publish(filepath.Join(dir, "out"), []byte(testCert), "changeit")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if m.SystemRoots != roots {
		t.Fatalf("system roots came from %q, want %q", m.SystemRoots, roots)
	}

	bare, err := os.ReadFile(m.CertPath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(bare), "BEGIN CERTIFICATE"); n != 1 {
		t.Errorf("the bare certificate holds %d certificates, want 1", n)
	}
	bundle, err := os.ReadFile(m.BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(bundle), "BEGIN CERTIFICATE"); n != 2 {
		t.Errorf("the bundle holds %d certificates, want the roots plus ours", n)
	}
}

// A JKS the JVM cannot parse is the same as no JKS: the handshake fails and the
// run reaches the real vendor. The header is what the JVM sniffs first.
func TestTheTrustStoreIsAWellFormedJKS(t *testing.T) {
	dir := t.TempDir()
	roots := filepath.Join(dir, "system-roots.pem")
	if err := os.WriteFile(roots, []byte(testCert), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERIS_SYSTEM_CA_BUNDLE", roots)

	m, err := Publish(filepath.Join(dir, "out"), []byte(testCert), "changeit")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	raw, err := os.ReadFile(m.JKSPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 32 {
		t.Fatalf("the truststore is %d bytes, which cannot hold a certificate", len(raw))
	}
	if got := binary.BigEndian.Uint32(raw[0:4]); got != jksMagic {
		t.Errorf("magic is %#x, want %#x", got, jksMagic)
	}
	if got := binary.BigEndian.Uint32(raw[4:8]); got != jksVersion {
		t.Errorf("version is %d, want %d", got, jksVersion)
	}
	// One root plus ours: the store must carry the public roots, or every
	// passthrough host fails the handshake in the JVM too.
	if got := binary.BigEndian.Uint32(raw[8:12]); got != 2 {
		t.Errorf("entry count is %d, want the system root plus ours", got)
	}
	// The trailer is a SHA-1, so the file cannot end where the last entry does.
	if len(raw) < 12+20 {
		t.Error("no room for the digest trailer")
	}
}

// The password is fed to the digest as UTF-16BE with no length and no
// terminator. Getting it wrong yields a file every JVM rejects as tampered.
func TestThePasswordIsHashedAsJavaHashesIt(t *testing.T) {
	got := utf16BE("ab")
	want := []byte{0x00, 'a', 0x00, 'b'}
	if string(got) != string(want) {
		t.Fatalf("utf16BE(%q) = % x, want % x", "ab", got, want)
	}
}

// Two entries under one alias collapse into one in the JVM, which would drop a
// hundred and nineteen public roots and keep the last.
func TestEveryCertificateGetsItsOwnAlias(t *testing.T) {
	block, err := pemToDER([]byte(testCert + testCert))
	if err != nil {
		t.Fatal(err)
	}
	if len(block) != 2 {
		t.Fatalf("parsed %d certificates, want 2", len(block))
	}
	if _, err := x509.ParseCertificate(block[0]); err != nil {
		t.Fatalf("the extracted DER does not parse: %v", err)
	}

	raw, err := buildJKS(block, "changeit")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "veris-0") != 1 || strings.Count(string(raw), "veris-1") != 1 {
		t.Error("the two entries do not carry distinct aliases")
	}
}

// Node's --use-env-proxy is not ignored by a Node that lacks it: that Node
// refuses to start, so emitting it blind breaks every Node command the run was
// supposed to instrument.
func TestNodeOptionsIsEmittedOnlyWhenTheRuntimeAcceptsIt(t *testing.T) {
	base := Options{ProxyURL: "http://127.0.0.1:9", CACertPath: "/ca.pem"}

	if named(Build(base), "NODE_OPTIONS") != nil {
		t.Error("NODE_OPTIONS emitted for a runtime that does not accept it")
	}
	base.NodeAcceptsEnvProxy = true
	v := named(Build(base), "NODE_OPTIONS")
	if v == nil {
		t.Fatal("NODE_OPTIONS missing for a runtime that accepts it")
	}
	if !v.Append {
		t.Error("NODE_OPTIONS must extend whatever the image already set")
	}
}

// The kernel tiers already route below every library, so the trust-only set
// must carry the bundle too -- and still nothing that routes.
func TestTrustOnlyCarriesTheBundleAndNothingThatRoutes(t *testing.T) {
	vars := Build(Options{
		TrustOnly: true, CACertPath: "/ca.pem", CABundlePath: "/bundle.pem",
		ProxyURL: "http://127.0.0.1:9",
	})
	if v := named(vars, "SSL_CERT_FILE"); v == nil || v.Value != "/bundle.pem" {
		t.Errorf("SSL_CERT_FILE = %v, want the bundle", v)
	}
	if v := named(vars, "NODE_EXTRA_CA_CERTS"); v == nil || v.Value != "/ca.pem" {
		t.Errorf("NODE_EXTRA_CA_CERTS = %v, want the bare certificate", v)
	}
	for _, name := range []string{"HTTP_PROXY", "https_proxy", "ALL_PROXY", "NODE_OPTIONS"} {
		if named(vars, name) != nil {
			t.Errorf("%s would make the client cooperate, masking a broken redirect", name)
		}
	}
}

func named(vars []Var, name string) *Var {
	for i := range vars {
		if vars[i].Name == name {
			return &vars[i]
		}
	}
	return nil
}
