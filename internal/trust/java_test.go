package trust

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/veris-ai/veris-proxy/internal/ca"
)

// These tests shell out to a real keytool, because the point of the feature
// is interoperating with the JDK's actual formats. No JDK, no test.
func requireKeytool(t *testing.T) {
	t.Helper()
	if _, err := findKeytool(""); err != nil {
		t.Skip("no keytool available:", err)
	}
}

func TestBuildAndInjectJavaTrustStore(t *testing.T) {
	requireKeytool(t)

	dir := t.TempDir()
	authority, err := ca.Load(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, DefaultJavaTrustStoreName)
	cacerts, err := BuildJavaTrustStore("", authority.CertPath(), out, "changeit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cacerts); err != nil {
		t.Errorf("reported cacerts source does not exist: %v", err)
	}
	assertHasVerisAlias(t, out)

	// Rebuilding must succeed from scratch, not fail on the existing alias.
	if _, err := BuildJavaTrustStore("", authority.CertPath(), out, "changeit"); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// Injecting into an app-managed keystore is idempotent: the second run
	// replaces the entry rather than erroring or duplicating it.
	if err := InjectCA("", authority.CertPath(), out, "changeit"); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if err := InjectCA("", authority.CertPath(), out, "changeit"); err != nil {
		t.Fatalf("second inject: %v", err)
	}
	assertHasVerisAlias(t, out)
}

func TestInjectMissingKeystoreFails(t *testing.T) {
	requireKeytool(t)

	dir := t.TempDir()
	authority, err := ca.Load(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if err := InjectCA("", authority.CertPath(), filepath.Join(dir, "absent.p12"), "changeit"); err == nil {
		t.Fatal("injecting into a nonexistent keystore should fail, not create one")
	}
}

func assertHasVerisAlias(t *testing.T, keystore string) {
	t.Helper()
	keytool, err := findKeytool("")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(keytool, "-list", "-alias", "veris-ca",
		"-keystore", keystore, "-storepass", "changeit").CombinedOutput()
	if err != nil {
		t.Fatalf("veris-ca alias not present in %s: %s", keystore, out)
	}
}
