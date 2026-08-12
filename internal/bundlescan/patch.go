package bundlescan

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Overlay is one patched copy, ready to bind-mount read-only over the
// original's exact container path.
type Overlay struct {
	SDK           string
	HostPath      string
	ContainerPath string
}

// WriteOverlays writes one patched copy per candidate into dir: the original
// bytes, a newline guard, then the Veris CA. Append, never replace -- the
// same rule the trust material applies to the system bundle, because a copy
// holding only our root would break the SDK's every other connection.
//
// A candidate already carrying the Veris CA (same DER, wherever in the file)
// gets no overlay: there is nothing to add, and mounting a byte-identical
// copy would only add a moving part. Those come back as skipped, so the
// caller can say "found and already trusted" rather than nothing at all.
func WriteOverlays(dir string, caPEM []byte, cands []Candidate) (
	overlays []Overlay, skipped []Candidate, err error,
) {
	if len(cands) == 0 {
		return nil, nil, nil
	}
	der, err := pemDER(caPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("the Veris CA is not a usable PEM certificate: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}

	for i, c := range cands {
		if bundleContains(c.Content, der) {
			skipped = append(skipped, c)
			continue
		}
		patched := make([]byte, 0, len(c.Content)+len(caPEM)+1)
		patched = append(patched, c.Content...)
		if len(patched) > 0 && patched[len(patched)-1] != '\n' {
			patched = append(patched, '\n')
		}
		patched = append(patched, caPEM...)

		name := fmt.Sprintf("%d-%s-%s", i, slug(c.SDK), path.Base(c.ContainerPath))
		hostPath := filepath.Join(dir, name)
		// World-readable on purpose: the reader is the workload container's
		// own user, and a CA bundle holds no secret.
		if err := os.WriteFile(hostPath, patched, 0o644); err != nil {
			return nil, nil, fmt.Errorf("patch the %s bundle for %s: %w", c.SDK, c.ContainerPath, err)
		}
		overlays = append(overlays, Overlay{
			SDK:           c.SDK,
			HostPath:      hostPath,
			ContainerPath: c.ContainerPath,
		})
	}
	return overlays, skipped, nil
}

// slug flattens an SDK label into filename-safe characters.
func slug(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
