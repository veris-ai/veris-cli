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

// The nft ruleset is the iptables chain translated, and a translation can
// drift: a dropped RETURN turns in-pod traffic into vendor calls, a reordered
// exemption turns the proxy's own upstream calls into a loop.
func TestNftRulesetMirrorsTheIptablesChain(t *testing.T) {
	rs := nftRuleset(14741, 8081, 8443)

	// Rules apply in order, so the uid exemption must precede the redirects --
	// after them it exempts nothing and the proxy redirects itself.
	uid := strings.Index(rs, "meta skuid 14741 return")
	redir := strings.Index(rs, "redirect")
	if uid == -1 || redir == -1 || uid > redir {
		t.Fatalf("uid exemption missing or after the redirects:\n%s", rs)
	}

	for _, want := range []string{
		"ip daddr 127.0.0.0/8 return",
		"ip daddr 10.0.0.0/8 return",
		"ip daddr 172.16.0.0/12 return",
		"ip daddr 192.168.0.0/16 return",
		"tcp dport 80 redirect to :8081",
		"tcp dport 443 redirect to :8443",
		"tcp dport 8443 redirect to :8443",
		"type nat hook output priority -100",
	} {
		if !strings.Contains(rs, want) {
			t.Fatalf("ruleset lacks %q:\n%s", want, rs)
		}
	}

	// Restart safety: declare-then-delete makes the script replace any earlier
	// table in one transaction instead of stacking rules behind it.
	if !strings.HasPrefix(rs, "table ip veris {}\ndelete table ip veris\n") {
		t.Fatalf("ruleset does not replace an existing table:\n%s", rs)
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
