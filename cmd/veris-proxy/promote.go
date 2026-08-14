package main

// Promoting the world a run built.
//
// Promotion is the platform's setup-once primitive: the sandbox's current
// state becomes the environment's default, so every later sandbox starts from
// the accounts, connections and fixtures that were built once instead of
// rebuilding them. It had a control-plane call and no way to reach it from
// here, which meant the only actor who ever holds a good world -- the run that
// just built one -- had to step outside the run to keep it. Eleven measured
// agent runs promoted nothing.
//
// So the verb belongs to the same command that owns the rest of the sandbox
// lifecycle. Two shapes, because runs come in two:
//
//	run --environment ... --promote-on-success   one-shot: the suite passed,
//	                                             the sandbox is about to be
//	                                             deleted, keep its world first
//	promote --sandbox <id>                       long session: the world grew
//	                                             into something worth keeping
//	                                             and the run is still up
//
// Both are the last thing done with a sandbox. The capture is a boundary, not
// a snapshot: it stops answering vendor requests and is left frozen and
// scrubbed, so promoting mid-suite destroys the world the suite is using.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/veris-ai/veris-proxy/internal/discovery"
	"github.com/veris-ai/veris-proxy/internal/proxy"
)

func cmdPromote(args []string) error {
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	sandbox := fs.String("sandbox", os.Getenv(discovery.EnvSandboxID),
		"sandbox `id` whose world becomes the environment's default "+
			"(the id the run logs as sandbox_id)")
	environment := fs.String("environment", "",
		"environment `id` the sandbox belongs to; read from the sandbox when omitted")
	apiBase := fs.String("api-base", "", "control plane base URL (defaults to $"+discovery.EnvAPIBase+")")
	apiKey := fs.String("api-key", "", "control plane API key (defaults to $"+discovery.EnvAPIKey+")")
	clock := fs.String("clock-restore", "rebase",
		"how a restored sandbox treats the captured instant: rebase (time starts "+
			"there and runs) or frozen (exact replay, delivery paused)")
	keepExternal := fs.Bool("keep-external-destinations", false,
		"promote callback destinations that were not this run's own receiver; "+
			"off by default, since baking someone else's URL in points every "+
			"future sandbox at them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *sandbox == "" {
		return errors.New(
			"promote needs a sandbox: --sandbox <id>, the id the run logged as " +
				"sandbox_id (or $" + discovery.EnvSandboxID + ")")
	}
	if *clock != "rebase" && *clock != "frozen" {
		return fmt.Errorf("--clock-restore %q is not rebase or frozen", *clock)
	}

	client, err := discovery.NewClient(*apiBase, *apiKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), promoteTimeout)
	defer cancel()

	envID := *environment
	if envID == "" {
		// The sandbox knows which environment it belongs to, so asking the
		// operator for it again is a second chance to get it wrong.
		sb, err := client.Fetch(ctx, *sandbox)
		if err != nil {
			return err
		}
		if sb.EnvironmentID == "" {
			return fmt.Errorf(
				"sandbox %s does not name its environment; pass --environment", *sandbox)
		}
		envID = sb.EnvironmentID
	}

	result, err := client.Promote(ctx, envID, *sandbox, discovery.PromoteOptions{
		ClockRestore:             *clock,
		KeepExternalDestinations: *keepExternal,
	})
	if err != nil {
		return err
	}
	printPromotion(os.Stderr, result)
	return nil
}

// promoteTimeout bounds the capture. It exports and pushes an image layer, so
// it is tens of seconds rather than the client's default 30.
const promoteTimeout = 10 * time.Minute

// promoteAfterRun is the --promote-on-success half: keep the world this run
// built, before teardown deletes the sandbox that holds it.
//
// Called only when the run earned it -- the command exited 0, every
// requirement held, and the sandbox actually received traffic. A promote off a
// failed run pins a world nobody validated as the starting point for every run
// after it, which is worse than promoting nothing.
func promoteAfterRun(spec dockerRun, sandboxID string, receipt proxy.Receipt) {
	if why := unpromotable(sandboxID, receipt); why != "" {
		fmt.Fprintf(os.Stderr, "veris-proxy: nothing was promoted: %s\n", why)
		return
	}
	client, err := discovery.NewClient(spec.APIBase, spec.APIKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "veris-proxy: cannot promote: %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), promoteTimeout)
	defer cancel()
	result, err := client.Promote(ctx, spec.Environment, sandboxID,
		discovery.PromoteOptions{})
	if err != nil {
		// Reported, never fatal: the tests passed and the receipt proved it.
		// Failing the run here would turn a good verdict into a red one over
		// bookkeeping.
		fmt.Fprintf(os.Stderr,
			"veris-proxy: the run succeeded but promoting its world failed: %v\n", err)
		return
	}
	printPromotion(os.Stderr, result)
}

// unpromotable names why this run's world must not become the environment's
// default, or "" when it may. The caller has already established that the
// command exited 0 and every requirement held.
func unpromotable(sandboxID string, receipt proxy.Receipt) string {
	if sandboxID == "" {
		return "the sandbox this run deployed could not be identified"
	}
	if receipt.Total == 0 {
		// Unreachable behind the built-in empty-receipt assertion, but that
		// assertion is handed over entirely to explicit --require-service
		// flags, and a world nothing ever reached is the one thing that must
		// never become every future run's starting point.
		return "the sandbox received nothing, so there is no world to keep"
	}
	return ""
}

// printPromotion says what the capture did, including what it removed. The
// scrub is the surprising half -- a baseline carries the world, not the
// session that built it -- so it is printed rather than left to be discovered.
func printPromotion(w io.Writer, r *discovery.PromoteResult) {
	fmt.Fprintf(w,
		"veris-proxy: sandbox %s is now environment %s's default world "+
			"(%s, clock %s). Every later sandbox starts from it; "+
			"reset_environment clears the pin.\n",
		r.SandboxID, r.EnvironmentID, humanBytes(r.SizeBytes), r.ClockRestore)
	if len(r.Scrubbed) > 0 {
		services := make([]string, 0, len(r.Scrubbed))
		for name := range r.Scrubbed {
			services = append(services, name)
		}
		sort.Strings(services)
		var parts []string
		for _, name := range services {
			parts = append(parts, name+": "+strings.Join(r.Scrubbed[name], ", "))
		}
		fmt.Fprintf(w,
			"veris-proxy: run-scoped state was scrubbed from the baseline (%s)\n",
			strings.Join(parts, "; "))
	}
	if !r.CuratorClockRestored {
		fmt.Fprintln(w,
			"veris-proxy: that sandbox stayed frozen -- its clock could not be "+
				"restored. The baseline is unaffected.")
	}
}

// adviseUnpromotedEnvironment is the feedback loop nothing closed: an agent
// that rebuilds the same world every run is never told that it is doing so.
// One line, only when it is actionable -- the run succeeded, the world is
// non-empty, and the environment still has no baseline.
func adviseUnpromotedEnvironment(spec dockerRun, receipt proxy.Receipt) {
	if spec.Environment == "" || spec.Quiet || receipt.Total == 0 {
		return
	}
	client, err := discovery.NewClient(spec.APIBase, spec.APIKey)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env, err := client.Environment(ctx, spec.Environment)
	if err != nil || env.Promoted() {
		return
	}
	fmt.Fprintf(os.Stderr,
		"veris-proxy: %s has no promoted world, so every run rebuilds this state "+
			"from the boot profile. --promote-on-success keeps it.\n",
		spec.Environment)
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return "size unreported"
	case n < 1<<20:
		return fmt.Sprintf("%d kB", n>>10)
	case n < 1<<30:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(int64(1)<<30))
	}
}
