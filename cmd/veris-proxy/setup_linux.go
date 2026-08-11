//go:build linux

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// selfSetup makes `serve --transparent` self-sufficient inside a container:
// it installs the kernel redirect, puts the CA where the system trust store
// looks, and drops to an unprivileged uid before serving a byte.
//
// Without it the transparent listeners bind and nothing ever routes to them,
// which looks exactly like working. The container entrypoint does the same
// steps in shell; doing them here means an image can just run the binary.
//
// Needs root, and a uid dropped by `docker --user` cannot stand in: Linux
// clears the capability set on a uid change, so a non-root process has no
// CAP_NET_ADMIN and cannot write a rule no matter what the container was
// granted. Started unprivileged, this can only refuse -- unless the caller
// says something else installed the redirect, which is exactly what the
// container entrypoint does when it drops the proxy before installing.
func selfSetup(log *slog.Logger, opts setupOptions) error {
	if err := validateUID(opts.UID); err != nil {
		return err
	}

	if opts.RedirectExternal {
		// Only the REDIRECT is somebody else's. Ownership and trust are still
		// ours, and skipping them left the proxy dropped to a uid that could
		// not write its own handoff files.
		if os.Geteuid() == 0 {
			if err := prepare(log, opts); err != nil {
				return err
			}
			if err := dropTo(opts.UID); err != nil {
				return err
			}
			log.Info("dropped privileges", "uid", opts.UID)
		}
		log.Info("assuming the kernel redirect is installed by something else",
			"exempt_uid", os.Geteuid())
		return nil
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf(
			"--transparent needs root to install the kernel redirect, and this "+
				"process is uid %d. Linux clears capabilities on a uid change, so "+
				"--cap-add=NET_ADMIN does not help a container started with "+
				"--user.\n\n"+
				"     Run this container as root (the proxy drops to uid %d itself "+
				"once the rules are in), or pass --redirect-external if something "+
				"else already installed them.\n\n"+
				"     Serving anyway would bind the transparent listeners and "+
				"intercept nothing, which is indistinguishable from working",
			os.Geteuid(), opts.UID)
	}

	httpPort, err := portOf(opts.TransparentHTTP)
	if err != nil {
		return err
	}
	httpsPort, err := portOf(opts.TransparentHTTPS)
	if err != nil {
		return err
	}

	if err := prepare(log, opts); err != nil {
		return err
	}

	backend, err := installRedirect(opts.UID, httpPort, httpsPort)
	if err != nil {
		return err
	}
	log.Info("kernel redirect installed", "via", backend,
		"http", httpPort, "https", httpsPort, "exempt_uid", opts.UID)

	// Last, and irreversible. Everything above needed root; nothing below
	// does, and the redirect exempts this uid by number.
	if err := dropTo(opts.UID); err != nil {
		return err
	}
	log.Info("dropped privileges", "uid", opts.UID)
	return nil
}

// prepare is everything that needs root but is not the redirect: make the
// directories the dropped proxy still writes to, and put the CA where the
// system trust store looks.
func prepare(log *slog.Logger, opts setupOptions) error {
	for _, dir := range opts.Writable {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := chownShallow(dir, opts.UID); err != nil {
			return err
		}
	}
	if opts.CAPath == "" {
		return nil
	}
	if err := installSystemTrust(opts.CAPath); err != nil {
		log.Warn("could not install the CA into the system trust store; "+
			"a workload sharing this filesystem will not trust it", "err", err)
		return nil
	}
	log.Info("CA installed into the system trust store")
	return nil
}

// validateUID refuses a uid that would leave the proxy where it started. Zero
// passes every syscall below and logs "dropped privileges" while still root.
func validateUID(uid int) error {
	if uid <= 0 {
		return fmt.Errorf(
			"--proxy-uid %d would leave the proxy running as root, and the "+
				"redirect exemption would then cover every process in the "+
				"container. Choose an unused non-zero uid", uid)
	}
	return nil
}

// installRedirect writes the redirect with iptables, and when that cannot work
// falls back to an equivalent native nftables ruleset. The fallback is not an
// exotic case: the iptables chain needs the owner match (xt_owner) to exempt
// the proxy's own uid, and minimal kernels -- Firecracker guests such as E2B
// sandboxes, modules-less container kernels -- ship nf_tables without xt_owner
// and without /lib/modules to load it from. nft needs no xt_owner: `meta
// skuid` is core nf_tables. The same fallback covers nft-only distros that
// stopped shipping iptables entirely.
//
// Returns which backend took the rules, for the "kernel redirect installed"
// log line -- two backends that both look like success must say which one is
// live, or a missing-interception hunt starts from the wrong tool.
func installRedirect(uid, httpPort, httpsPort int) (string, error) {
	iptErr := installRedirectIptables(uid, httpPort, httpsPort)
	if iptErr == nil {
		return "iptables", nil
	}
	// A failed install may have left a half-built VERIS chain (created, some
	// rules in, jump absent). Remove it before the fallback: two backends'
	// rules layered over each other is exactly the state nobody can debug.
	removeIptablesRedirect()
	nftErr := installRedirectNft(uid, httpPort, httpsPort)
	if nftErr == nil {
		return "nftables", nil
	}
	return "", fmt.Errorf(
		"the kernel redirect could not be installed, so nothing would be "+
			"intercepted.\n\n"+
			"     iptables: %v\n"+
			"     nftables: %v\n\n"+
			"     The iptables path needs the owner match (xt_owner) in the "+
			"kernel; the nftables path needs only nf_tables and the nft tool "+
			"(apt-get install nftables / apk add nftables). Add one of them, "+
			"or drop --transparent and use the explicit proxy",
		iptErr, nftErr)
}

// installRedirectIptables writes the same chain the container entrypoint used
// to write.
func installRedirectIptables(uid, httpPort, httpsPort int) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return errors.New("iptables is not in this image")
	}

	// A fresh chain either way, so a restart does not append duplicates.
	if err := execTool("iptables", "-t", "nat", "-N", "VERIS"); err != nil {
		if err := execTool("iptables", "-t", "nat", "-F", "VERIS"); err != nil {
			return fmt.Errorf("prepare the VERIS chain: %w", err)
		}
	}

	rules := [][]string{
		// First and non-optional: without it the proxy's own upstream calls
		// are redirected back into the proxy, which is a loop rather than a
		// degraded mode.
		{"-m", "owner", "--uid-owner", fmt.Sprint(uid), "-j", "RETURN"},
		// Loopback and the private ranges: the workload's own database,
		// sidecars and in-pod traffic are not vendor calls.
		{"-d", "127.0.0.0/8", "-j", "RETURN"},
		{"-d", "10.0.0.0/8", "-j", "RETURN"},
		{"-d", "172.16.0.0/12", "-j", "RETURN"},
		{"-d", "192.168.0.0/16", "-j", "RETURN"},
		{"-p", "tcp", "--dport", "80", "-j", "REDIRECT", "--to-port", fmt.Sprint(httpPort)},
		{"-p", "tcp", "--dport", "443", "-j", "REDIRECT", "--to-port", fmt.Sprint(httpsPort)},
	}
	for _, rule := range rules {
		args := append([]string{"-t", "nat", "-A", "VERIS"}, rule...)
		if err := execTool("iptables", args...); err != nil {
			return fmt.Errorf("iptables %v: %w", rule, err)
		}
	}

	// A jump left by an earlier start may sit ANYWHERE, including behind a
	// RETURN that means our chain is never reached -- and `iptables -C` is true
	// for it wherever it is. So delete any copy first, then insert at the head.
	for execTool("iptables", "-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-j", "VERIS") == nil {
	}
	return execTool("iptables", "-t", "nat", "-I", "OUTPUT", "1", "-p", "tcp", "-j", "VERIS")
}

// removeIptablesRedirect undoes whatever installRedirectIptables managed to
// write. Best-effort by design: every step is "make it not exist", and on the
// kernels that sent us here most of it never existed.
func removeIptablesRedirect() {
	if _, err := exec.LookPath("iptables"); err != nil {
		return
	}
	for execTool("iptables", "-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-j", "VERIS") == nil {
	}
	_ = execTool("iptables", "-t", "nat", "-F", "VERIS")
	_ = execTool("iptables", "-t", "nat", "-X", "VERIS")
}

// installRedirectNft applies the nftables mirror of the iptables chain in one
// atomic `nft -f` transaction: partial application is the failure mode this
// file keeps designing against, and nft gives all-or-nothing for free.
func installRedirectNft(uid, httpPort, httpsPort int) error {
	if _, err := exec.LookPath("nft"); err != nil {
		return errors.New("nft is not in this image")
	}
	return execToolStdin(nftRuleset(uid, httpPort, httpsPort), "nft", "-f", "-")
}

// nftRuleset mirrors the iptables chain rule for rule. The leading empty
// declaration makes the delete valid when no table exists yet, so a restart
// replaces the table instead of stacking duplicate rules -- the same "fresh
// chain either way" the iptables path gets from -N-then-F.
//
// Priority -100 is where iptables' own nat OUTPUT hook sits (NF_IP_PRI_NAT_DST),
// so the two backends see traffic at the same point relative to everything
// else on the box.
func nftRuleset(uid, httpPort, httpsPort int) string {
	return fmt.Sprintf(`table ip veris {}
delete table ip veris
table ip veris {
	chain output {
		type nat hook output priority -100; policy accept;
		meta skuid %d return
		ip daddr 127.0.0.0/8 return
		ip daddr 10.0.0.0/8 return
		ip daddr 172.16.0.0/12 return
		ip daddr 192.168.0.0/16 return
		tcp dport 80 redirect to :%d
		tcp dport 443 redirect to :%d
	}
}
`, uid, httpPort, httpsPort)
}

func installSystemTrust(caPath string) error {
	body, err := os.ReadFile(caPath)
	if err != nil {
		return err
	}
	for _, target := range []struct{ dir, name, tool string }{
		{"/usr/local/share/ca-certificates", "veris-ca.crt", "update-ca-certificates"},
		{"/etc/pki/ca-trust/source/anchors", "veris-ca.pem", "update-ca-trust"},
	} {
		if _, err := exec.LookPath(target.tool); err != nil {
			continue
		}
		if err := os.MkdirAll(target.dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(target.dir, target.name), body, 0o644); err != nil {
			return err
		}
		return execTool(target.tool)
	}
	return fmt.Errorf("no system trust tool found")
}

// dropTo lowers this process to an unprivileged uid. Groups first: once the
// uid goes, so does the privilege to change any of them.
func dropTo(uid int) error {
	// Supplementary groups are NOT cleared by setuid. A container started with
	// --group-add would otherwise leave the dropped proxy holding access to
	// group-owned sockets and mounted secrets it has no use for.
	if err := syscall.Setgroups([]int{uid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(uid); err != nil {
		return fmt.Errorf("setgid %d: %w", uid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid %d: %w", uid, err)
	}
	if os.Geteuid() != uid {
		return fmt.Errorf("still running as uid %d after dropping to %d", os.Geteuid(), uid)
	}
	return nil
}

// systemDirs must never be handed to the proxy's uid. A directory derived from
// a user-supplied path can land on any of them -- `--write-env /veris.env`
// makes the parent "/" -- and taking ownership of one as root is not a
// degraded mode, it is damage.
var systemDirs = map[string]bool{
	"/": true, "/etc": true, "/usr": true, "/var": true, "/bin": true,
	"/sbin": true, "/lib": true, "/opt": true, "/root": true, "/home": true,
	"/tmp": true, "/dev": true, "/proc": true, "/sys": true, "/run": true,
}

// chownShallow makes one directory usable by the dropped uid.
//
// The directory itself must succeed -- without it the proxy cannot create the
// files it is about to write. Its immediate entries are best-effort, which
// covers the CA files root just minted; a read-only bind-mounted config or a
// file belonging to someone else is not ours to take.
//
// Deliberately NOT recursive. It used to be, over a directory derived from a
// user-supplied path, which made `--write-env /veris.env` a recursive chown of
// the entire filesystem.
//
// Lchown, not Chown: this runs as root, and Chown follows a final symlink, so
// a "veris-ca.pem -> /etc/shadow" in there would hand that file to the uid.
func chownShallow(dir string, uid int) error {
	clean := filepath.Clean(dir)
	if systemDirs[clean] {
		return fmt.Errorf(
			"refusing to change ownership of %s: point --ca-dir, --write-env and "+
				"--ready-file at a directory of their own", clean)
	}
	if err := os.Lchown(clean, uid, uid); err != nil {
		return err
	}
	entries, err := os.ReadDir(clean)
	if err != nil {
		return nil //nolint:nilerr // unreadable children are not ours to fix
	}
	for _, e := range entries {
		_ = os.Lchown(filepath.Join(clean, e.Name()), uid, uid)
	}
	return nil
}

func execTool(name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // fixed argv built above
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}

func execToolStdin(stdin, name string, args ...string) error {
	cmd := exec.Command(name, args...) //nolint:gosec // fixed argv built above
	cmd.Stdin = strings.NewReader(stdin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}

func portOf(addr string) (int, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	n, err := net.LookupPort("tcp", port)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%q has no usable port; the redirect needs a fixed one", addr)
	}
	return n, nil
}
