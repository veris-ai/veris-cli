package trust

// Trust material: the files a process under test reads in order to accept the
// proxy's certificate.
//
// There are three of them rather than one, because the variables that carry
// them do not all mean the same thing:
//
//   - NODE_EXTRA_CA_CERTS ADDS to the runtime's own roots, so it takes the bare
//     Veris certificate.
//   - REQUESTS_CA_BUNDLE, SSL_CERT_FILE, CURL_CA_BUNDLE and the rest REPLACE
//     the runtime's roots. Handing them the bare certificate makes the client
//     trust intercepted hosts and reject every other HTTPS host on the
//     internet -- which in the default passthrough mode is every unmapped
//     vendor, every package index, and every telemetry endpoint.
//   - The JVM reads neither, and needs a keystore.
//
// So the bare certificate and a public-roots-plus-Veris bundle are both
// published, and each variable gets the one it actually means.

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// File names, published side by side so a reader that knows one knows them all.
const (
	CertFileName   = "veris-ca.pem"
	BundleFileName = "veris-ca-bundle.pem"
	JKSFileName    = "veris-truststore.jks"
)

// Material is where each artifact ended up, as the READER will see it. In the
// container tiers that is a path inside a shared mount, not ours.
type Material struct {
	CertPath   string
	BundlePath string
	JKSPath    string
	// SystemRoots is the system bundle the public roots came from, or empty if
	// none was found -- in which case BundlePath holds only the Veris CA and
	// passthrough HTTPS will fail for the runtimes that read it.
	SystemRoots string
}

// systemBundles are the PEM bundles the common distributions ship, most
// specific first. Alpine and Debian agree on the first; macOS ships the last.
var systemBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL, Fedora, Amazon Linux
	"/etc/ssl/ca-bundle.pem",             // openSUSE
	"/etc/pki/tls/cacert.pem",            // older RHEL
	"/etc/ssl/cert.pem",                  // macOS, OpenBSD, Alpine
}

// Publish writes the certificate, the bundle and the JKS into dir and returns
// where they landed.
//
// storePass is the keystore password, which callers keep at "changeit": it is
// what every stock JDK uses for cacerts, and a truststore holds no secret.
func Publish(dir string, certPEM []byte, storePass string) (Material, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Material{}, err
	}
	m := Material{
		CertPath:   filepath.Join(dir, CertFileName),
		BundlePath: filepath.Join(dir, BundleFileName),
		JKSPath:    filepath.Join(dir, JKSFileName),
	}

	roots, source := systemRoots()
	m.SystemRoots = source

	// The Veris CA goes LAST. Both orders verify, but a reader inspecting the
	// file finds the one certificate that is not a public root at the end,
	// rather than buried among a hundred and twenty.
	bundle := AppendCA(roots, certPEM)

	if err := writeFile(m.CertPath, certPEM, 0o644); err != nil {
		return Material{}, err
	}
	if err := writeFile(m.BundlePath, bundle, 0o644); err != nil {
		return Material{}, err
	}

	ders, err := PEMToDER(bundle)
	if err != nil {
		return Material{}, fmt.Errorf("build the JVM truststore: %w", err)
	}
	jks, err := buildJKS(ders, storePass)
	if err != nil {
		return Material{}, fmt.Errorf("build the JVM truststore: %w", err)
	}
	if err := writeFile(m.JKSPath, jks, 0o644); err != nil {
		return Material{}, err
	}
	return m, nil
}

// systemRoots returns the first system bundle that exists, and its path.
func systemRoots() ([]byte, string) {
	if override := os.Getenv("VERIS_SYSTEM_CA_BUNDLE"); override != "" {
		if raw, err := os.ReadFile(override); err == nil && len(raw) > 0 {
			return raw, override
		}
	}
	for _, p := range systemBundles {
		raw, err := os.ReadFile(p)
		if err == nil && len(raw) > 0 {
			return raw, p
		}
	}
	return nil, ""
}

// AppendCA returns base with caPEM appended, guarded by a newline when base is
// non-empty and lacks a trailing one so two adjacent PEM blocks never fuse into
// an unparseable line. Append, never replace: a bundle holding only the added
// CA would break every other TLS connection the reader makes.
func AppendCA(base, caPEM []byte) []byte {
	out := make([]byte, 0, len(base)+len(caPEM)+1)
	out = append(out, base...)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	out = append(out, caPEM...)
	return out
}

// PEMToDER pulls the DER of every CERTIFICATE block out of a PEM bundle,
// skipping anything else the file happens to contain. A bundle with one
// unparseable block is still a bundle worth building from -- dropping the other
// hundred and nineteen roots over it would be the worse outcome.
func PEMToDER(raw []byte) ([][]byte, error) {
	var out [][]byte
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			out = append(out, block.Bytes)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no CERTIFICATE block found")
	}
	return out, nil
}

func writeFile(path string, body []byte, mode os.FileMode) error {
	// A truststore is world-readable on purpose: the reader is another
	// container's user, and none of these files holds a private key.
	if err := os.WriteFile(path, body, mode); err != nil {
		return err
	}
	// WriteFile honours the mode only when it creates the file, and these are
	// rewritten on every start.
	return os.Chmod(path, mode)
}
