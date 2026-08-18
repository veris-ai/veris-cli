package main

import (
	"bytes"
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
	case "cli":
		// The binary as a script sees it: the dispatcher on the argv handed
		// over in the environment, exiting exactly the way main does. The
		// test flags in os.Args are why the argv travels separately.
		os.Exit(exitStatus(run(strings.Fields(os.Getenv("VERIS_TEST_CLI_ARGS")))))
	case "stubborn":
		// Traps SIGTERM and keeps running, which is what a shell script with a
		// cleanup handler or a JVM with a shutdown hook can look like.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		// Announce that the trap is armed. The parent test must not signal
		// before this exists: supervise installs ITS handler before starting
		// this child, so the marker also proves the test binary can survive
		// the SIGTERM it is about to send itself.
		if ready := os.Getenv("VERIS_TEST_READY_FILE"); ready != "" {
			_ = os.WriteFile(ready, []byte("armed"), 0o600)
		}
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

// cli runs `veris-proxy args...` as a separate process and reports what a
// script would see: the two streams apart, and the exit code.
func cli(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	argv := child(t, "cli")
	t.Setenv("VERIS_TEST_CLI_ARGS", strings.Join(args, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatalf("run %v: %v", args, err)
	}
	return out.String(), errOut.String(), code
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
	ready := filepath.Join(t.TempDir(), "stubborn-armed")
	t.Setenv("VERIS_TEST_READY_FILE", ready)

	done := make(chan error, 1)
	go func() {
		done <- cmdRun(append([]string{"--config", cfg, "--quiet", "--"}, argv...))
	}()

	// Signal only once the child says its trap is armed. A fixed sleep raced
	// cmdRun's own startup (fresh-CA keygen takes seconds on a loaded runner):
	// a SIGTERM arriving before supervise installs its handler takes the
	// default action and kills the whole test binary -- the intermittent
	// "signal: terminated" CI failure.
	waitForFile(t, ready)
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

// `veris-proxy run --help 2>&1` exited 1 with the usage followed by
// "veris-proxy: flag: help requested": flag.ContinueOnError reports -h the
// same way it reports a bad flag, and main treated both as a failure. Help that
// was asked for is the answer -- on stdout, exit 0 -- on every subcommand; a
// genuine flag error keeps usage on stderr and a non-zero exit.
func TestHelpIsAnAnswerNotAFailure(t *testing.T) {
	for _, argv := range [][]string{
		{"run", "--help"}, {"run", "-h"}, {"serve", "--help"}, {"check", "-h"},
	} {
		stdout, stderr, code := cli(t, argv...)
		if code != 0 {
			t.Errorf("%v exited %d, want 0 (stderr: %q)", argv, code, stderr)
		}
		if !strings.Contains(stdout, "Usage of "+argv[0]+":") {
			t.Errorf("%v: usage should be on stdout, got %q", argv, stdout)
		}
		if stderr != "" {
			t.Errorf("%v: nothing belongs on stderr for help that was asked for, got %q", argv, stderr)
		}
	}

	stdout, stderr, code := cli(t, "run", "--no-such-flag")
	if code != 1 {
		t.Errorf("a bad flag exited %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("a bad flag must put nothing on stdout, got %q", stdout)
	}
	for _, want := range []string{"flag provided but not defined: -no-such-flag", "Usage of run:"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("a bad flag should report %q on stderr, got %q", want, stderr)
		}
	}
	// The top-level help was already right and stays so.
	if stdout, _, code := cli(t, "--help"); code != 0 || !strings.Contains(stdout, "Usage:") {
		t.Errorf("top-level --help: exit %d, stdout %q", code, stdout)
	}
}

// --cap-add is a container property, so it needs --image; and it hands back
// named capabilities, never the isolation itself. Both refusals happen before
// anything reaches docker.
func TestCapAddNeedsAnImageAndRefusesTheIsolationDefeaters(t *testing.T) {
	isolateHome(t)
	cfg := writeConfig(t, sandbox(t))

	err := cmdRun([]string{"--config", cfg, "--cap-add", "SETUID", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "--cap-add needs --image") {
		t.Errorf("--cap-add without --image should be refused as image-only, got: %v", err)
	}
	for _, c := range []string{"ALL", "CAP_SYS_ADMIN", "setuid"} {
		err := cmdRun([]string{"--config", cfg, "--image", "veris-nonexistent:probe", "--cap-add", c})
		if err == nil || !strings.Contains(err.Error(), "-cap-add") {
			t.Errorf("--cap-add %s should be refused at parse time, got: %v", c, err)
		}
	}
	// A legitimate one gets past the flag and the image-only check, proved
	// the way TestAnImageMayRunWithNoCommandOfItsOwn proves it: the next
	// refusal in line is the incompatible --listen, not anything about caps.
	err = cmdRun([]string{
		"--config", cfg, "--image", "veris-nonexistent:probe",
		"--cap-add", "SETUID", "--cap-add", "SETGID", "--listen", ":0",
	})
	if err == nil || !strings.Contains(err.Error(), "--listen") {
		t.Errorf("SETUID/SETGID should be accepted; expected the --listen refusal, got: %v", err)
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

// A harness seeding its world reads /veris/* on the same service, and those
// reads once counted toward --require-service -- which is how a run whose
// every SDK call failed TLS still reported "stripe 4" and passed. They are
// excluded now, and the unmet message names them so the excluded traffic does
// not read as the traffic having vanished.
func TestControlPlaneReadsDoNotSatisfyARequirement(t *testing.T) {
	r := proxy.Receipt{
		ByService:        map[string]int64{},
		ByHost:           map[string]int64{},
		ByServiceControl: map[string]int64{"stripe": 4},
		ControlTotal:     4,
	}
	got := unmetRequirements([]requirement{{kind: "service", name: "stripe", count: 1}}, r)
	if len(got) != 1 {
		t.Fatalf("control-plane-only traffic satisfied the requirement: %v", got)
	}
	for _, want := range []string{"0 time(s)", "4 /veris/* control-plane request(s)"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("unmet message is missing %q: %s", want, got[0])
		}
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

// The SDK-bundled-CA failure mode: a mapped host whose minted certificate the
// client refused, with nothing completed, was never actually tested -- the
// suite may still exit 0, so the trust verdict must own the exit code the way
// an unmet requirement does.
func TestATrustRejectedHostWithNoTrafficFailsTheRun(t *testing.T) {
	rejected := proxy.Receipt{
		ByHost: map[string]int64{},
		TrustFailures: []proxy.TrustFailure{{
			Host: "api.stripe.com", Mapped: true, Rejected: 3,
			Reasons: map[string]int64{"unknown_ca": 3},
		}},
	}
	msgs, fatal := trustFailureDiagnostics(rejected, trustAdvice{ContainerTier: true})
	if !fatal {
		t.Fatal("a mapped host with rejections and no completed requests must fail the run")
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %v, want exactly one diagnostic", msgs)
	}
	for _, want := range []string{
		"api.stripe.com", "3 TLS handshake(s) rejected", "unknown_ca",
		"0 requests completed", "refused the interception CA",
	} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("diagnostic is missing %q: %s", want, msgs[0])
		}
	}

	// The same rejections beside completed requests: the command's verdict
	// stands, but the refusal must still PRINT -- the completing client can be
	// the harness while the refusing one is the code under test, and silence
	// here once let a fully TLS-broken SDK pass with everything looking
	// healthy.
	rejected.ByHost = map[string]int64{"api.stripe.com": 5}
	msgs, fatal = trustFailureDiagnostics(rejected, trustAdvice{ContainerTier: true})
	if fatal {
		t.Fatal("a host with completed requests must not change the exit code")
	}
	if len(msgs) != 1 {
		t.Fatalf("mixed traffic must still warn: msgs=%v", msgs)
	}
	for _, want := range []string{
		"api.stripe.com", "3 TLS handshake(s) rejected", "5 request(s) completed",
		"--patch-bundled-cas", "never reached the sandbox",
	} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("mixed-traffic warning is missing %q: %s", want, msgs[0])
		}
	}

	// Trust failures are keyed by lowercased SNI, but the explicit-proxy tier
	// records receipt hosts as the client wrote them -- a mixed-case Host on
	// the completed requests must still suppress the fatal verdict.
	rejected.ByHost = map[string]int64{"API.Stripe.Com": 5}
	if msgs, fatal := trustFailureDiagnostics(rejected, trustAdvice{ContainerTier: true}); fatal || len(msgs) != 1 {
		t.Fatalf("a mixed-case completed host must warn without failing: fatal=%v msgs=%v", fatal, msgs)
	}
}

// Rejections on an unmapped host are background noise -- telemetry, an
// unrelated pinned client -- not missing sandbox traffic.
func TestAnUnmappedHostsRejectionsNeverFailTheRun(t *testing.T) {
	r := proxy.Receipt{
		ByHost: map[string]int64{},
		TrustFailures: []proxy.TrustFailure{{
			Host: "telemetry.example.com", Rejected: 4,
			Reasons: map[string]int64{"unknown_ca": 4},
		}},
	}
	if msgs, fatal := trustFailureDiagnostics(r, trustAdvice{ContainerTier: true}); fatal || len(msgs) != 0 {
		t.Fatalf("unmapped rejections changed the verdict: fatal=%v msgs=%v", fatal, msgs)
	}
}

// An EOF after the leaf was selected is consistent with a refusal and with a
// crashed client; the wire cannot tell them apart. The wording stays
// probabilistic and the exit code stays the command's own.
func TestAbortedOnlyHandshakesWarnWithoutFailing(t *testing.T) {
	r := proxy.Receipt{
		ByHost: map[string]int64{},
		TrustFailures: []proxy.TrustFailure{{
			Host: "api.stripe.com", Mapped: true, Aborted: 2,
		}},
	}
	msgs, fatal := trustFailureDiagnostics(r, trustAdvice{ContainerTier: true})
	if fatal {
		t.Fatal("aborted-only handshakes must not change the exit code")
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %v, want exactly one diagnostic", msgs)
	}
	for _, want := range []string{"api.stripe.com", "likely", "not certain"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("diagnostic is missing %q: %s", want, msgs[0])
		}
	}
	if strings.Contains(msgs[0], "refused the interception CA") {
		t.Errorf("an EOF must never be worded as a confirmed refusal: %s", msgs[0])
	}
}

// Every refusal diagnostic must end in an action or a stop, because its
// reader is often an agent deciding what to run next: flag off names the
// flag, flag on with candidates names the exact file to over-mount, flag on
// with none says pinning and stop -- the difference between one deterministic
// retry and a blind search.
func TestRefusalAdviceNamesTheNextAction(t *testing.T) {
	rejected := proxy.Receipt{
		ByHost: map[string]int64{},
		TrustFailures: []proxy.TrustFailure{{
			Host: "api.stripe.com", Mapped: true, Rejected: 2,
			Reasons: map[string]int64{"unknown_ca": 2},
		}},
	}
	for _, tc := range []struct {
		name   string
		advice trustAdvice
		want   []string
	}{
		{"flag off", trustAdvice{ContainerTier: true},
			[]string{"re-run with --patch-bundled-cas"}},
		{"host tier", trustAdvice{},
			[]string{"run --image", "--patch-bundled-cas"}},
		{"flag on, unknown candidate", trustAdvice{ContainerTier: true, PatchEnabled: true,
			UnknownBundles: []string{"/opt/trust/cacert.pem"}},
			[]string{"/opt/trust/cacert.pem", "bind it over that exact path"}},
		{"flag on, nothing left", trustAdvice{ContainerTier: true, PatchEnabled: true},
			[]string{"real certificate pinning", "Stop and report"}},
	} {
		msgs, fatal := trustFailureDiagnostics(rejected, tc.advice)
		if !fatal || len(msgs) != 1 {
			t.Fatalf("%s: fatal=%v msgs=%v, want one fatal diagnostic", tc.name, fatal, msgs)
		}
		for _, want := range tc.want {
			if !strings.Contains(msgs[0], want) {
				t.Errorf("%s: diagnostic is missing %q: %s", tc.name, want, msgs[0])
			}
		}
	}
}

func TestDominantReasonPrefersTheMostFrequentAlert(t *testing.T) {
	got := dominantReason(map[string]int64{"bad_certificate": 1, "unknown_ca": 5})
	if got != "unknown_ca" {
		t.Errorf("dominantReason = %q, want unknown_ca", got)
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
	// Control-plane reads alone are the harness talking to the sandbox, not
	// the suite calling its dependencies -- still unmet, and the message says
	// what DID arrive.
	got := environmentReceiptUnmet("env_1", nil, proxy.Receipt{ControlTotal: 4})
	if len(got) != 1 {
		t.Fatalf("a control-plane-only receipt must be unmet, got %v", got)
	}
	if !strings.Contains(got[0], "4 /veris/* control-plane request(s)") {
		t.Errorf("the unmet message should name the control traffic: %s", got[0])
	}
	// Attaching to an existing sandbox keeps the documented contract: the
	// receipt is printed, the exit code stays the command's own.
	if got := environmentReceiptUnmet("", nil, proxy.Receipt{}); got != nil {
		t.Fatalf("--sandbox runs must not gain an implicit assertion, got %v", got)
	}
}
