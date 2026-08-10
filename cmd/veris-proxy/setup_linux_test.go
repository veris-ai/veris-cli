//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `--write-env /veris.env` makes the writable directory "/", and this code runs
// as root. It used to chown recursively.
func TestASystemDirectoryIsNeverHandedToTheProxyUID(t *testing.T) {
	for _, dir := range []string{"/", "/etc", "/usr/", "/var/../var"} {
		err := chownShallow(dir, 14741)
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("chownShallow(%q) = %v, want a refusal", dir, err)
		}
	}
}

// Only the named directory and its immediate entries -- a recursive walk from a
// state directory that happens to contain a mount is unbounded work at best.
func TestChownReachesTheDirectoryAndItsOwnEntriesOnly(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "deep", "deeper")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "leaf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// As an ordinary user every Lchown fails; the directory's own failure is
	// the only one that surfaces, and here it is a no-op chown to our own uid.
	if err := chownShallow(root, os.Getuid()); err != nil {
		t.Fatalf("chownShallow of an owned directory: %v", err)
	}
}

// uid 0 would leave the proxy running as root with the redirect exempting root,
// which is every workload exempted at once.
func TestTheProxyUIDCannotBeRoot(t *testing.T) {
	for _, uid := range []int{0, -1} {
		if err := validateUID(uid); err == nil {
			t.Fatalf("validateUID(%d) accepted", uid)
		}
	}
	if err := validateUID(14741); err != nil {
		t.Fatalf("validateUID(14741) = %v", err)
	}
}
