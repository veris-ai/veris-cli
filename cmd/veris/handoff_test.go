package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/trust"
)

// The entrypoint's contract with `serve`: an environment file it can source and
// a marker that appears only once everything it vouches for is complete. Both
// exist because a POSIX-sh entrypoint in an arbitrary image cannot poll a port
// -- debian-slim and python-slim carry no curl, wget or nc, and sh has no
// /dev/tcp.

func startedProxy(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	cfg := writeConfig(t, sandbox(t))
	return cfg, func() { _ = os.RemoveAll(dir) }
}

func TestServeWritesASourceableEnvironmentAndAMarker(t *testing.T) {
	isolateHome(t)
	cfg, cleanup := startedProxy(t)
	defer cleanup()

	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	readyFile := filepath.Join(dir, "ready")

	// serve blocks, so run it until the marker appears and then stop it.
	done := make(chan error, 1)
	go func() {
		done <- cmdServe([]string{
			"--config", cfg, "--listen", "127.0.0.1:0",
			"--write-env", envFile, "--ready-file", readyFile,
			"--log-level", "error",
		})
	}()
	t.Cleanup(func() { stopServe(t, done) })

	waitForFile(t, readyFile)

	// The marker is written last, so by the time it exists the environment is
	// complete. Reading them in this order is the whole contract.
	body, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	text := string(body)

	for _, want := range []string{
		"export HTTPS_PROXY=", "export SSL_CERT_FILE=",
		"export VERIS_SANDBOX_ID=", "export VERIS_CANARY=",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("env file is missing %q:\n%s", want, text)
		}
	}
	// An appended variable must extend whatever the image already set rather
	// than replace it. JAVA_TOOL_OPTIONS is the one always emitted; NODE_OPTIONS
	// depends on the node in play and is covered in the trust package.
	if !strings.Contains(text, `export JAVA_TOOL_OPTIONS="${JAVA_TOOL_OPTIONS:+$JAVA_TOOL_OPTIONS }`) {
		t.Errorf("JAVA_TOOL_OPTIONS does not preserve an existing value:\n%s", text)
	}

	// SSL_CERT_FILE and its family REPLACE a runtime's roots, so they must name
	// the bundle. The bare certificate there trusts intercepted hosts and
	// rejects every passthrough one.
	if !strings.Contains(text, "/"+trust.BundleFileName+"'") ||
		!strings.Contains(text, "export SSL_CERT_FILE=") {
		t.Errorf("SSL_CERT_FILE does not point at the public-roots bundle:\n%s", text)
	}
	if !strings.Contains(text, "export NODE_EXTRA_CA_CERTS='") ||
		!strings.Contains(text, "/"+trust.CertFileName+"'") {
		t.Errorf("NODE_EXTRA_CA_CERTS should be the bare certificate, since Node adds it:\n%s", text)
	}

	// The bound port, not the requested one. --listen :0 names no port, so a
	// second command computing this from config could not get it right.
	if strings.Contains(text, "127.0.0.1:0'") {
		t.Errorf("env file records the requested port, not the bound one:\n%s", text)
	}

	for _, path := range []string{envFile, readyFile} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s is mode %o, want 600", filepath.Base(path), info.Mode().Perm())
		}
	}
}

// Without a canary, `check` cannot tell the proxy for this run from one left
// running by an earlier one -- so a canary always exists, whether or not the
// config named one.
func TestACanaryExistsEvenWhenTheConfigOmitsOne(t *testing.T) {
	isolateHome(t)
	cfg, cleanup := startedProxy(t)
	defer cleanup()

	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	readyFile := filepath.Join(dir, "ready")

	done := make(chan error, 1)
	go func() {
		done <- cmdServe([]string{
			"--config", cfg, "--listen", "127.0.0.1:0",
			"--write-env", envFile, "--ready-file", readyFile,
			"--log-level", "error",
		})
	}()
	t.Cleanup(func() { stopServe(t, done) })

	waitForFile(t, readyFile)
	body, _ := os.ReadFile(envFile)
	if !strings.Contains(string(body), "export VERIS_CANARY='cnry_") {
		t.Fatalf("no minted canary in the environment:\n%s", body)
	}
}

// check is an assertion. Given nothing to assert against it must say so rather
// than quietly becoming a liveness probe.
func TestCheckRefusesToDegradeIntoALivenessProbe(t *testing.T) {
	t.Setenv("VERIS_CANARY", "")
	err := cmdCheck([]string{"--proxy", "http://127.0.0.1:1"})
	if err == nil {
		t.Fatal("expected an error with no canary")
	}
	if !strings.Contains(err.Error(), "--any-run") {
		t.Errorf("the error should name the explicit opt-out: %v", err)
	}

	if err := cmdCheck([]string{"--proxy", "http://x", "--expect-canary", "a", "--any-run"}); err == nil {
		t.Error("--any-run with an expected canary is contradictory and should be refused")
	}
}

// waitForFile blocks until the marker exists, the way the entrypoint's poll
// loop does.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	for range 200 {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// stopServe signals the serve goroutine and waits for it to return.
func stopServe(t *testing.T, done chan error) {
	t.Helper()
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("serve did not shut down")
	}
}
