package bundlescan

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteOverlaysAppendsTheCAAfterTheOriginal(t *testing.T) {
	veris := testCA(t, "Veris Local CA")
	// No trailing newline, to prove the guard: PEM blocks glued together
	// without one make the parser skip the second block.
	original := bytes.TrimRight(testCA(t, "Original Root"), "\n")
	dir := t.TempDir()

	overlays, skipped, err := WriteOverlays(dir, veris, []Candidate{{
		SDK:           "certifi",
		ContainerPath: "/usr/lib/python3.12/site-packages/certifi/cacert.pem",
		Content:       original,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(overlays) != 1 || len(skipped) != 0 {
		t.Fatalf("got %d overlays and %d skipped, want 1 and 0", len(overlays), len(skipped))
	}
	o := overlays[0]
	if o.ContainerPath != "/usr/lib/python3.12/site-packages/certifi/cacert.pem" {
		t.Errorf("container path %q changed", o.ContainerPath)
	}
	if filepath.Dir(o.HostPath) != dir || !strings.Contains(filepath.Base(o.HostPath), "certifi") {
		t.Errorf("host path %q should sit in %s and name its SDK", o.HostPath, dir)
	}
	patched, err := os.ReadFile(o.HostPath)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte{}, original...), '\n'), veris...)
	if !bytes.Equal(patched, want) {
		t.Error("the patched copy must be the original, a newline guard, then the Veris CA")
	}
	// Every root the SDK trusted stays trusted, plus exactly one more.
	if n := strings.Count(string(patched), "BEGIN CERTIFICATE"); n != 2 {
		t.Errorf("the patched copy holds %d certificates, want the original plus ours", n)
	}
}

func TestWriteOverlaysSkipsABundleAlreadyCarryingTheCA(t *testing.T) {
	veris := testCA(t, "Veris Local CA")
	already := append(append([]byte{}, testCA(t, "Other Root")...), veris...)

	overlays, skipped, err := WriteOverlays(t.TempDir(), veris, []Candidate{{
		SDK:           "stripe",
		ContainerPath: "/app/vendor/stripe/data/ca-certificates.crt",
		Content:       already,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(overlays) != 0 {
		t.Fatalf("a bundle already trusting the CA needs no overlay, got %+v", overlays)
	}
	// Reported back rather than silently dropped: the caller's log has to
	// tell "found and already trusted" apart from "nothing found".
	if len(skipped) != 1 || skipped[0].ContainerPath != "/app/vendor/stripe/data/ca-certificates.crt" {
		t.Fatalf("the already-trusted candidate must come back as skipped, got %+v", skipped)
	}
}
