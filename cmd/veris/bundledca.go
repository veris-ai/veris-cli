package main

// --patch-bundled-cas: the over-mount that closes the failure mode
// tlsreject.go can only diagnose, and the default for an SDK that bundles
// its own CA. An SDK shipping its own CA bundle
// reads no environment variable, so the containerised run finds each known
// bundle in the image and the user's -v mounts, appends the Veris CA to a
// COPY, and bind-mounts the copy read-only over the original's exact path.
// The SDK keeps loading its own bundle through its own code path; the file
// just holds one more root.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/veris-ai/veris-cli/internal/bundlescan"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/trust"
)

// bundlesSubdir is where the patched copies live under the share. The share
// itself is mounted read-write in the workload, so this subtree gets its own
// deeper read-only bind.
const bundlesSubdir = "bundles"

// bundledCAOverlays scans, patches and reports. scanLabel tags the scan's
// throwaway containers so teardown can reap what a signal strands. The
// failure posture is the binary's usual one: a known bundle that cannot be
// extracted, validated or patched aborts the run, because leaving it in play
// fails the workload's TLS later with everything here having looked healthy.
//
// The second return is the unknown-candidate report: CA-bundle-shaped files
// the scan table does not know, held quietly and printed only if a client
// refuses the minted certificate anyway -- the handoff that tells the
// operator (or an agent) exactly which file to over-mount by hand, or that
// none exists and the client is really pinning.
func bundledCAOverlays(spec dockerRun, share, scanLabel string) ([]bundlescan.Overlay, []string, error) {
	caPEM, err := os.ReadFile(filepath.Join(share, trust.CertFileName))
	if err != nil {
		return nil, nil, fmt.Errorf("--patch-bundled-cas needs the published Veris CA: %w", err)
	}
	scanner := &bundlescan.Scanner{CacheDir: bundleCacheDir(), ContainerLabel: scanLabel}
	cands, unknown, err := scanner.Collect(context.Background(), spec.Image, spec.Volumes)
	if err != nil {
		return nil, nil, err
	}
	overlays, skipped, err := bundlescan.WriteOverlays(
		filepath.Join(share, bundlesSubdir), caPEM, cands)
	if err != nil {
		return nil, nil, err
	}
	if !spec.Quiet {
		// A found-and-already-trusted bundle is reported too, or it would be
		// indistinguishable from nothing having been found.
		for _, c := range skipped {
			fmt.Fprintf(os.Stderr,
				"veris: %s: bundled CA at %s already carries the Veris CA\n",
				c.SDK, c.ContainerPath)
		}
		for _, o := range overlays {
			fmt.Fprintf(os.Stderr,
				"veris: %s: bundled CA at %s -- over-mounted with the Veris CA appended\n",
				o.SDK, o.ContainerPath)
		}
		if len(overlays) > 0 {
			fmt.Fprintf(os.Stderr,
				"veris: %d bundled CA file(s) over-mounted\n", len(overlays))
		}
	}
	return overlays, unknown, nil
}

// bundleCacheDir is where scan results live, keyed by immutable image ID. ""
// disables the cache, which only costs the next run a re-scan.
func bundleCacheDir() string {
	root := discovery.Dir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "cache", "bundlescan")
}
