// Package bundlescan locates the CA bundles that SDKs ship INSIDE the code
// under test -- certifi, botocore, stripe and friends -- so the container
// runner can over-mount each one with a copy that also carries the Veris CA.
//
// Environment variables cannot reach these files: an SDK that bundles its own
// roots loads them through its own code path and may read no variable at all,
// so the minted certificate is refused during the TLS handshake and the run
// fails with the diagnostics in internal/proxy/tlsreject.go. Patching the
// file keeps the SDK on the code path that ships; the bundle merely holds one
// more root.
package bundlescan

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"path"
	"strings"
)

// rule is one known bundled-CA location, matched as a slash-anchored path
// SUFFIX: site-packages prefixes vary per image, the tail does not. There is
// deliberately no bare cacert.pem rule -- that filename also names test
// fixtures and client-auth material, and quietly adding a root to one of
// those would change the code under test, not its trust.
type rule struct {
	// SDK names whose bundle this is, for the startup log.
	SDK    string
	Suffix string
}

// rules holds the known locations, most specific first: pip's vendored
// certifi also ends in certifi/cacert.pem, and the first match names the SDK.
var rules = []rule{
	{"pip (vendored certifi)", "pip/_vendor/certifi/cacert.pem"},
	{"certifi", "certifi/cacert.pem"},
	{"botocore", "botocore/cacert.pem"},
	// Matches both stripe-python's site-packages layout and the stripe-ruby
	// gem's lib/stripe/data.
	{"stripe", "stripe/data/ca-certificates.crt"},
	{"httplib2", "httplib2/cacerts.txt"},
}

// rulesFingerprint identifies the table's contents, so a cache entry written
// under an older table reads as a miss rather than pinning its match set
// after a rule lands. The unknown-candidate heuristic version rides the same
// hash: an entry cached before candidates existed must also read as a miss,
// or a rejection would report "no candidates" off a cache that never looked.
func rulesFingerprint() string {
	h := sha256.New()
	for _, r := range rules {
		h.Write([]byte(r.SDK))
		h.Write([]byte{0})
		h.Write([]byte(r.Suffix))
		h.Write([]byte{0})
	}
	h.Write([]byte(candidateHeuristicVersion))
	return hex.EncodeToString(h.Sum(nil))
}

// candidateHeuristicVersion names the current candidateBasename behavior;
// bump it when the heuristic changes so caches re-scan.
const candidateHeuristicVersion = "unknown-v1"

// maxUnknownReported caps how many unknown candidate paths a run reports. The
// report exists to hand an agent the one or two paths worth over-mounting;
// past a handful it stops informing and starts listing.
const maxUnknownReported = 6

// candidateBasename reports whether a file NAME is shaped like a CA bundle.
// This drives the report-only unknown-candidate channel: a file matching here
// but no rule in the table is never patched -- quietly adding a root to a
// test fixture would change the code under test -- but when a client refuses
// the minted certificate anyway, these paths are the difference between "an
// SDK the scan does not know" (over-mount it by hand) and "real pinning"
// (stop). Deliberately name-based and slightly broad: a false positive costs
// one printed line after an already-failed run, a false negative costs the
// agent a blind search.
func candidateBasename(name string) bool {
	base := strings.ToLower(path.Base(name))
	if stashNames[base] {
		return true
	}
	for _, marker := range []string{"cacert", "ca-cert", "ca_cert", "ca-bundle", "ca_bundle", "ca-certificates"} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	return false
}

// matchRule reports which rule a slash-separated, root-relative path
// satisfies. The suffix is anchored at a separator, so app/mycertifi/
// cacert.pem does not match the certifi rule.
func matchRule(p string) (rule, bool) {
	for _, r := range rules {
		if p == r.Suffix || strings.HasSuffix(p, "/"+r.Suffix) {
			return r, true
		}
	}
	return rule{}, false
}

// maxBundleSize bounds a candidate. certifi ships ~290KB and the largest
// system bundles run ~250KB, so 2MiB accepts every real bundle with room to
// grow while refusing to slurp something that merely reuses the filename.
const maxBundleSize = 2 << 20

// errTooLarge is the one place the size limit's wording lives, so the constant
// and its message stay together.
func errTooLarge(n int64) error {
	return fmt.Errorf("%d bytes is larger than any CA bundle (limit %d)", n, maxBundleSize)
}

// validate confirms content is a CA bundle rather than something that reuses
// a bundle's filename: PEM holding at least one CERTIFICATE that is a CA, or
// at least self-signed-root-shaped. A filename match alone is never enough.
func validate(content []byte) error {
	if len(content) > maxBundleSize {
		return errTooLarge(int64(len(content)))
	}
	sawBlock := false
	rest := content
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		sawBlock = true
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			continue
		}
		if cert.IsCA || bytes.Equal(cert.RawSubject, cert.RawIssuer) {
			return nil
		}
	}
	if !sawBlock {
		return errors.New("not a PEM file with a CERTIFICATE block, so not a CA bundle")
	}
	return errors.New("holds certificates but no CA or self-signed root, so not a CA bundle")
}

// Candidate is one validated bundled-CA file: where it sits in the workload
// container, and the bytes it holds today.
type Candidate struct {
	SDK           string
	ContainerPath string // absolute path inside the workload container
	Content       []byte

	// mountDest is the -v destination the candidate came in through, "" for
	// an image match. When two mounts supply the same container path, the
	// deeper destination is the copy docker leaves visible.
	mountDest string
}

// cleanTarPath normalises an archive member name to a root-relative,
// slash-separated path.
func cleanTarPath(name string) string {
	return path.Clean(strings.TrimPrefix(name, "/"))
}

// bundleContains reports whether the bundle already carries the certificate,
// compared by DER: whitespace and header text vary per writer, the block's
// bytes do not.
func bundleContains(bundle, der []byte) bool {
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" && bytes.Equal(block.Bytes, der) {
			return true
		}
	}
}
