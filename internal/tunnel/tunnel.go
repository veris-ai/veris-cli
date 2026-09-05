// Package tunnel supervises a cloudflared process that publishes a local port
// at a public https hostname.
//
// This is the callback direction. The proxy's other half routes the code under
// test OUT to a sandbox; a webhook has to come back IN, and a sandbox in a
// cluster cannot reach a laptop behind NAT. cloudflared dials out and holds the
// connection open, so nothing here listens on a routable address and no
// firewall is touched.
//
// Two modes, one binary:
//
//   - A QUICK tunnel needs no account and mints a random *.trycloudflare.com
//     hostname, which it announces on stderr. That is the default because
//     zero-provisioning is the product promise -- a client should not have to
//     open an account to receive a webhook.
//   - A NAMED tunnel authenticates with a token and serves a hostname the
//     operator already owns. It announces nothing, so the hostname is supplied
//     rather than parsed.
//
// The URL matters more than the process: a tunnel that is running but whose
// hostname we failed to read is useless, and reporting it as ready would be the
// same silent-success failure the redirect tier is built to refuse.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/veris-ai/veris-cli/internal/procgroup"
)

// DefaultBinary is the cloudflared executable name. The runner image ships it;
// a local `serve --expose` needs it on PATH.
const DefaultBinary = "cloudflared"

// startupTimeout bounds the wait for a quick tunnel to announce its hostname.
// Cloudflare normally answers in a couple of seconds; a minute means something
// is wrong that waiting longer will not fix.
const startupTimeout = 60 * time.Second

// gracePeriod is how long each teardown signal is given before escalating.
const gracePeriod = 5 * time.Second

// quickHostname matches the hostname cloudflared prints for a quick tunnel.
// Anchored on the scheme so a mention of the domain in surrounding prose cannot
// be mistaken for the announcement.
var quickHostname = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com`)

// Options describes the tunnel to open.
type Options struct {
	// Target is the local origin cloudflared forwards to, e.g.
	// "http://127.0.0.1:8082".
	Target string

	// Binary overrides the cloudflared executable.
	Binary string

	// RunAsUID drops the tunnel child to this uid at exec.
	//
	// It is started before the proxy drops its own privileges, because the URL
	// has to exist before the sandbox is created. Left as root it would keep
	// the container's NET_ADMIN for the whole run, so it drops itself instead
	// -- and this is the uid the redirect exempts, which its egress needs
	// anyway.
	RunAsUID int

	// Token authenticates a named tunnel. With it, Hostname is required: a
	// named tunnel serves a hostname from its own configuration and prints no
	// announcement for us to read.
	Token    string
	Hostname string

	Log *slog.Logger
}

// Tunnel is a running cloudflared with a known public URL.
type Tunnel struct {
	url string

	cmd    *exec.Cmd
	log    *slog.Logger
	exited chan struct{}
	stop   sync.Once
	mu     sync.Mutex
	err    error
}

// Start launches cloudflared and returns once its public URL is known.
//
// Blocking until the URL is known is the point: the caller registers that URL
// with the sandbox before letting a run begin, and a run that starts against an
// unregistered destination silently receives no callbacks.
func Start(ctx context.Context, opts Options) (*Tunnel, error) {
	if opts.Target == "" {
		return nil, errors.New("tunnel: no local target to publish")
	}
	if opts.Token != "" && opts.Hostname == "" {
		return nil, errors.New(
			"tunnel: a named tunnel serves a hostname it is configured with and " +
				"announces nothing, so --expose-hostname is required alongside its token")
	}
	bin := opts.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil, fmt.Errorf(
			"%s is not on PATH, so the callback tunnel cannot be opened. Install it "+
				"(brew install cloudflared / see cloudflare.com/products/tunnel), or drop "+
				"--expose to run without ingress", bin)
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	// Token tunnels receive their ingress rules from Cloudflare. A local --url
	// does not override those rules; the caller must verify the configured
	// destination reaches its recorder. Only quick tunnels use a local target.
	args := []string{"tunnel", "--no-autoupdate"}
	if opts.Token != "" {
		args = append(args, "run", "--token", opts.Token)
	} else {
		args = append(args, "--url", opts.Target)
	}

	cmd := exec.Command(bin, args...) //nolint:gosec // argv built above
	// Its own process group. cloudflared and any wrapper around it spawn
	// children that inherit the stderr pipe, and killing only the parent leaves
	// them holding it open -- so the wait below blocks until its timeout rather
	// than returning, and teardown takes five seconds every time.
	procgroup.Isolate(cmd)
	// Only while we are still root. With --environment the tunnel opens before
	// the proxy drops, so the child must drop itself or keep NET_ADMIN for the
	// whole run; on every other path the drop already happened and the child
	// inherits it -- where asking for a credential is "operation not
	// permitted", since setuid needs the privilege we just gave up.
	if opts.RunAsUID > 0 && os.Geteuid() == 0 {
		procgroup.RunAsUID(cmd, opts.RunAsUID)
	}
	// cloudflared writes its banner to stderr, including the assigned hostname.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	// /dev/null rather than io.Discard: a non-*os.File writer makes os/exec
	// create a pipe and a copying goroutine that Wait blocks on, and that pipe
	// is inherited by everything cloudflared spawns. Wait then returns only
	// once the last of them exits, which turns Stop into a five-second wait.
	if devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		defer devNull.Close()
		cmd.Stdout = devNull
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	t := &Tunnel{cmd: cmd, log: log, exited: make(chan struct{}), url: httpsOrigin(opts.Hostname)}

	found := make(chan string, 1)
	go t.readOutput(stderr, found)
	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		t.err = err
		t.mu.Unlock()
		close(t.exited)
	}()

	// A named tunnel announces nothing, so its configured hostname is the
	// answer and there is nothing to wait for.
	if opts.Token != "" {
		return t, nil
	}

	select {
	case u := <-found:
		t.url = u
		log.Info("callback tunnel open", "url", u)
		return t, nil
	case <-t.exited:
		_ = t.Stop()
		return nil, fmt.Errorf("%s exited before announcing a hostname: %w", bin, t.waitErr())
	case <-ctx.Done():
		_ = t.Stop()
		return nil, ctx.Err()
	case <-time.After(startupTimeout):
		_ = t.Stop()
		return nil, fmt.Errorf(
			"%s did not announce a hostname within %s; the tunnel may be running but "+
				"nothing can be registered without its URL", bin, startupTimeout)
	}
}

// httpsOrigin turns a bare hostname into a URL. The flag asks for a hostname,
// and a hostname is not a callback destination: the sandbox refuses anything
// without an https scheme, so `hooks.example.com` would register and fail.
func httpsOrigin(v string) string {
	if v == "" || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") {
		return v
	}
	return "https://" + v
}

// readOutput drains cloudflared's stderr for the whole run, not just until the
// hostname appears: a process whose stderr pipe fills up blocks on write, so
// abandoning the reader would wedge the tunnel some minutes into a suite.
func (t *Tunnel) readOutput(r io.Reader, found chan<- string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	announced := false
	for scanner.Scan() {
		line := scanner.Text()
		if !announced {
			if m := quickHostname.FindString(line); m != "" {
				announced = true
				select {
				case found <- m:
				default:
				}
			}
		}
		// Cloudflared is chatty at info level; only its own errors are worth a
		// line in ours.
		if strings.Contains(line, "ERR ") || strings.Contains(line, "error") {
			t.log.Debug("cloudflared", "line", line)
		}
	}
}

// URL is the public https origin callbacks should be sent to.
func (t *Tunnel) URL() string { return t.url }

// Done is closed when cloudflared exits, so a caller can notice a tunnel that
// died mid-run rather than discovering it through missing callbacks.
func (t *Tunnel) Done() <-chan struct{} { return t.exited }

func (t *Tunnel) waitErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// Stop ends the tunnel. Safe to call more than once.
func (t *Tunnel) Stop() error {
	t.stop.Do(func() {
		if t.cmd.Process == nil {
			return
		}
		// SIGTERM first so cloudflared can close its edge connection tidily.
		procgroup.Terminate(t.cmd, syscall.SIGTERM)
		select {
		case <-t.exited:
		case <-time.After(gracePeriod):
		}
		// SIGKILL the GROUP regardless of whether the leader exited. The leader
		// exiting is not the same as the tunnel being gone: launched through a
		// wrapper, a child that ignored SIGTERM keeps the public URL answering
		// after its parent has been reaped. Signalling an already-empty group
		// is a no-op, and Stop runs once, so there is no later pass to catch it.
		procgroup.Terminate(t.cmd, syscall.SIGKILL)
		select {
		case <-t.exited:
		case <-time.After(gracePeriod):
			t.log.Warn("the callback tunnel did not exit after SIGKILL",
				"pid", t.cmd.Process.Pid)
		}
	})
	return nil
}
