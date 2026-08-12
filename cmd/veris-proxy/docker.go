package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/veris-ai/veris-proxy/internal/discovery"
	"github.com/veris-ai/veris-proxy/internal/proxy"
)

// runContainerised is `run --image`: the whole two-container arrangement,
// without the two docker commands.
//
// It starts the proxy in its own container, waits for it, runs the image under
// test in a second container sharing that network namespace, streams its
// output, and tears everything down. The image under test needs no
// capabilities, no iptables, no entrypoint change and no modification -- every
// requirement sits on the proxy container, which we build.
//
// The alternative was asking people to type two docker invocations carrying a
// --network container: reference, an --env-file, a bind mount and a capability
// flag, in the right order, and to remember the teardown.
type dockerRun struct {
	Image      string
	ProxyImage string
	Sandbox    string
	APIBase    string
	APIKey     string
	Config     string
	Volumes    []string
	EnvVars    []string
	Workdir    string
	Argv       []string

	Requirements []requirement

	// The callback direction. Ingress runs in the proxy container, where
	// cloudflared forwards to the shared namespace's loopback -- which is the
	// workload's port.
	Expose         int
	ExposeHost     string
	TunnelToken    string
	TunnelHostname string
	Environment    string
	TTLMinutes     int
	CallbackReqs   []requirement

	ProxyUID  int
	Quiet     bool
	KeepProxy bool
	Strict    bool
	LogLevel  string
}

func runContainerised(spec dockerRun) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("--image needs docker on PATH")
	}

	share, err := os.MkdirTemp("", "veris-share-*")
	if err != nil {
		return err
	}
	// Not deferred: --keep-proxy leaves a container with this directory
	// mounted, so teardown below decides whether it may go. Until teardown is
	// armed, the early returns clean up for themselves.
	// The workload container runs as its own user and has to read these.
	if err := os.Chmod(share, 0o755); err != nil {
		_ = os.RemoveAll(share)
		return err
	}

	name := fmt.Sprintf("veris-proxy-%d", os.Getpid())
	workload := fmt.Sprintf("veris-workload-%d", os.Getpid())
	// A network of our own, so nothing collides with whatever else is running.
	// Always ours: the proxy reaches the sandbox over its public ingress, the
	// same way a client would, so there is no third party on this network for
	// it to need to resolve.
	network := fmt.Sprintf("veris-net-%d", os.Getpid())
	if err := dockerQuiet("network", "create", network); err != nil {
		_ = os.RemoveAll(share)
		return fmt.Errorf("create docker network: %w", err)
	}

	// Teardown runs on every path out, including a signal: a leaked proxy
	// container holds a network namespace and a name that the next run wants.
	// Once-guarded because the signal path and the deferred path both call it.
	// envSandboxID is read by the teardown closure; it is set once the proxy
	// reports ready and the deployed sandbox is knowable from here.
	var teardown sync.Once
	var envSandboxID string
	stop := func() {
		teardown.Do(func() {
			// The workload first -- it lives in the proxy's network namespace,
			// so removing the proxy underneath it is how a container survives
			// with no route to anywhere. Named for exactly this: an unnamed
			// `docker run` cannot be reached once we stop waiting on it.
			_ = dockerQuiet("rm", "-f", workload)
			if spec.KeepProxy {
				fmt.Fprintf(os.Stderr,
					"veris-proxy: leaving %s running, env file at %s (--keep-proxy)\n",
					name, filepath.Join(share, "veris.env"))
				return
			}
			// SIGTERM and wait before rm -f. serve clears the callback
			// registration and deletes a sandbox it created in deferred work,
			// and `rm -f` is SIGKILL -- so forcing here leaves the sandbox
			// pointing at a dead tunnel, and an --environment sandbox alive
			// until its TTL. Below the KeepProxy check, because keeping the
			// proxy means keeping it RUNNING.
			if spec.Expose > 0 || spec.Environment != "" {
				_ = dockerQuiet("stop", "-t", "20", name)
			}
			_ = dockerQuiet("rm", "-f", name)
			_ = dockerQuiet("network", "rm", network)
			_ = os.RemoveAll(share)
			// The container's own deferred delete is the normal path, but the
			// stop grace above can expire mid-teardown -- cloudflared drains
			// its tunnel before the delete request goes out -- and the SIGKILL
			// that follows leaks the sandbox until its TTL. The host holds the
			// same credentials, so it guarantees the delete rather than hoping;
			// a 404 here just means the container got there first.
			deleteDeployedSandbox(spec, envSandboxID)
		})
	}
	defer stop()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		// os.Exit skips deferred work, so teardown happens here explicitly.
		stop()
		os.Exit(130)
	}()

	if err := refuseExemptWorkloadUID(spec.Image, spec.ProxyUID); err != nil {
		return err
	}
	if err := ensureProxyImage(spec.ProxyImage, spec.Quiet); err != nil {
		return err
	}
	if err := startProxyContainer(spec, name, network, share); err != nil {
		return err
	}

	// --environment deploys a sandbox INSIDE the container before the marker is
	// written, and that is allowed minutes for scheduling and image pulls. A
	// ninety-second budget would kill healthy deployments as startup failures.
	readyBudget := 90 * time.Second
	if spec.Environment != "" {
		// Comfortably above the worst case a healthy deploy can hit: quick
		// tunnel up to 60s, WaitReady up to 5min, plus creation, CA setup and
		// polling. A tight budget kills a deployment that stayed within every
		// component's own timeout.
		readyBudget = 9 * time.Minute
	}
	if err := waitForReady(share, name, readyBudget); err != nil {
		return err
	}
	// Read only after readiness: a container that crashed on startup surfaces
	// as waitForReady's exited-with-these-logs error, not as an inscrutable
	// failure to read a published port from a container that no longer exists.
	statusURL, err := proxyStatusURL(name)
	if err != nil {
		return err
	}
	if spec.Environment != "" {
		envSandboxID = fetchSandboxID(statusURL)
	}
	if !spec.Quiet {
		fmt.Fprintf(os.Stderr, "veris-proxy: interception live in %s\n", name)
	}

	status, runErr := runWorkload(spec, name, workload, share)
	if runErr != nil {
		return runErr
	}

	receipt, rErr := fetchReceipt(statusURL)
	if rErr != nil {
		fmt.Fprintf(os.Stderr,
			"veris-proxy: could not read the receipt (%v), so what the sandbox "+
				"received is unknown\n", rErr)
		if status == 0 {
			return exitCode(exitIndeterminate)
		}
		return exitCode(status)
	}
	if !spec.Quiet {
		printReceipt(os.Stderr, receipt)
	}

	unmet := unmetRequirements(spec.Requirements, receipt)
	unmet = append(unmet, environmentReceiptUnmet(spec.Environment, spec.Requirements, receipt)...)
	trustMsgs, trustFatal := trustFailureDiagnostics(receipt)
	if spec.Expose > 0 {
		// The proxy is in another container, so the only way to know what the
		// app received is to read it from the status endpoint. Without this
		// the flag parsed and asserted nothing.
		inbound, ierr := fetchInboundReceipt(statusURL)
		if ierr != nil {
			fmt.Fprintf(os.Stderr,
				"veris-proxy: could not read what your app received (%v)\n", ierr)
			// A workload that already failed keeps its own status; only an
			// otherwise-successful run becomes indeterminate.
			if status != 0 {
				return exitCode(status)
			}
			return exitCode(exitIndeterminate)
		}
		if !spec.Quiet {
			printInbound(os.Stderr, inbound)
		}
		unmet = append(unmet, unmetCallbacks(spec.CallbackReqs, inbound)...)
	}
	for _, u := range unmet {
		fmt.Fprintf(os.Stderr, "veris-proxy: %s\n", u)
	}
	for _, m := range trustMsgs {
		fmt.Fprintf(os.Stderr, "veris-proxy: %s\n", m)
	}
	if status != 0 {
		return exitCode(status)
	}
	if len(unmet) > 0 || trustFatal {
		return exitCode(exitRequirementUnmet)
	}
	return nil
}

// ensureProxyImage pulls the proxy's own image when it is absent, with the
// pull's progress on stderr. Leaving the pull to `docker run -d` swallows that
// progress, and a first run then fails minutes later with whatever secondary
// error the missing image caused -- measured as "read the proxy's published
// port: exit status 1", which names neither the image nor the pull.
func ensureProxyImage(image string, quiet bool) error {
	if exec.Command("docker", "image", "inspect", image).Run() == nil {
		return nil
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "veris-proxy: pulling %s (first run)\n", image)
	}
	cmd := exec.Command("docker", "pull", image)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cannot pull the proxy image %s: %w. "+
			"Pulling needs a logged-in gcloud; if it answered 401, run "+
			"`gcloud auth configure-docker us-central1-docker.pkg.dev` once",
			image, err)
	}
	return nil
}

// fetchSandboxID reads which sandbox the proxy container is routing at, so the
// host can guarantee the delete of one it asked to be deployed. Best-effort:
// "" only costs the backstop, and the TTL still bounds the sandbox's life.
func fetchSandboxID(statusURL string) string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(statusURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	var state struct {
		SandboxID string `json:"sandbox_id"`
	}
	if json.Unmarshal(body, &state) != nil {
		return ""
	}
	return state.SandboxID
}

// deleteDeployedSandbox is the host-side half of --environment teardown.
func deleteDeployedSandbox(spec dockerRun, sandboxID string) {
	if spec.Environment == "" || sandboxID == "" {
		return
	}
	client, err := discovery.NewClient(spec.APIBase, spec.APIKey)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = client.Delete(ctx, spec.Environment, sandboxID)
	if err == nil || strings.Contains(err.Error(), "404") {
		return
	}
	fmt.Fprintf(os.Stderr,
		"veris-proxy: could not delete sandbox %s; it will expire on its TTL (%v)\n",
		sandboxID, err)
}

// refuseExemptWorkloadUID stops an image that runs as the uid the redirect
// exempts. One rule cannot tell it from the proxy, so every request would go
// to the real vendor with the rules installed and everything looking healthy.
func refuseExemptWorkloadUID(image string, uid int) error {
	inspect := func() (string, error) {
		out, err := exec.Command("docker", "image", "inspect",
			"-f", "{{.Config.User}}", image).Output()
		return strings.TrimSpace(string(out)), err
	}
	user, err := inspect()
	if err != nil {
		// Not pulled yet. Pull now rather than skip the check: `docker run`
		// would pull it a moment later anyway, and skipping is how an image
		// declaring USER 14741 reached the real vendor with everything
		// reporting healthy.
		if pullErr := dockerQuiet("pull", "-q", image); pullErr != nil {
			return nil // docker run will report this properly
		}
		if user, err = inspect(); err != nil {
			return nil
		}
	}
	if user == "" {
		return nil
	}
	if name, _, _ := strings.Cut(user, ":"); name == fmt.Sprint(uid) {
		return fmt.Errorf(
			"%s runs as uid %d, which the kernel redirect exempts, so its "+
				"requests would reach the real vendor with everything appearing "+
				"healthy. Rebuild it under a different USER, or pass "+
				"--proxy-uid to move the exemption", image, uid)
	}
	// A named USER resolving to the exempt uid would still slip through: the
	// label carries the name, and resolving it means running the image. The
	// numeric form is what images in practice declare.
	return nil
}

func startProxyContainer(spec dockerRun, name, network, share string) error {
	args := []string{
		"run", "-d", "--name", name, "--network", network,
		"--cap-add=NET_ADMIN",
		// Published on loopback only, and on a port docker picks, so the
		// receipt can be read afterwards without claiming a fixed one.
		"-p", "127.0.0.1:0:8080",
		"-v", share + ":/veris-share",
		"-e", "VERIS_SHARE_DIR=/veris-share",
	}
	switch {
	case spec.Sandbox != "":
		args = append(args, "-e", "VERIS_SANDBOX_ID="+spec.Sandbox)
	case spec.Config != "":
		abs, err := filepath.Abs(spec.Config)
		if err != nil {
			return err
		}
		args = append(args, "-v", abs+":/veris-share/config.json:ro",
			"-e", "VERIS_CONFIG=/veris-share/config.json")
	}
	// Ingress runs in the proxy container: cloudflared forwards to the shared
	// namespace's loopback, which is the workload's port.
	if spec.Expose > 0 {
		args = append(args, "-e", "VERIS_EXPOSE="+fmt.Sprint(spec.Expose))
		if spec.ExposeHost != "" {
			args = append(args, "-e", "VERIS_EXPOSE_HOST="+spec.ExposeHost)
		}
		if spec.TunnelToken != "" {
			args = append(args, "-e", "VERIS_TUNNEL_TOKEN="+spec.TunnelToken)
		}
		if spec.TunnelHostname != "" {
			args = append(args, "-e", "VERIS_TUNNEL_HOSTNAME="+spec.TunnelHostname)
		}
	}
	if spec.Environment != "" {
		args = append(args, "-e", "VERIS_ENVIRONMENT_ID="+spec.Environment)
	}
	if spec.TTLMinutes > 0 {
		args = append(args, "-e", "VERIS_TTL_MINUTES="+fmt.Sprint(spec.TTLMinutes))
	}
	if spec.ProxyUID != defaultProxyUID {
		args = append(args, "-e", "VERIS_PROXY_UID="+fmt.Sprint(spec.ProxyUID))
	}
	if spec.Strict {
		args = append(args, "-e", "VERIS_STRICT=1")
	}
	for _, pair := range []struct{ key, val string }{
		{"VERIS_API_KEY", spec.APIKey}, {"VERIS_API_BASE", spec.APIBase},
		{"VERIS_LOG_LEVEL", spec.LogLevel},
	} {
		if pair.val != "" {
			args = append(args, "-e", pair.key+"="+pair.val)
		}
	}
	// No command after the image: that is what selects the mode where this
	// container only intercepts and the workload lives in its own.
	args = append(args, spec.ProxyImage)

	if err := dockerQuiet(args...); err != nil {
		return fmt.Errorf("start the proxy container (is %s present? try docker pull): %w",
			spec.ProxyImage, err)
	}
	return nil
}

// waitForReady blocks on the marker the proxy writes once every listener is
// bound, checking the container is still alive so a crash surfaces as its own
// logs rather than a timeout.
func waitForReady(share, name string, budget time.Duration) error {
	marker := filepath.Join(share, "ready")
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(marker); err == nil && info.Size() > 0 {
			return nil
		}
		if !containerRunning(name) {
			logs, _ := exec.Command("docker", "logs", "--tail", "20", name).CombinedOutput()
			return fmt.Errorf("the proxy container exited during startup:\n%s", logs)
		}
		time.Sleep(150 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", "--tail", "20", name).CombinedOutput()
	return fmt.Errorf("the proxy container never became ready:\n%s", logs)
}

func runWorkload(spec dockerRun, proxyName, name, share string) (int, error) {
	args := []string{
		"run", "--rm", "--name", name,
		// The load-bearing flag: one network namespace, so the proxy's
		// iptables rules apply to this container's sockets too.
		"--network", "container:" + proxyName,
		"--cap-drop=ALL",
		"--env-file", filepath.Join(share, "veris.env"),
		"-v", share + ":/veris-share",
	}
	if isTerminal(os.Stdin) {
		args = append(args, "-i")
	}
	for _, v := range spec.Volumes {
		args = append(args, "-v", v)
	}
	for _, e := range spec.EnvVars {
		args = append(args, "-e", e)
	}
	if spec.Workdir != "" {
		args = append(args, "-w", spec.Workdir)
	}
	args = append(args, spec.Image)
	// Nothing appended when the caller named no command, so the image's own
	// ENTRYPOINT and CMD run untouched.
	args = append(args, spec.Argv...)

	cmd := exec.Command("docker", args...) //nolint:gosec // argv built above
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 0, fmt.Errorf("run %s: %w", spec.Image, err)
	}
	return 0, nil
}

// proxyStatusURL reads back the loopback port docker chose for the proxy's
// control endpoint.
func proxyStatusURL(name string) (string, error) {
	out, err := exec.Command("docker", "port", name, "8080/tcp").Output()
	if err != nil {
		return "", fmt.Errorf("read the proxy's published port: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return "", errors.New("the proxy container published no port")
	}
	return "http://" + line + proxy.StatusPath, nil
}

func fetchReceipt(statusURL string) (proxy.Receipt, error) {
	var out struct {
		Receipt proxy.Receipt `json:"receipt"`
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(statusURL)
	if err != nil {
		return out.Receipt, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out.Receipt, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out.Receipt, fmt.Errorf("status is not readable: %w", err)
	}
	return out.Receipt, nil
}

func containerRunning(name string) bool {
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func dockerQuiet(args ...string) error {
	cmd := exec.Command("docker", args...) //nolint:gosec // argv built by callers above
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker %s: %w: %s",
			strings.Join(redactSecrets(args), " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// redactSecrets blanks the value of any -e VAR=secret before an argv reaches an
// error message. The API key is passed to the proxy container this way, and a
// missing image or a stopped daemon would otherwise print it into a CI log.
func redactSecrets(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		name, _, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		switch {
		case strings.Contains(strings.ToUpper(name), "KEY"),
			strings.Contains(strings.ToUpper(name), "TOKEN"),
			strings.Contains(strings.ToUpper(name), "SECRET"):
			out[i] = name + "=<redacted>"
		}
	}
	return out
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// fetchInboundReceipt reads what the app received, from a proxy in another
// container. The egress twin of fetchReceipt, and needed for the same reason:
// --require-callback cannot be enforced from here without it.
func fetchInboundReceipt(statusURL string) (proxy.InboundReceipt, error) {
	// A POINTER, so an image that predates --expose -- and omits the field
	// entirely -- is told apart from one reporting an empty receipt. Silently
	// reading the first as the second lets `run --expose` exit 0 with no tunnel
	// ever opened.
	var out struct {
		Inbound *proxy.InboundReceipt `json:"inbound_receipt"`
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(statusURL)
	if err != nil {
		return proxy.InboundReceipt{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return proxy.InboundReceipt{}, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return proxy.InboundReceipt{}, fmt.Errorf("status is not readable: %w", err)
	}
	if out.Inbound == nil {
		return proxy.InboundReceipt{}, fmt.Errorf(
			"this proxy image reports no inbound receipt, so it predates --expose; " +
				"rebuild or re-pull the runner image (--proxy-image)")
	}
	return *out.Inbound, nil
}
