package bundlescan

import (
	"archive/tar"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

// testCA returns a freshly minted self-signed CA in PEM, the shape every real
// bundle is full of.
func testCA(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// testLeaf returns a CA-signed end-entity certificate: valid PEM, valid
// certificate, and NOT a trust root -- the client-auth material a bundle
// filename can collide with.
func testLeaf(t *testing.T) []byte {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Issuing Root"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "leaf.example"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

type tarEntry struct {
	name string
	typ  byte
	body []byte
	link string
}

func buildTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644}
		if e.typ == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		hdr.Linkname = e.link
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestValidateTellsBundlesFromLookalikes(t *testing.T) {
	if err := validate(testCA(t, "Real Root")); err != nil {
		t.Errorf("a self-signed CA must validate: %v", err)
	}
	if err := validate(testLeaf(t)); err == nil {
		t.Error("a CA-signed leaf is client-auth-shaped, not a bundle; it must be rejected")
	}
	if err := validate([]byte("not a pem at all")); err == nil {
		t.Error("junk must be rejected")
	}
	if err := validate(bytes.Repeat([]byte("A"), maxBundleSize+1)); err == nil {
		t.Error("an oversized file must be rejected before any parsing")
	}
	// A real bundle carries prose between blocks; the CA inside still counts.
	mixed := append([]byte("# Bundle of CA Root Certificates\n"), testCA(t, "Root")...)
	if err := validate(mixed); err != nil {
		t.Errorf("prose around the blocks is normal bundle shape: %v", err)
	}
}

func TestMatchRuleAnchorsAtASeparator(t *testing.T) {
	cases := map[string]string{
		"usr/lib/python3.12/site-packages/certifi/cacert.pem":                       "certifi",
		"usr/lib/python3.12/site-packages/pip/_vendor/certifi/cacert.pem":           "pip (vendored certifi)",
		"opt/venv/lib/python3.11/site-packages/botocore/cacert.pem":                 "botocore",
		"usr/local/lib/python3.12/site-packages/stripe/data/ca-certificates.crt":    "stripe",
		"var/lib/gems/3.2.0/gems/stripe-10.1.0/lib/stripe/data/ca-certificates.crt": "stripe",
		"usr/lib/python3/dist-packages/httplib2/cacerts.txt":                        "httplib2",
	}
	for p, want := range cases {
		r, ok := matchRule(p)
		if !ok || r.SDK != want {
			t.Errorf("matchRule(%q) = (%q, %v), want %q", p, r.SDK, ok, want)
		}
	}
	for _, p := range []string{
		// Bare bundle names outside a known SDK directory: fixtures and
		// client-auth material use them too.
		"app/testdata/cacert.pem",
		"etc/ssl/certs/ca-certificates.crt",
		// The suffix is anchored at a separator.
		"app/mycertifi/cacert.pem",
		"app/nonstripe/data/ca-certificates.crt",
	} {
		if r, ok := matchRule(p); ok {
			t.Errorf("matchRule(%q) matched %q; it must not", p, r.SDK)
		}
	}
}

func TestBundleContainsComparesByDER(t *testing.T) {
	ca := testCA(t, "Veris Local CA")
	other := testCA(t, "Unrelated Root")
	der, err := pemDER(ca)
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(append([]byte{}, other...), ca...)
	if !bundleContains(bundle, der) {
		t.Error("a bundle holding the exact certificate must report it")
	}
	if bundleContains(other, der) {
		t.Error("a bundle without the certificate must not report it")
	}
	// Re-wrapped PEM (different line breaks) still matches: the DER is the
	// identity, not the base64 layout.
	rewrapped := []byte(strings.ReplaceAll(string(ca), "\n", "\r\n"))
	if !bundleContains(append(append([]byte{}, other...), rewrapped...), der) {
		t.Error("layout changes must not defeat the dedupe")
	}
}
