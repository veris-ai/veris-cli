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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/proxy"
	"github.com/veris-ai/veris-cli/internal/routes"
	"github.com/veris-ai/veris-cli/internal/trust"
)

// --- the fake serve -----------------------------------------------------------

// The host tier composes `veris serve --expose` as a child, and a real one
// needs cloudflared and a sandbox. This fake is the contract instead: it
// writes the env and ready files as serve does, answers the status endpoint
// with the requests it proxied and a canned inbound receipt, prints serve's
// own inbound lines on the way out, and stops on SIGTERM -- recording that it
// did, which is what every exit-path test asserts. It is a separate process,
// so it is steered by environment variables.
const (
	// fakeServeMode is "", "die-at-start" (exit before the marker) or "hang"
	// (never write it). A status read with die=1 makes a running fake exit
	// mid-run, the way a tunnel death does.
	fakeServeMode = "VERIS_TEST_FAKE_SERVE"
	// fakeServeStopped is the file written when SIGTERM arrives, holding
	// whether the drained status read was made.
	fakeServeStopped = "VERIS_TEST_FAKE_STOPPED"
	// fakeServeArgv is the file the argv and the environment serve reads --
	// the secrets, and the lifecycle variable it must not see -- are recorded
	// in, so a test can see what reached serve and how.
	fakeServeArgv = "VERIS_TEST_FAKE_ARGV"
	// fakeServeInbound is the inbound receipt's JSON; "" is an empty one and
	// "none" omits the field, as a proxy with no callback path would.
	fakeServeInbound = "VERIS_TEST_FAKE_INBOUND"
	// fakeServeArmed is written once the signal trap is installed in hang
	// mode, so a test never signals before it exists.
	fakeServeArmed = "VERIS_TEST_FAKE_ARMED"
)

const fakePublicURL = "https://odd-forest-1a2b.trycloudflare.com"

func fakeServe() int {
	args := os.Args
	if i := slices.Index(args, "serve"); i >= 0 {
		args = args[i:]
	}
	var envPath, readyPath string
	expose := 0
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--write-env":
			envPath = args[i+1]
		case "--ready-file":
			readyPath = args[i+1]
		case "--expose":
			expose, _ = strconv.Atoi(args[i+1])
		}
	}
	if p := os.Getenv(fakeServeArgv); p != "" {
		record := strings.Join(args, " ") + "\n" +
			"VERIS_API_KEY=" + os.Getenv("VERIS_API_KEY") + "\n" +
			"VERIS_TUNNEL_TOKEN=" + os.Getenv("VERIS_TUNNEL_TOKEN") + "\n" +
			"VERIS_ENVIRONMENT_ID=" + os.Getenv("VERIS_ENVIRONMENT_ID") + "\n"
		_ = os.WriteFile(p, []byte(record), 0o600)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	var drained bool
	var mu sync.Mutex
	stopped := func() {
		mu.Lock()
		defer mu.Unlock()
		if p := os.Getenv(fakeServeStopped); p != "" {
			_ = os.WriteFile(p, []byte(fmt.Sprintf("stopped drain=%v\n", drained)), 0o600)
		}
	}

	switch os.Getenv(fakeServeMode) {
	case "die-at-start":
		fmt.Fprintln(os.Stderr, "fake serve: refusing to start")
		return 1
	case "hang":
		if p := os.Getenv(fakeServeArmed); p != "" {
			_ = os.WriteFile(p, []byte("armed"), 0o600)
		}
		<-stop
		stopped()
		return 0
	}
	if envPath == "" || readyPath == "" || expose == 0 {
		fmt.Fprintln(os.Stderr, "fake serve: missing --write-env, --ready-file or --expose")
		return 2
	}

	// The proxy: a plain server counts what the command under test sends
	// through HTTP_PROXY, and the status endpoint reports it as a receipt.
	var stripe int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == proxy.StatusPath {
			if r.URL.Query().Get("drain") == "1" {
				drained = true
			}
			if r.URL.Query().Get("die") == "1" {
				fmt.Fprintln(os.Stderr, "fake serve: the tunnel exited")
				go func() {
					time.Sleep(50 * time.Millisecond)
					os.Exit(1)
				}()
			}
			state := map[string]any{
				"veris_proxy": true, "sandbox_id": "sbx_run",
				"receipt": proxy.Receipt{
					Total:     stripe,
					ByService: map[string]int64{"stripe": stripe},
					ByHost:    map[string]int64{"api.stripe.com": stripe},
				},
			}
			if raw := os.Getenv(fakeServeInbound); raw != "none" {
				var in proxy.InboundReceipt
				if raw != "" {
					_ = json.Unmarshal([]byte(raw), &in)
				}
				state["inbound_receipt"] = in
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(state)
			return
		}
		if r.Host == "api.stripe.com" {
			stripe++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The env file, in the posix shape writeEnvFile produces, with the
	// values that exercise its quoting: an apostrophe, a dollar, a quote,
	// and the two append variables.
	var env bytes.Buffer
	env.WriteString("# Written by veris serve --write-env. Source, do not edit.\n")
	for _, v := range fakeEnvVars(srv.URL) {
		if v.Append {
			fmt.Fprintf(&env, "export %s=\"${%s:+$%s }%s\"\n", v.Name, v.Name, v.Name, shellEscapeDouble(v.Value))
			continue
		}
		fmt.Fprintf(&env, "export %s=%s\n", v.Name, shellQuoteSingle(v.Value))
	}
	if err := writeFileAtomic(envPath, env.Bytes(), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake serve:", err)
		return 1
	}
	if err := writeFileAtomic(readyPath, []byte(fmt.Sprintf("proxy=%s\npid=%d\n", srv.URL, os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake serve:", err)
		return 1
	}
	<-stop
	// What serve prints at exit: its inbound receipt, which the run must
	// not show twice, and a line that must still come through.
	fmt.Fprintln(os.Stderr, "veris: your app received 99 callback(s):")
	fmt.Fprintln(os.Stderr, "  POST   /fake                        99 -> 200")
	fmt.Fprintln(os.Stderr, "fake serve: shutting down")
	stopped()
	return 0
}

// fakeEnvVars is what the fake writes: the host tier's variables plus
// VERIS_PUBLIC_URL, with values chosen to exercise the quoting.
func fakeEnvVars(proxyURL string) []trust.Var {
	return []trust.Var{
		{Name: "HTTP_PROXY", Value: proxyURL},
		{Name: "http_proxy", Value: proxyURL},
		{Name: "HTTPS_PROXY", Value: proxyURL},
		{Name: "https_proxy", Value: proxyURL},
		{Name: "NO_PROXY", Value: "localhost,127.0.0.1"},
		{Name: "VERIS_PROXY_URL", Value: proxyURL},
		{Name: "VERIS_SANDBOX_ID", Value: "sbx_run"},
		{Name: "VERIS_CANARY", Value: `it's "$5" \ ` + "`x`"},
		{Name: "VERIS_PUBLIC_URL", Value: fakePublicURL},
		{Name: "NODE_OPTIONS", Value: "--use-env-proxy", Append: true},
		{Name: "JAVA_TOOL_OPTIONS", Value: `-Dhttp.proxyHost=127.0.0.1 -Djavax.net.ssl.trustStore=/pub/veris.jks -Djavax.net.ssl.trustStorePassword=changeit "$q"`, Append: true},
	}
}

// useFakeServe points the host tier at the fake for the rest of the test and
// returns the two files it reports through: the stop marker and the argv
// record. The marker's absence after a run is the leak every test here is
// about.
func useFakeServe(t *testing.T) (stopped, argv string) {
	t.Helper()
	dir := t.TempDir()
	stopped, argv = filepath.Join(dir, "stopped"), filepath.Join(dir, "argv")
	t.Setenv(fakeServeStopped, stopped)
	t.Setenv(fakeServeArgv, argv)
	t.Setenv(fakeServeMode, "")
	t.Setenv(fakeServeInbound, "")
	t.Setenv(fakeServeArmed, "")
	t.Setenv("VERIS_TUNNEL_TOKEN", "")
	prev := serveCommand
	serveCommand = func(args []string) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=TestMain"}, args...)...)
		// Last wins: the workload child's role is in the test's environment,
		// and this child is serve.
		cmd.Env = append(os.Environ(), childMarker+"=fakeserve")
		return cmd, nil
	}
	t.Cleanup(func() { serveCommand = prev })
	return stopped, argv
}

// mustExit asserts err carries the exit code the tier promises.
func mustExit(t *testing.T, err error, want int) {
	t.Helper()
	var code exitCode
	if !errors.As(err, &code) || int(code) != want {
		t.Fatalf("err = %v, want exit %d", err, want)
	}
}

// --- the composition ----------------------------------------------------------

// The whole host-tier arrangement: serve started with the run's target and
// the handoff files, the command run with the environment serve wrote
// (through HTTP_PROXY it reaches serve, which is the receipt), both receipts
// read in one drained status read, the callback requirement judged, serve
// stopped. The secrets travel in serve's environment and never in its argv.
func TestRunHostExposeComposesServeAndJudgesBothReceipts(t *testing.T) {
	stopped, argvFile := useFakeServe(t)
	receiptPath := filepath.Join(t.TempDir(), "receipt.json")
	t.Setenv(fakeServeInbound, `{"total":2,"delivered":2,"callbacks":[{"method":"POST","path":"/hooks/stripe","status":200,"count":2}],"by_path":{"/hooks/stripe":2},"delivered_by_path":{"/hooks/stripe":2}}`)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "call")

	var err error
	stderr := captureStderr(t, func() {
		err = cmdRun(append([]string{
			"--config", cfgPath, "--expose", "3000", "--receipt", receiptPath,
			"--expose-token", "tok_secret", "--expose-hostname", "hooks.example.com",
			"--require-service", "stripe", "--require-callback", "/hooks/stripe", "--",
		}, argv...))
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	raw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Engine struct {
			Callbacks []proxy.Callback `json:"callbacks"`
		} `json:"engine"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	wantCallbacks := []proxy.Callback{{Method: "POST", Path: "/hooks/stripe", Status: 200, Count: 2}}
	if !slices.Equal(saved.Engine.Callbacks, wantCallbacks) {
		t.Fatalf("saved callbacks = %+v, want %+v", saved.Engine.Callbacks, wantCallbacks)
	}
	for _, want := range []string{
		"veris: callbacks arrive at " + fakePublicURL,
		"veris: the sandbox received 1 request(s):",
		fmt.Sprintf("  %-28s %d", "stripe", 1),
		"veris: your app received 2 callback(s):",
		fmt.Sprintf("  %-6s %-28s %d -> %d", "POST", "/hooks/stripe", 2, 200),
		"fake serve: shutting down",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	// serve's own inbound receipt is dropped; the run's is the one printed.
	if strings.Contains(stderr, "99") {
		t.Errorf("serve's inbound receipt must not print beside the run's:\n%s", stderr)
	}
	waitForFile(t, stopped)
	if b, _ := os.ReadFile(stopped); !strings.Contains(string(b), "drain=true") {
		t.Errorf("the verdict read must drain the proxy, got %q", b)
	}
	record, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	line, rest, _ := strings.Cut(string(record), "\n")
	for _, want := range []string{
		"serve --listen 127.0.0.1:0 --expose 3000 ",
		"--write-env ", "--ready-file ",
		"--log-level warn --log-format text",
		"--config " + cfgPath,
		"--expose-hostname hooks.example.com",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("serve argv lacks %q: %s", want, line)
		}
	}
	if strings.Contains(line, "tok_secret") {
		t.Errorf("the tunnel token must not be in serve's argv: %s", line)
	}
	if !strings.Contains(rest, "VERIS_TUNNEL_TOKEN=tok_secret") {
		t.Errorf("the tunnel token must reach serve through its environment, got %q", rest)
	}
}

// A callback requirement the inbound receipt refutes exits 3, as the
// container tier does, and serve is stopped on the way.
func TestRunHostExposeUnmetCallbackExits3AndStopsServe(t *testing.T) {
	stopped, _ := useFakeServe(t)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "silent")

	var err error
	stderr := captureStderr(t, func() {
		err = cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--require-callback", "/hooks/stripe", "--"}, argv...))
	})
	mustExit(t, err, exitRequirementUnmet)
	want := "veris: the run required callback /hooks/stripe at least 1 time(s) but your app received it 0 time(s)"
	if !strings.Contains(stderr, want) || !strings.Contains(stderr, "veris: your app received no callbacks from this run.") {
		t.Errorf("stderr:\n%s", stderr)
	}
	waitForFile(t, stopped)
}

// The command's own verdict keeps its exit code, and --quiet keeps the
// receipts off stderr; serve is stopped either way.
func TestRunHostExposeChildFailureKeepsItsCodeAndStopsServe(t *testing.T) {
	stopped, _ := useFakeServe(t)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "fail")

	var err error
	stderr := captureStderr(t, func() {
		err = cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--quiet", "--"}, argv...))
	})
	mustExit(t, err, 7)
	if strings.Contains(stderr, "received") {
		t.Errorf("--quiet must keep the receipts off stderr:\n%s", stderr)
	}
	waitForFile(t, stopped)
}

// serve that exits before the marker is a run that never started: its
// output is on stderr and the error says so, promptly.
func TestRunHostExposeServeDyingAtStartupFailsTheRun(t *testing.T) {
	stopped, _ := useFakeServe(t)
	t.Setenv(fakeServeMode, "die-at-start")
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "silent")

	var err error
	stderr := captureStderr(t, func() {
		err = cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--"}, argv...))
	})
	if err == nil || !strings.Contains(err.Error(), "the proxy exited during startup (exit status 1)") {
		t.Fatalf("err = %v\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "fake serve: refusing to start") {
		t.Errorf("serve's own output must reach stderr:\n%s", stderr)
	}
	if _, statErr := os.Stat(stopped); statErr == nil {
		t.Errorf("a serve that never became ready was not asked to stop, yet the marker exists")
	}
}

// serve that dies mid-run takes both receipts with it: the run says the
// receipt could not be read and why, and an otherwise green run exits 4.
func TestRunHostExposeServeDyingMidRunIsIndeterminate(t *testing.T) {
	useFakeServe(t)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "killproxy")

	var err error
	stderr := captureStderr(t, func() {
		err = cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--"}, argv...))
	})
	mustExit(t, err, exitIndeterminate)
	for _, want := range []string{
		"veris: could not read the receipt (",
		"veris: the proxy did not shut down cleanly (the proxy exited during the run (exit status 1))",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// The shell's VERIS_ENVIRONMENT_ID -- the CLI's usual environment source,
// and what the MCP flow exports -- must not reach serve, whose --environment
// defaults to it: it would refuse to start beside --sandbox, or deploy a
// sandbox of its own. The run keeps working, and serve never sees it.
func TestRunHostExposeScrubsTheEnvironmentIDFromServe(t *testing.T) {
	stopped, argvFile := useFakeServe(t)
	t.Setenv("VERIS_ENVIRONMENT_ID", "env_1")
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "silent")

	var err error
	stderr := captureStderr(t, func() {
		err = cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--"}, argv...))
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stderr)
	}
	waitForFile(t, stopped)
	record, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(record), "\nVERIS_ENVIRONMENT_ID=\n") {
		t.Errorf("serve must not inherit VERIS_ENVIRONMENT_ID, got %q", record)
	}
	if strings.Contains(string(record), "--environment") {
		t.Errorf("serve's argv must not carry the lifecycle: %q", record)
	}
}

// Ctrl-C while the command under test runs: the signal is forwarded to it,
// and once it is gone the receipts are still read from serve -- before serve
// is asked to leave -- so the interrupted run reports what happened, exits
// 130, and leaves no serve behind.
func TestRunHostExposeInterruptMidRunReadsReceiptsThenStopsServe(t *testing.T) {
	stopped, _ := useFakeServe(t)
	t.Setenv(fakeServeInbound, `{"total":1,"delivered":1,"callbacks":[{"method":"POST","path":"/hooks/stripe","status":200,"count":1}],"by_path":{"/hooks/stripe":1},"delivered_by_path":{"/hooks/stripe":1}}`)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "wait")
	running := filepath.Join(t.TempDir(), "running")
	t.Setenv("VERIS_TEST_READY_FILE", running)

	done := make(chan error, 1)
	var stderr string
	go func() {
		var err error
		stderr = captureStderr(t, func() {
			err = cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--"}, argv...))
		})
		done <- err
	}()
	waitForFile(t, running)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("an interrupt mid-run did not return")
	}
	mustExit(t, err, 130)
	for _, want := range []string{
		"veris: the sandbox received nothing from this run.",
		"veris: your app received 1 callback(s):",
		fmt.Sprintf("  %-6s %-28s %d -> %d", "POST", "/hooks/stripe", 1, 200),
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	waitForFile(t, stopped)
	if b, _ := os.ReadFile(stopped); !strings.Contains(string(b), "drain=true") {
		t.Errorf("the receipts must be read before serve is stopped, got %q", b)
	}
}

// Ctrl-C while the tunnel is still opening: serve is in its own process
// group, so the run has to stop it itself, and it does, exiting 130.
func TestRunHostExposeInterruptDuringStartupStopsServe(t *testing.T) {
	stopped, _ := useFakeServe(t)
	t.Setenv(fakeServeMode, "hang")
	armed := filepath.Join(t.TempDir(), "armed")
	t.Setenv(fakeServeArmed, armed)
	cfgPath := writeConfig(t, sandbox(t))
	argv := child(t, "silent")

	done := make(chan error, 1)
	go func() {
		done <- cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--"}, argv...))
	}()
	waitForFile(t, armed)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}
	select {
	case err := <-done:
		mustExit(t, err, 130)
	case <-time.After(30 * time.Second):
		t.Fatal("an interrupt during startup did not return")
	}
	waitForFile(t, stopped)
}

// What the host tier still refuses without --image: a sandbox lifecycle,
// which up and --fresh own; and the tunnel's own rule, before serve starts.
func TestRunHostExposeRefusesTheLifecycleFlags(t *testing.T) {
	cfgPath := writeConfig(t, sandbox(t))
	for _, tc := range []struct {
		line []string
		want string
	}{
		{[]string{"--environment", "env_1"}, "--environment needs --image: the sandbox's lifecycle is the CLI's in the host tier"},
		{[]string{"--sandbox", "sbx_1", "--environment", "env_1"}, "--environment needs --image"},
		{[]string{"--ttl-minutes", "5"}, "--ttl-minutes needs --image"},
		{[]string{"--expose", "3000", "--expose-token", "tok"}, "--expose-hostname is required alongside it"},
		{[]string{"--require-callback", "/hooks"}, "--require-callback asserts what your app received, and nothing can arrive without --expose <port>"},
	} {
		args := []string{"--config", cfgPath}
		args = append(args, tc.line...)
		err := cmdRun(append(args, "--", "true"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v\nwant %s", tc.line, err, tc.want)
		}
	}
	// The token from the shell is the same token: refused up front, not
	// after a serve started for nothing.
	t.Setenv("VERIS_TUNNEL_TOKEN", "tok")
	err := cmdRun([]string{"--config", cfgPath, "--expose", "3000", "--", "true"})
	if err == nil || !strings.Contains(err.Error(), "--expose-hostname is required alongside it") {
		t.Errorf("$VERIS_TUNNEL_TOKEN without a hostname: err = %v", err)
	}
}

// --- the pieces ---------------------------------------------------------------

// The env file round-trips through the writer's quoting: apostrophes,
// dollars, quotes, backslashes and backticks in a plain value, and the append
// form for the variables that extend what the developer set.
func TestHostTunnelParsesTheEnvFileServeWrites(t *testing.T) {
	want := fakeEnvVars("http://127.0.0.1:51822")
	var body bytes.Buffer
	body.WriteString("# Written by veris serve --write-env. Source, do not edit.\n\n")
	for _, v := range want {
		if v.Append {
			fmt.Fprintf(&body, "export %s=\"${%s:+$%s }%s\"\n", v.Name, v.Name, v.Name, shellEscapeDouble(v.Value))
			continue
		}
		fmt.Fprintf(&body, "export %s=%s\n", v.Name, shellQuoteSingle(v.Value))
	}
	got, err := parseEnvFile(body.Bytes())
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, body.String())
	}
	if !slices.Equal(got, want) {
		t.Errorf("parsed %+v\nwant   %+v", got, want)
	}
	// And it is what mergeEnv layers: the append variable extends.
	env := mergeEnv([]string{"NODE_OPTIONS=--max-old-space-size=4096", "HOME=/h"}, got)
	if !slices.Contains(env, "NODE_OPTIONS=--max-old-space-size=4096 --use-env-proxy") {
		t.Errorf("NODE_OPTIONS did not extend: %v", env)
	}
	if !slices.Contains(env, "VERIS_PUBLIC_URL="+fakePublicURL) {
		t.Errorf("VERIS_PUBLIC_URL missing: %v", env)
	}

	for _, bad := range []string{
		"HTTP_PROXY=http://x\n",
		"export HTTP_PROXY=http://x\n",
		"export =''\n",
	} {
		if _, err := parseEnvFile([]byte(bad)); err == nil {
			t.Errorf("parseEnvFile(%q) accepted", bad)
		}
	}
}

// An explicit --java-truststore replaces the store serve published, and
// nothing else in the variable.
func TestHostTunnelJavaTruststoreOverridesThePublishedOne(t *testing.T) {
	ht := &hostTunnel{env: fakeEnvVars("http://127.0.0.1:1")}
	if got := ht.childEnv(javaOptions{}); !slices.Equal(got, ht.env) {
		t.Errorf("no override must hand the file's variables over unchanged")
	}
	got := ht.childEnv(javaOptions{store: "/me/cacerts.jks", pass: "secret"})
	var java string
	for _, v := range got {
		if v.Name == "JAVA_TOOL_OPTIONS" {
			java = v.Value
		}
	}
	want := `-Dhttp.proxyHost=127.0.0.1 -Djavax.net.ssl.trustStore=/me/cacerts.jks -Djavax.net.ssl.trustStorePassword=secret "$q"`
	if java != want {
		t.Errorf("JAVA_TOOL_OPTIONS = %q\nwant %q", java, want)
	}
	// The original is untouched.
	if strings.Contains(ht.env[len(ht.env)-1].Value, "/me/") {
		t.Errorf("childEnv modified the tunnel's own variables")
	}
}

// serve's argv restates the run's target and settings, in resolveConfig's
// precedence, with the secrets in the environment instead.
func TestHostTunnelServeArgsNameTheTargetAndNeverTheKey(t *testing.T) {
	base := localRun{expose: 3000, logLevel: "warn", logFormat: "text"}
	cases := []struct {
		name string
		o    localRun
		// want is a list of argv fragments, each contiguous.
		want []string
		none []string
	}{
		{"config file", func() localRun {
			o := base
			o.sources.File = "/p/proxy.json"
			o.sources.Sandbox = "sbx_ignored"
			return o
		}(), []string{"--config /p/proxy.json"}, []string{"--sandbox"}},
		{"sandbox", func() localRun {
			o := base
			o.sources.Sandbox = "sbx_1"
			o.sources.Local = "sbx_local"
			o.sources.Refresh = true
			return o
		}(), []string{"--sandbox sbx_1 --refresh"}, []string{"sbx_local"}},
		{"the folder's pointer", func() localRun {
			o := base
			o.sources.Local = "sbx_local"
			return o
		}(), []string{"--sandbox sbx_local"}, nil},
		{"proxy settings", func() localRun {
			o := base
			o.listen = ":9999"
			o.strict = true
			o.caDir = "/ca"
			o.exposeHost = "host.docker.internal"
			o.tunnelHostname = "hooks.example.com"
			o.tunnelToken = "tok"
			o.sources.APIKey = "vk_secret"
			o.sources.Overrides = map[string][]routes.Entry{
				"stripe": {{Host: "api.stripe.com", Paths: []string{"/v1"}}},
				"github": {{Host: "api.github.com"}},
			}
			return o
		}(), []string{
			"--listen :9999",
			"--strict --ca-dir /ca --expose-host host.docker.internal --expose-hostname hooks.example.com " +
				"--route github=api.github.com --route stripe=api.stripe.com/v1",
		}, []string{"tok", "vk_secret", "--api-key", "--expose-token", "127.0.0.1:0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := serveArgs(tc.o, "/tmp/veris.env", "/tmp/ready")
			if args[0] != "serve" {
				t.Fatalf("argv = %v", args)
			}
			joined := " " + strings.Join(args, " ") + " "
			if !strings.Contains(joined, " --expose 3000 ") || !strings.Contains(joined, " --write-env /tmp/veris.env ") || !strings.Contains(joined, " --ready-file /tmp/ready ") {
				t.Errorf("argv lacks the handoff: %v", args)
			}
			for _, w := range tc.want {
				if !strings.Contains(joined, " "+w+" ") {
					t.Errorf("argv lacks %q: %v", w, args)
				}
			}
			for _, n := range tc.none {
				if strings.Contains(joined, n) {
					t.Errorf("argv must not carry %q: %v", n, args)
				}
			}
		})
	}
	// The default listen is a free loopback port.
	if args := serveArgs(base, "e", "r"); !strings.Contains(strings.Join(args, " "), "--listen 127.0.0.1:0") {
		t.Errorf("argv = %v", args)
	}
	env := serveEnv(localRun{sources: configSources{APIBase: "https://api.veris.ai", APIKey: "vk_secret"}, tunnelToken: "tok"})
	if !slices.Equal(env, []string{"VERIS_API_BASE=https://api.veris.ai", "VERIS_API_KEY=vk_secret", "VERIS_TUNNEL_TOKEN=tok"}) {
		t.Errorf("serveEnv = %v", env)
	}
	if env := serveEnv(localRun{}); len(env) != 0 {
		t.Errorf("nothing known must hand over nothing, got %v", env)
	}
}

// The stderr filter drops exactly serve's inbound receipt block -- header
// and indented lines -- whatever the write boundaries, and nothing else.
func TestHostTunnelStderrFilterDropsServesInboundReceipt(t *testing.T) {
	in := "time=1 level=WARN msg=\"callbacks registered, but the sandbox could not reach the app yet\"\n" +
		"veris: your app received 2 callback(s):\n" +
		"  POST   /hooks/stripe                2 -> 200\n" +
		"  1 never reached your app: it was not answering on the exposed port\n" +
		"time=2 level=INFO msg=\"shutting down\"\n" +
		"veris: your app received no callbacks from this run.\n" +
		"tail without newline"
	want := "time=1 level=WARN msg=\"callbacks registered, but the sandbox could not reach the app yet\"\n" +
		"time=2 level=INFO msg=\"shutting down\"\n" +
		"tail without newline"
	for _, chunk := range []int{1, 7, 64, len(in)} {
		var out bytes.Buffer
		f := &inboundFilter{w: &out}
		for i := 0; i < len(in); i += chunk {
			end := min(i+chunk, len(in))
			if _, err := f.Write([]byte(in[i:end])); err != nil {
				t.Fatal(err)
			}
		}
		f.flush()
		if out.String() != want {
			t.Errorf("chunk %d: got\n%s\nwant\n%s", chunk, out.String(), want)
		}
	}
}

// One drained read for both receipts, and a proxy that reports no inbound
// receipt is told apart from one reporting an empty one.
func TestHostTunnelReadsBothReceiptsInOneDrainedRead(t *testing.T) {
	var reads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads = append(reads, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/no-ingress") {
			_, _ = w.Write([]byte(`{"receipt":{"total":3,"by_service":{"stripe":3}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"receipt":{"total":3,"by_service":{"stripe":3}},"inbound_receipt":{"total":0}}`))
	}))
	defer srv.Close()

	receipt, inbound, err := readStatusReceipts(srv.URL + proxy.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Total != 3 || receipt.ByService["stripe"] != 3 || inbound == nil || inbound.Total != 0 {
		t.Errorf("receipt = %+v inbound = %+v", receipt, inbound)
	}
	if len(reads) != 1 || reads[0] != proxy.StatusPath+"?drain=1" {
		t.Errorf("reads = %v, want one drained read", reads)
	}
	// Absent is nil, not empty.
	if _, inbound, err := readStatusReceipts(srv.URL + "/no-ingress" + proxy.StatusPath); err != nil || inbound != nil {
		t.Errorf("inbound = %+v err = %v, want nil and no error", inbound, err)
	}
	if _, _, err := readStatusReceipts("http://127.0.0.1:1" + proxy.StatusPath); err == nil {
		t.Error("an unreachable proxy must be an error")
	}
}

func TestRunReceiptRetainsRejectedCallbackEvidence(t *testing.T) {
	useFakeServe(t)
	t.Setenv(fakeServeInbound, `{"total":1,"delivered":0,"callbacks":[{"method":"POST","path":"/hooks/stripe","status":500,"count":1}],"by_path":{"/hooks/stripe":1},"delivered_by_path":{}}`)
	cfgPath := writeConfig(t, sandbox(t))
	path := filepath.Join(t.TempDir(), "receipt.json")
	argv := child(t, "silent")
	var err error
	captureStderr(t, func() {
		err = cmdRun(append([]string{"--config", cfgPath, "--expose", "3000", "--require-callback", "/hooks/stripe", "--receipt", path, "--"}, argv...))
	})
	mustExit(t, err, exitRequirementUnmet)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		ExitCode int `json:"exit_code"`
		Engine   struct {
			Callbacks []proxy.Callback `json:"callbacks"`
		} `json:"engine"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	want := []proxy.Callback{{Method: "POST", Path: "/hooks/stripe", Status: 500, Count: 1}}
	if saved.ExitCode != exitRequirementUnmet || !slices.Equal(saved.Engine.Callbacks, want) {
		t.Fatalf("rejected callback evidence lost: %+v", saved)
	}
}
