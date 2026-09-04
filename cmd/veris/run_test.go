package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/veris-ai/veris-cli/internal/cfg"
	"github.com/veris-ai/veris-cli/internal/cli"
	"github.com/veris-ai/veris-cli/internal/discovery"
	"github.com/veris-ai/veris-cli/internal/proxy"
	"github.com/veris-ai/veris-cli/internal/trust"
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
	case "yente":
		// The client of a twin the proxy does not route: it reads the base
		// URL the run handed it and calls the twin directly, off the proxy
		// (a loopback address is never proxied) -- and calls stripe through
		// the proxy too, so both ledgers have something to compare.
		base := os.Getenv("YENTE_API_BASE")
		if base == "" {
			fmt.Fprintln(os.Stderr, "child: YENTE_API_BASE is not set")
			os.Exit(8)
		}
		for _, u := range []string{base + "/search/default?q=x", "http://api.stripe.com/v1/charges"} {
			resp, err := http.Get(u)
			if err != nil {
				fmt.Fprintln(os.Stderr, "child:", err)
				os.Exit(9)
			}
			resp.Body.Close()
		}
		os.Exit(0)
	case "fakeserve":
		// The serve the host tier composes for --expose, faked: it writes
		// the same files, answers the same endpoint and stops on the same
		// signal (hosttunnel_test.go).
		os.Exit(fakeServe())
	case "killproxy":
		// A workload that takes the proxy down mid-run and exits only once
		// it is gone, so the run's after-read finds nothing to read.
		url := os.Getenv("VERIS_PROXY_URL") + proxy.StatusPath
		if resp, err := http.Get(url + "?die=1"); err == nil {
			resp.Body.Close()
		}
		for range 400 {
			resp, err := http.Get(url)
			if err != nil {
				os.Exit(0)
			}
			resp.Body.Close()
			time.Sleep(25 * time.Millisecond)
		}
		os.Exit(9)
	case "cli":
		// The binary as a script sees it: the dispatcher on the argv handed
		// over in the environment, exiting exactly the way main does. The
		// test flags in os.Args are why the argv travels separately.
		os.Exit(exitStatus(run(strings.Fields(os.Getenv("VERIS_TEST_CLI_ARGS")))))
	case "wait":
		// Announces itself, then runs until a signal takes it: a suite the
		// developer interrupts mid-run.
		if ready := os.Getenv("VERIS_TEST_READY_FILE"); ready != "" {
			_ = os.WriteFile(ready, []byte("running"), 0o600)
		}
		time.Sleep(5 * time.Minute)
		os.Exit(0)
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

// cli runs `veris args...` as a separate process and reports what a
// script would see: the two streams apart, and the exit code.
func invoke(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return invokeIn(t, "", args...)
}

// invokeIn is invoke with the child's working directory pinned; "" inherits
// the test's, which is fine for a command that looks at no project file.
func invokeIn(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	argv := child(t, "cli")
	t.Setenv("VERIS_TEST_CLI_ARGS", strings.Join(args, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
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

// `veris run --help 2>&1` exited 1 with the usage followed by
// "veris: flag: help requested": flag.ContinueOnError reports -h the
// same way it reports a bad flag, and main treated both as a failure. Help that
// was asked for is the answer -- on stdout, exit 0 -- on every subcommand; a
// genuine flag error keeps usage on stderr and a non-zero exit.
func TestHelpIsAnAnswerNotAFailure(t *testing.T) {
	for _, argv := range [][]string{
		{"run", "--help"}, {"run", "-h"}, {"serve", "--help"}, {"check", "-h"},
	} {
		stdout, stderr, code := invoke(t, argv...)
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

	stdout, stderr, code := invoke(t, "run", "--no-such-flag")
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
	if stdout, _, code := invoke(t, "--help"); code != 0 || !strings.Contains(stdout, "Usage:") {
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
	// Anonymously pullable, which is the point of the default: whoever runs
	// this is on their own laptop or their own CI, and a registry that wants a
	// login first is a wall in front of the one command the tool exists for.
	// An Artifact Registry host is exactly that wall -- it is where this image
	// used to live, and every client had to `gcloud auth configure-docker`
	// before their first run.
	if strings.Contains(defaultProxyImage, "pkg.dev") {
		t.Errorf("default proxy image %q is in Artifact Registry, which cannot be pulled without a Google login", defaultProxyImage)
	}
	// Its own repository, never the one holding the images our cluster runs:
	// a grant that makes this image public must not make those public too.
	if !strings.Contains(defaultProxyImage, "veris-cli") {
		t.Errorf("default proxy image %q is not the proxy's own repository", defaultProxyImage)
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
// crashed client; the wire cannot tell them apart, so the wording stays
// probabilistic. The VERDICT depends on the rest of the receipt: beside an
// empty one the run proved nothing and fails (Node closes without an alert
// on exactly this path -- nango-server exited green while every vendor call
// died); beside any vendor-surface traffic it stays advisory.
func TestAbortedOnlyHandshakesFailOnlyAnEmptyRun(t *testing.T) {
	r := proxy.Receipt{
		ByHost: map[string]int64{},
		TrustFailures: []proxy.TrustFailure{{
			Host: "api.figma.com", Mapped: true, Aborted: 2,
		}},
	}
	msgs, fatal := trustFailureDiagnostics(r, trustAdvice{ContainerTier: true})
	if !fatal {
		t.Fatal("aborted handshakes beside an empty receipt must fail the run")
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %v, want exactly one diagnostic", msgs)
	}
	for _, want := range []string{
		"api.figma.com", "likely", "not certain",
		"proved nothing", "sibling container", "veris.env",
	} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("diagnostic is missing %q: %s", want, msgs[0])
		}
	}
	if strings.Contains(msgs[0], "refused the interception CA") {
		t.Errorf("an EOF must never be worded as a confirmed refusal: %s", msgs[0])
	}

	// Vendor-surface traffic flowed (another service, or this host earlier):
	// the EOF is back to advisory and the command's exit code stands.
	r.Total = 3
	if msgs, fatal := trustFailureDiagnostics(r, trustAdvice{ContainerTier: true}); fatal || len(msgs) != 1 {
		t.Fatalf("aborted beside real traffic must warn without failing: fatal=%v msgs=%v", fatal, msgs)
	}
}

// The sibling-container clause is container-tier knowledge: the host tier
// has no compose sidecars to warn about, and appending it there would send
// the reader chasing a /veris-share that does not exist.
func TestSiblingNoteIsContainerTierOnly(t *testing.T) {
	r := proxy.Receipt{
		ByHost: map[string]int64{},
		TrustFailures: []proxy.TrustFailure{{
			Host: "api.stripe.com", Mapped: true, Rejected: 1,
			Reasons: map[string]int64{"unknown_ca": 1},
		}},
	}
	msgs, _ := trustFailureDiagnostics(r, trustAdvice{ContainerTier: true})
	if len(msgs) != 1 || !strings.Contains(msgs[0], "sibling container") {
		t.Fatalf("container tier must carry the sibling note: %v", msgs)
	}
	msgs, _ = trustFailureDiagnostics(r, trustAdvice{})
	if len(msgs) != 1 || strings.Contains(msgs[0], "sibling container") {
		t.Fatalf("host tier must not carry the sibling note: %v", msgs)
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

// --- run defaults -------------------------------------------------------------

// The project file answers for what the command line left out: the command,
// the requirements, what to expose, which image, strict mode, and which
// sandbox. A flag or an environment variable always wins, and "given" is what
// counts -- a flag set to its zero value still beats the file.

// captureStderr runs f with os.Stderr redirected and returns what was
// written there: run reports on stderr directly, as the engine always has.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	os.Stderr = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// runProject writes a project whose default environment dev carries conf,
// and this folder's sandbox pointer at sb when it is not empty.
func runProject(b *bench, dev cfg.EnvConfig, sb string) {
	b.t.Helper()
	dev.ID = devID
	b.projectFile(cfg.Project{Project: "proj", Default: "dev", Environments: map[string]cfg.EnvConfig{
		"dev": dev, "ci": {ID: ciID},
	}})
	if sb != "" {
		b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sb, EnvironmentID: devID}})
	}
}

func TestRunDefaultsCommandComesFromTheProject(t *testing.T) {
	b := newBench(t)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "fail")
	runProject(b, cfg.EnvConfig{Run: cfg.RunConfig{Command: argv}}, "")

	err := cmdRun([]string{"--config", cfgPath, "--quiet"})
	var code exitCode
	if !errors.As(err, &code) || code != 7 {
		t.Fatalf("the configured command should have run and exited 7, got %v", err)
	}

	// A command on the line beats the file's.
	err = cmdRun(append([]string{"--config", cfgPath, "--quiet", "--"}, child(t, "silent")...))
	if err != nil {
		t.Fatalf("the command after -- should replace the file's: %v", err)
	}
}

func TestRunDefaultsNeedsACommandNamesTheEnvironment(t *testing.T) {
	b := newBench(t)
	cfgPath := writeConfig(t, sandbox(t))
	runProject(b, cfg.EnvConfig{}, "")
	err := cmdRun([]string{"--config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "run needs a command (none configured for 'dev')") {
		t.Fatalf("an environment without run.command should be named, got %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(b.project, ".veris", "twin.yaml")) {
		t.Errorf("the error should say which file to set run.command in, got %v", err)
	}
}

func TestRunDefaultsRequireServiceComesFromTheProject(t *testing.T) {
	b := newBench(t)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "call")
	runProject(b, cfg.EnvConfig{Proxy: cfg.ProxyConfig{RequireService: []string{"github"}}}, "")

	err := cmdRun(append([]string{"--config", cfgPath, "--quiet", "--"}, argv...))
	var code exitCode
	if !errors.As(err, &code) || code != exitRequirementUnmet {
		t.Fatalf("the file's require_service github is unmet by a call to stripe (exit 3), got %v", err)
	}

	// The flag replaces the file's list rather than adding to it.
	err = cmdRun(append([]string{"--config", cfgPath, "--quiet", "--require-service", "stripe", "--"}, argv...))
	if err != nil {
		t.Fatalf("--require-service stripe should beat the file's github: %v", err)
	}
}

func TestRunDefaultsFillOnlyWhatTheLineLeftOut(t *testing.T) {
	b := newBench(t)
	runProject(b, cfg.EnvConfig{
		Run: cfg.RunConfig{Command: []string{"pytest", "-q"}},
		Proxy: cfg.ProxyConfig{
			RequireService: []string{"stripe:2"}, RequireCallback: []string{"/hooks/stripe"},
			Expose: 3000, Image: "app:test", Strict: true,
		},
	}, "")
	s, stderr := open(t, cli.Globals{}, "", "")

	var d projectDefaults
	name, err := d.fill(s, "", map[string]bool{})
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	if name != "dev" {
		t.Errorf("name = %q, want dev", name)
	}
	if !slices.Equal(d.argv, []string{"pytest", "-q"}) {
		t.Errorf("argv = %q", d.argv)
	}
	if !slices.Equal(d.reqs, []requirement{{kind: "service", name: "stripe", count: 2}}) {
		t.Errorf("reqs = %+v", d.reqs)
	}
	if !slices.Equal(d.callbackReqs, []requirement{{kind: "callback", name: "/hooks/stripe", count: 1}}) {
		t.Errorf("callbackReqs = %+v", d.callbackReqs)
	}
	if d.expose != 3000 || d.image != "app:test" || !d.strict {
		t.Errorf("expose %d image %q strict %v", d.expose, d.image, d.strict)
	}

	// Every flag on the line keeps its value, a zero one included.
	d = projectDefaults{argv: []string{"go", "test"}, reqs: []requirement{{kind: "service", name: "github", count: 1}}}
	given := map[string]bool{"require-service": true, "require-callback": true, "expose": true, "image": true, "strict": true}
	if _, err := d.fill(s, "", given); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if !slices.Equal(d.argv, []string{"go", "test"}) || len(d.reqs) != 1 || d.reqs[0].name != "github" ||
		d.callbackReqs != nil || d.expose != 0 || d.image != "" || d.strict {
		t.Errorf("flags on the line were overridden: %+v", d)
	}

	// --env picks the config; ci configures none of this.
	s, _ = open(t, cli.Globals{}, "ci", "")
	d = projectDefaults{}
	if name, err := d.fill(s, "ci", nil); err != nil || name != "ci" || d.argv != nil || d.reqs != nil || d.expose != 0 {
		t.Errorf("--env ci: name %q err %v defaults %+v", name, err, d)
	}

	// A name the file does not know is refused as every command refuses it.
	s, stderr = open(t, cli.Globals{}, "stagin", "")
	var already printedError
	if _, err := d.fill(s, "stagin", nil); !errors.As(err, &already) {
		t.Errorf("--env stagin: err %v, want the printed refusal", err)
	}
	if !strings.Contains(stderr.String(), "✗ No environment 'stagin' in") {
		t.Errorf("stderr %q", stderr.String())
	}

	// A pasted id the file does not carry names no config and fills nothing
	// (one it does carry resolves to that entry, as --env ci would).
	const pasted = "z9y8x7w6v5u4t3s2r1q0p9o8n"
	s, _ = open(t, cli.Globals{}, pasted, "")
	d = projectDefaults{}
	if name, err := d.fill(s, pasted, nil); err != nil || name != "" || d.argv != nil {
		t.Errorf("--env <id>: name %q err %v defaults %+v", name, err, d)
	}

	// A file entry that would not parse as a flag names the file.
	runProject(b, cfg.EnvConfig{Proxy: cfg.ProxyConfig{RequireService: []string{"stripe:zero"}}}, "")
	s, _ = open(t, cli.Globals{}, "", "")
	d = projectDefaults{}
	_, err = d.fill(s, "", nil)
	for _, want := range []string{filepath.Join(b.project, ".veris", "twin.yaml"), "environments.dev.proxy.require_service", "not a positive count"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("bad file entry: err %v lacks %q", err, want)
		}
	}
}

func TestRunDefaultsPointerYieldsToEveryExplicitSource(t *testing.T) {
	b := newBench(t)
	t.Setenv(discovery.EnvConfig, "")
	runProject(b, cfg.EnvConfig{}, sbID)
	s, _ := open(t, cli.Globals{}, "", "")
	if got, err := pointerSandbox(s, configSources{}, ""); err != nil || got != sbID {
		t.Fatalf("with nothing explicit the pointer routes; got %q, %v", got, err)
	}
	cases := map[string]struct {
		src         configSources
		environment string
		env         map[string]string
	}{
		"--config":            {src: configSources{File: "proxy.json"}},
		"--sandbox":           {src: configSources{Sandbox: "sbx_flag"}},
		"--environment":       {environment: devID},
		"$VERIS_PROXY_CONFIG": {env: map[string]string{discovery.EnvConfig: "/x/proxy.json"}},
		"$VERIS_SANDBOX_ID":   {env: map[string]string{discovery.EnvSandboxID: "sbx_env"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got, err := pointerSandbox(s, tc.src, tc.environment); err != nil || got != "" {
				t.Errorf("%s should hide the pointer, got %q, %v", name, got, err)
			}
		})
	}
	// No project file, no local file, no pointer.
	if got, err := pointerSandbox(&session{res: &cfg.Resolved{}}, configSources{}, ""); err != nil || got != "" {
		t.Errorf("without a local file: %q, %v", got, err)
	}

	// The pointer belongs to dev; a run for ci must not route at it with
	// ci's settings, and the refusal names both and the way out.
	t.Run("another environment's pointer is refused", func(t *testing.T) {
		s, _ := open(t, cli.Globals{}, "ci", "")
		_, err := pointerSandbox(s, configSources{}, "")
		want := "this folder's sandbox " + sbID + " belongs to environment dev (k3j2v0d8…), not 'ci'. Run veris up ci for one of its own, or pass --sandbox"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v\nwant %s", err, want)
		}
		// A pasted id is held to the same rule; dev's own id is dev.
		s, _ = open(t, cli.Globals{}, "z9y8x7w6v5u4t3s2r1q0p9o8n", "")
		if _, err := pointerSandbox(s, configSources{}, ""); err == nil || !strings.Contains(err.Error(), "not 'z9y8x7w6v5u4t3s2r1q0p9o8n'") {
			t.Errorf("a pasted id of another environment: %v", err)
		}
		s, _ = open(t, cli.Globals{}, devID, "")
		if got, err := pointerSandbox(s, configSources{}, ""); err != nil || got != sbID {
			t.Errorf("dev by id: %q, %v", got, err)
		}
		// End to end the refusal is the run's error, before anything routes.
		var runErr error
		stderr := captureStderr(t, func() {
			runErr = cmdRun(append([]string{"--env", "ci", "--quiet", "--"}, child(t, "silent")...))
		})
		if runErr == nil || !strings.Contains(runErr.Error(), "belongs to environment dev") {
			t.Errorf("run --env ci: %v", runErr)
		}
		if strings.Contains(stderr, "using sandbox") {
			t.Errorf("a refused run must not announce a routing decision:\n%s", stderr)
		}
	})
}

func TestRunDefaultsAnnounceThePointerOnlyOnceTheLineIsAccepted(t *testing.T) {
	b := newBench(t)
	t.Setenv(discovery.EnvConfig, "")
	runProject(b, cfg.EnvConfig{}, sbID)
	var err error
	stderr := captureStderr(t, func() { err = cmdRun([]string{"--cap-add", "NET_ADMIN", "--", "true"}) })
	if err == nil || !strings.Contains(err.Error(), "--cap-add needs --image") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(stderr, "using sandbox") {
		t.Errorf("the pointer was announced on a refused line:\n%s", stderr)
	}
}

func TestRunDefaultsEnvNeedsAProjectFile(t *testing.T) {
	b := newBench(t)
	cfgPath := writeConfig(t, sandbox(t))
	var err error
	stderr := captureStderr(t, func() { err = cmdRun([]string{"--config", cfgPath, "--env", "dev", "--", "true"}) })
	var already printedError
	if !errors.As(err, &already) {
		t.Fatalf("err = %v, want the printed refusal", err)
	}
	if !strings.Contains(stderr, "✗ No .veris/twin.yaml found (searched up from "+b.project+")") || !strings.Contains(stderr, "→ Next: veris env create") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

func TestRunDefaultsRefusalsNameTheFileSetting(t *testing.T) {
	b := newBench(t)
	cfgPath := writeConfig(t, sandbox(t))
	projPath := filepath.Join(b.project, ".veris", "twin.yaml")
	cases := []struct {
		name string
		conf cfg.EnvConfig
		line []string
		want string
	}{
		{"require_callback without expose", cfg.EnvConfig{Proxy: cfg.ProxyConfig{RequireCallback: []string{"/hooks"}, Image: "app"}}, nil,
			"proxy.require_callback in " + projPath + " asserts what your app received"},
		{"require_callback without expose, host tier", cfg.EnvConfig{Proxy: cfg.ProxyConfig{RequireCallback: []string{"/hooks"}}}, nil,
			"proxy.require_callback in " + projPath + " asserts what your app received"},
		{"image against --listen", cfg.EnvConfig{Proxy: cfg.ProxyConfig{Image: "app"}}, []string{"--listen", ":0"},
			"--listen applies to a local proxy, and proxy.image in " + projPath + " puts the proxy in its own container. Drop it, or pass --image '' to run locally"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runProject(b, tc.conf, "")
			line := append([]string{"--config", cfgPath}, tc.line...)
			err := cmdRun(append(line, "--", "true"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v\nwant %s", err, tc.want)
			}
		})
	}
	// A flag typed on the line is still named as the flag.
	runProject(b, cfg.EnvConfig{}, "")
	if err := cmdRun([]string{"--config", cfgPath, "--ttl-minutes", "5", "--", "true"}); err == nil || !strings.HasPrefix(err.Error(), "--ttl-minutes needs --image") {
		t.Errorf("err = %v", err)
	}
}

func TestRunDefaultsABrokenProfilesFileYieldsToAnExplicitTarget(t *testing.T) {
	b := newBench(t)
	t.Setenv(discovery.EnvConfig, "")
	cfgPath := writeConfig(t, sandbox(t))
	if err := os.MkdirAll(filepath.Join(b.home, ".veris"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.home, ".veris", "twin.yaml"), []byte("profiles: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := child(t, "silent")

	// --config means exactly that file, as it always has.
	var err error
	stderr := captureStderr(t, func() { err = cmdRun(append([]string{"--config", cfgPath, "--quiet", "--"}, argv...)) })
	if err != nil {
		t.Fatalf("--config with a broken profiles file: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "veris: warning: ") || !strings.Contains(stderr, "twin.yaml is unreadable") {
		t.Errorf("the parse failure should be a warning, got:\n%s", stderr)
	}

	// Without an explicit target the files are what the run reads: fatal.
	err = cmdRun(append([]string{"--quiet", "--"}, argv...))
	if err == nil || !strings.Contains(err.Error(), "twin.yaml is unreadable") {
		t.Errorf("a bare run must fail on the broken file, got %v", err)
	}
	// --env reads the project file through the same session: fatal too.
	err = cmdRun(append([]string{"--config", cfgPath, "--env", "dev", "--"}, argv...))
	if err == nil || !strings.Contains(err.Error(), "twin.yaml is unreadable") {
		t.Errorf("--env with the broken file, got %v", err)
	}
}

// runPlane is a control plane for run's discovery client: it serves one
// ready sandbox under any path that names it and records the key on every
// request, so a test can tell whose key the engine sent.
type runPlane struct {
	srv  *httptest.Server
	keys chan string
}

func newRunPlane(t *testing.T, id string) *runPlane {
	t.Helper()
	p := &runPlane{keys: make(chan string, 16)}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.keys <- r.Header.Get("X-API-Key")
		if !strings.HasSuffix(r.URL.Path, "/v1/sandboxes/"+id) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "environment_id": devID, "status": "ready",
			"services": []map[string]any{{"name": "stripe", "url": "http://sandbox.test/s/" + id + "/stripe",
				"status": "ready", "routes": []map[string]any{{"host": "api.stripe.com"}}}},
		})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// sawKey is the key the last request carried, the earlier ones drained: a
// run asks the plane more than once (discovery, then the ledger's read of
// the sandbox), all with the one key it resolved.
func (p *runPlane) sawKey(t *testing.T) string {
	t.Helper()
	var last string
	seen := false
	for {
		select {
		case k := <-p.keys:
			last, seen = k, true
		default:
			if !seen {
				t.Fatal("the control plane was never asked")
			}
			return last
		}
	}
}

func TestRunDefaultsTheProfilesKeyAndPlaneServeDiscovery(t *testing.T) {
	b := newBench(t)
	t.Setenv(discovery.EnvConfig, "")
	plane := newRunPlane(t, "sbx_prof")
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
		"default": {APIBase: plane.srv.URL, APIKey: "vsk_profile_key"},
	}})
	runProject(b, cfg.EnvConfig{}, "sbx_prof")
	argv := child(t, "silent")
	line := append([]string{"--refresh", "--quiet", "--listen", "127.0.0.1:0", "--"}, argv...)

	var err error
	stderr := captureStderr(t, func() { err = cmdRun(line) })
	if err != nil {
		t.Fatalf("a run with only the profile's login: %v\n%s", err, stderr)
	}
	if got := plane.sawKey(t); got != "vsk_profile_key" {
		t.Errorf("the plane saw key %q, want the profile's", got)
	}

	// --api-key on the line beats the profile's key.
	stderr = captureStderr(t, func() { err = cmdRun(append([]string{"--api-key", "vsk_line_key"}, line...)) })
	if err != nil {
		t.Fatalf("--api-key: %v\n%s", err, stderr)
	}
	if got := plane.sawKey(t); got != "vsk_line_key" {
		t.Errorf("the plane saw key %q, want the flag's", got)
	}

	// --api-base on the line beats the profile's plane.
	other := newRunPlane(t, "sbx_prof")
	stderr = captureStderr(t, func() { err = cmdRun(append([]string{"--api-base", other.srv.URL}, line...)) })
	if err != nil {
		t.Fatalf("--api-base: %v\n%s", err, stderr)
	}
	if got := other.sawKey(t); got != "vsk_profile_key" {
		t.Errorf("the other plane saw key %q, want the profile's", got)
	}
	select {
	case k := <-plane.keys:
		t.Errorf("the profile's plane was asked (key %q) although --api-base named another", k)
	default:
	}
}

func TestRunDefaultsRoutesAtTheFoldersSandbox(t *testing.T) {
	b := newBench(t)
	t.Setenv(discovery.EnvConfig, "")
	cacheSandbox(t, "sbx_local", "stripe")
	runProject(b, cfg.EnvConfig{}, "sbx_local")
	argv := child(t, "silent")
	line := append([]string{"--quiet", "--listen", "127.0.0.1:0", "--"}, argv...)

	var err error
	stderr := captureStderr(t, func() { err = cmdRun(line) })
	if err != nil {
		t.Fatalf("a bare run with only the folder's pointer: %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "veris: using sandbox sbx_local (this folder)\n") {
		t.Errorf("the pointer should be announced on stderr, got %q", stderr)
	}

	// $VERIS_SANDBOX_ID names another sandbox and the pointer is not mentioned.
	cacheSandbox(t, "sbx_env", "stripe")
	t.Setenv(discovery.EnvSandboxID, "sbx_env")
	stderr = captureStderr(t, func() { err = cmdRun(line) })
	if err != nil {
		t.Fatalf("run under $VERIS_SANDBOX_ID: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "this folder") {
		t.Errorf("the pointer must yield to $VERIS_SANDBOX_ID, got %q", stderr)
	}
}
