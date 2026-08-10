package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/proxy"
	"github.com/veris-ai/veris-proxy/internal/trust"
)

// The child is this test binary re-invoked, which keeps the end-to-end test
// free of any dependency on curl, a network, or an installed toolchain.
const childMarker = "VERIS_TEST_CHILD"

func TestMain(m *testing.M) {
	switch os.Getenv(childMarker) {
	case "":
		os.Exit(m.Run())
	case "call":
		// Go's http.Transport reads HTTP_PROXY from the environment, which is
		// exactly the mechanism `run` relies on.
		resp, err := http.Get("http://api.stripe.com/v1/charges")
		if err != nil {
			fmt.Fprintln(os.Stderr, "child:", err)
			os.Exit(9)
		}
		resp.Body.Close()
		os.Exit(0)
	case "silent":
		os.Exit(0)
	case "fail":
		os.Exit(7)
	case "stubborn":
		// Traps SIGTERM and keeps running, which is what a shell script with a
		// cleanup handler or a JVM with a shutdown hook can look like.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		time.Sleep(5 * time.Minute)
		os.Exit(0)
	default:
		os.Exit(99)
	}
}

func writeConfig(t *testing.T, upstream string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := map[string]any{
		"version":    1,
		"listen":     "127.0.0.1:0",
		"sandbox_id": "sbx_run",
		"mode":       "passthrough",
		"ca_dir":     filepath.Join(dir, "ca"),
		"upstream":   map[string]any{"base_url": upstream},
		"services": []map[string]any{
			{"name": "stripe", "hosts": []string{"api.stripe.com"}},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "proxy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func sandbox(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// child returns the argv that re-invokes this binary in the given child role.
func child(t *testing.T, role string) []string {
	t.Helper()
	t.Setenv(childMarker, role)
	return []string{os.Args[0], "-test.run=TestMain"}
}

func TestRunInterceptsTheChildsRequests(t *testing.T) {
	cfg := writeConfig(t, sandbox(t))
	argv := child(t, "call")

	err := cmdRun(append([]string{"--config", cfg, "--quiet", "--require-service", "stripe", "--"}, argv...))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

// The whole point of the receipt: a command that succeeds without ever calling
// its dependency is not a passing test of that dependency.
func TestRunFailsWhenTheChildNeverCalledTheService(t *testing.T) {
	cfg := writeConfig(t, sandbox(t))
	argv := child(t, "silent")

	err := cmdRun(append([]string{"--config", cfg, "--quiet", "--require-service", "stripe", "--"}, argv...))
	var code exitCode
	if !errors.As(err, &code) || int(code) != exitRequirementUnmet {
		t.Fatalf("err = %v, want exit %d", err, exitRequirementUnmet)
	}
}

// The command's own verdict outranks ours: a failing suite keeps its exit code
// so the surrounding harness reads the same status it always did.
func TestRunPropagatesTheChildsExitCode(t *testing.T) {
	cfg := writeConfig(t, sandbox(t))
	argv := child(t, "fail")

	err := cmdRun(append([]string{"--config", cfg, "--quiet", "--"}, argv...))
	var code exitCode
	if !errors.As(err, &code) || int(code) != 7 {
		t.Fatalf("err = %v, want exit 7", err)
	}
}

// A child that traps and ignores SIGTERM must not be able to hold the proxy
// open forever: that hangs a CI job rather than failing it. This pins the
// grace-then-kill path, which has now been silently dropped twice by an edit
// that looked applied and was not.
func TestASignalledChildThatIgnoresItIsEventuallyKilled(t *testing.T) {
	if childGrace > 30*time.Second {
		t.Fatalf("childGrace is %s, too long to be a backstop", childGrace)
	}

	cfg := writeConfig(t, sandbox(t))
	argv := child(t, "stubborn")

	done := make(chan error, 1)
	go func() {
		done <- cmdRun(append([]string{"--config", cfg, "--quiet", "--"}, argv...))
	}()

	// Let the child install its trap, then ask it to stop.
	time.Sleep(500 * time.Millisecond)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}

	select {
	case <-done:
		// Killed, one way or another. The assertion is that it returned at all.
	case <-time.After(childGrace + 20*time.Second):
		t.Fatal("a child ignoring SIGTERM held the run open past the grace period")
	}
}

func TestRunNeedsACommand(t *testing.T) {
	cfg := writeConfig(t, sandbox(t))
	if err := cmdRun([]string{"--config", cfg}); err == nil {
		t.Fatal("run with no command should be a usage error")
	}
}

func TestARequirementCountIsParsed(t *testing.T) {
	got, err := parseRequirement("service", "stripe:3")
	if err != nil {
		t.Fatal(err)
	}
	if got.name != "stripe" || got.count != 3 {
		t.Errorf("parsed %+v, want stripe:3", got)
	}
	if _, err := parseRequirement("service", "stripe:0"); err == nil {
		t.Error("a zero count should be rejected: it would assert nothing")
	}
	if _, err := parseRequirement("service", "stripe:many"); err == nil {
		t.Error("a non-numeric count should be rejected")
	}
}

func TestARequirementCountIsEnforced(t *testing.T) {
	r := proxy.Receipt{ByService: map[string]int64{"stripe": 2}, ByHost: map[string]int64{}}
	if got := unmetRequirements([]requirement{{kind: "service", name: "stripe", count: 2}}, r); len(got) != 0 {
		t.Errorf("met requirement reported as unmet: %v", got)
	}
	if got := unmetRequirements([]requirement{{kind: "service", name: "stripe", count: 3}}, r); len(got) != 1 {
		t.Errorf("unmet requirement not reported: %v", got)
	}
}

// NODE_OPTIONS is the case that matters: replacing it would silently drop
// whatever the developer or their CI had already put there.
func TestMergeEnvExtendsAppendVariablesAndReplacesOthers(t *testing.T) {
	base := []string{"NODE_OPTIONS=--max-old-space-size=4096", "HTTP_PROXY=http://stale:1", "PATH=/bin"}
	got := mergeEnv(base, []trust.Var{
		{Name: "NODE_OPTIONS", Value: "--use-env-proxy", Append: true},
		{Name: "HTTP_PROXY", Value: "http://127.0.0.1:9"},
		{Name: "SSL_CERT_FILE", Value: "/ca.pem"},
	})

	want := []string{
		"NODE_OPTIONS=--max-old-space-size=4096 --use-env-proxy",
		"HTTP_PROXY=http://127.0.0.1:9",
		"PATH=/bin",
		"SSL_CERT_FILE=/ca.pem",
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	// A duplicate entry would leave the resolution up to the OS, which is
	// unspecified.
	for _, name := range []string{"NODE_OPTIONS", "HTTP_PROXY"} {
		n := 0
		for _, e := range got {
			if strings.HasPrefix(e, name+"=") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s appears %d times, want 1", name, n)
		}
	}
}

var _ = exec.Command // keep the child-invocation idiom obvious to readers

// The proxy runner has a repository of its own, holding this image and nothing
// else. Pointing the default back at the cluster's image repo would mean
// widening access on the repository that holds sandbox-api and gateway in order
// to let a client pull a proxy.
func TestTheProxyImageDefaultsToItsOwnRepository(t *testing.T) {
	if !strings.Contains(defaultProxyImage, "-proxy-") {
		t.Errorf("default proxy image %q is not in the proxy repository", defaultProxyImage)
	}
	if strings.Contains(defaultProxyImage, "-images-") {
		t.Errorf("default proxy image %q sits in the cluster's image repo", defaultProxyImage)
	}
	if !strings.HasSuffix(defaultProxyImage, ":runner") {
		t.Errorf("default proxy image %q does not name the moving runner tag", defaultProxyImage)
	}
}

// An image built to run a server or an agent carries its own ENTRYPOINT and
// CMD, and that is what its author tested. Demanding a command here would mean
// restating them -- and `docker inspect` is how you would have to find them.
func TestAnImageMayRunWithNoCommandOfItsOwn(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, sandbox(t))

	// No argv, no --image: there is genuinely nothing to exec, and the error
	// has to name both ways forward.
	err := cmdRun([]string{"--config", cfg})
	if err == nil {
		t.Fatal("a local run with no command should be refused")
	}
	for _, want := range []string{"needs a command", "--image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}

	// With --image, an empty argv is the ordinary case and must get PAST the
	// argument check. Proved by tripping the next check in line -- the
	// incompatible-flag one -- which sits immediately after it and before
	// anything reaches docker. Driving the real container path from a unit test
	// would need a daemon and a registry pull, and would install a process-wide
	// signal handler that another test's SIGTERM then fires.
	err = cmdRun([]string{
		"--config", cfg, "--image", "veris-nonexistent:probe", "--listen", ":0",
	})
	if err == nil {
		t.Fatal("--listen with --image should be refused")
	}
	if strings.Contains(err.Error(), "needs a command") {
		t.Errorf("--image with no command was refused as a missing command: %v", err)
	}
	if !strings.Contains(err.Error(), "--listen") {
		t.Errorf("expected the incompatible-flag refusal, got: %v", err)
	}
}

func TestAnEnvironmentRunThatSentNothingFailsByDefault(t *testing.T) {
	// The environment already names the services, so nobody should have to
	// spell out --require-service for "the suite reached the sandbox at all".
	if got := environmentReceiptUnmet("env_1", nil, proxy.Receipt{}); len(got) != 1 {
		t.Fatalf("empty receipt on an --environment run must be unmet, got %v", got)
	}
	// Explicit requirements take over the judgement entirely.
	reqs := []requirement{{kind: "service", name: "stripe", count: 1}}
	if got := environmentReceiptUnmet("env_1", reqs, proxy.Receipt{}); got != nil {
		t.Fatalf("explicit requirements must own the verdict, got %v", got)
	}
	// Traffic flowed: nothing to add.
	if got := environmentReceiptUnmet("env_1", nil, proxy.Receipt{Total: 2}); got != nil {
		t.Fatalf("a non-empty receipt is not unmet, got %v", got)
	}
	// Attaching to an existing sandbox keeps the documented contract: the
	// receipt is printed, the exit code stays the command's own.
	if got := environmentReceiptUnmet("", nil, proxy.Receipt{}); got != nil {
		t.Fatalf("--sandbox runs must not gain an implicit assertion, got %v", got)
	}
}
