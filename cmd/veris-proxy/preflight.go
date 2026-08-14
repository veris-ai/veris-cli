package main

// The per-run gate.
//
// Everything here was already a precondition of `run`; what was missing was a
// way to assert them BEFORE a run, cheaply, with one exit code. Without that,
// a precondition that quietly stopped holding -- most often the API key, which
// lives in an MCP config the shell never sees -- surfaced as a confusing
// failure deep inside a run, and the measured response to that was never "stop
// and fix it" but "improvise around it": a hand-authored routing file, a
// tunnel, a base URL edited into the code under test. Each improvisation
// produces a green whose code path is not the shipping code path.
//
// So this command fails closed and suggests nothing. Every message names the
// fix for the thing that is missing, and never an alternative route to a
// green.
//
// It also reports one thing that is not a precondition at all: whether the
// environment has a promoted world. An agent that is rebuilding accounts and
// fixtures every run has no other way to learn that it is paying for setup
// somebody already paid for.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/veris-ai/veris-proxy/internal/discovery"
)

// exitPreflight is the assertion family's exit code, shared with `check`: this
// binary asserted something about the world and it did not hold. Distinct from
// 1 (usage) so a caller can tell "you typed it wrong" from "your environment
// is not ready".
const exitPreflight = 2

func cmdPreflight(args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	environment := fs.String("environment", os.Getenv("VERIS_ENVIRONMENT_ID"),
		"environment `id` the run will use (also read from $VERIS_ENVIRONMENT_ID)")
	apiBase := fs.String("api-base", "", "control plane base URL (defaults to $"+discovery.EnvAPIBase+")")
	apiKey := fs.String("api-key", "", "control plane API key (defaults to $"+discovery.EnvAPIKey+")")
	image := fs.String("image", "",
		"also require this test `image` to be built locally, so the run does not "+
			"discover a missing image after deploying a sandbox")
	quiet := fs.Bool("quiet", false, "print nothing when everything holds")
	if err := fs.Parse(args); err != nil {
		return err
	}

	report := runPreflight(preflightSpec{
		Environment: *environment,
		APIBase:     *apiBase,
		APIKey:      *apiKey,
		Image:       *image,
	})
	report.write(os.Stderr, *quiet)
	if !report.ok() {
		return exitCode(exitPreflight)
	}
	return nil
}

type preflightSpec struct {
	Environment string
	APIBase     string
	APIKey      string
	Image       string
}

// preflightCheck is one precondition and its verdict. Detail carries the fix
// when it failed, and the fact worth knowing when it held.
type preflightCheck struct {
	Name   string
	Failed bool
	Detail string
}

type preflightReport struct {
	Checks []preflightCheck
	// Notes are observations that are not preconditions -- true or false, the
	// run may proceed. Today: whether the environment has a promoted world.
	Notes []string
}

func (r *preflightReport) add(name string, err error, okDetail string) {
	c := preflightCheck{Name: name, Detail: okDetail}
	if err != nil {
		c.Failed, c.Detail = true, err.Error()
	}
	r.Checks = append(r.Checks, c)
}

func (r *preflightReport) ok() bool {
	for _, c := range r.Checks {
		if c.Failed {
			return false
		}
	}
	return true
}

func (r *preflightReport) write(w io.Writer, quiet bool) {
	if r.ok() && quiet {
		return
	}
	for _, c := range r.Checks {
		mark := "ok  "
		if c.Failed {
			mark = "FAIL"
		}
		line := fmt.Sprintf("%s %s", mark, c.Name)
		if c.Detail != "" {
			line += ": " + c.Detail
		}
		fmt.Fprintf(w, "veris-proxy preflight: %s\n", line)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "veris-proxy preflight: note  %s\n", n)
	}
	if !r.ok() {
		// The one sentence this command exists to make unambiguous. An agent
		// reading a failure above must not read on to an alternative route.
		fmt.Fprintln(w,
			"veris-proxy preflight: a precondition is missing. Fix it or stop; "+
				"do not route the code under test by hand -- no base URLs, no "+
				"config file, no tunnel. A green earned that way tests a code path "+
				"that does not ship.")
	}
}

// runPreflight performs the checks in dependency order, so the first failure
// reported is the one to fix. Each is bounded: the whole command is meant to
// cost about a second, and one that hangs would be routed around.
func runPreflight(spec preflightSpec) preflightReport {
	var report preflightReport

	client, credErr := discovery.NewClient(spec.APIBase, spec.APIKey)
	report.add("credential", credErr, "$"+discovery.EnvAPIKey+" is set")
	if credErr != nil {
		// Only useful when there is no key at all, and the client's own error
		// already names the variable. This names where the key already is: in
		// the measured runs the agent held it in a readable MCP config and
		// still could not use the CLI.
		report.Checks[len(report.Checks)-1].Detail += ". If the Veris MCP server " +
			"is registered, the same key is its X-API-Key header value; export " +
			"that as $" + discovery.EnvAPIKey + ". Ask the user for it otherwise -- " +
			"it arrives out of band and is never written into the repo"
		return report
	}

	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()

	if spec.Environment == "" {
		// No environment named, so the control plane is probed on its own. A
		// run still needs one, and saying so here beats a run that deploys
		// nothing and reports an empty receipt.
		report.add("control plane", reachable(ctx, client),
			"reachable at "+client.APIBase+" and the key is accepted")
		report.add("environment", errors.New(
			"no environment id: pass --environment, or set $VERIS_ENVIRONMENT_ID. "+
				"The run needs one -- it is what deploys the sandbox"), "")
	} else {
		env, err := client.Environment(ctx, spec.Environment)
		report.add("control plane", err,
			"reachable at "+client.APIBase+" and the key is accepted")
		if err == nil {
			report.add("environment", nil, fmt.Sprintf("%s runs %s",
				spec.Environment, describeServices(env.Services)))
			report.Notes = append(report.Notes, promotedNote(spec.Environment, env))
		}
	}

	report.add("docker", dockerReady(ctx),
		"the daemon answers; the run's containers can start")

	if spec.Image != "" {
		report.add("test image", imageBuilt(ctx, spec.Image),
			spec.Image+" is built")
	}
	return report
}

// preflightTimeout bounds the whole command. A gate that can hang is a gate
// somebody skips.
const preflightTimeout = 15 * time.Second

func reachable(ctx context.Context, client *discovery.Client) error {
	// Any authenticated read does; the catalog is the cheapest and needs no
	// id. Reached through Environment's own error handling would require an
	// environment, which is exactly the case this branch does not have.
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, client.APIBase+"/v1/services", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", client.APIKey)
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the control plane at %s: %w. "+
			"Check $%s, or the network between here and it",
			client.APIBase, err, discovery.EnvAPIBase)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("%s refused the API key (%d); it is wrong, revoked, "+
			"or for another deployment", client.APIBase, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s answered %s", client.APIBase, resp.Status)
	}
	return nil
}

// dockerReady checks the daemon, not just the binary. `docker` on PATH with no
// daemon behind it fails at the first `docker run`, minutes into a run.
func dockerReady(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker is not on PATH. Every run is containerised, so " +
			"this skill does not proceed without it -- install docker, or move " +
			"the work to a machine with a daemon")
	}
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker is installed but its daemon did not answer (%s). "+
			"Start it and try again", firstLine(string(out)))
	}
	return nil
}

func imageBuilt(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", image)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s is not built here (%s). Build it from the "+
			"Dockerfile the setup phase wrote, then run preflight again",
			image, firstLine(string(out)))
	}
	return nil
}

// promotedNote closes the loop nothing else closes. An environment with no
// baseline makes every run rebuild the same world from the boot profile, and
// the run that is paying for it is the one that never hears about it.
func promotedNote(environmentID string, env *discovery.Environment) string {
	if !env.Promoted() {
		return fmt.Sprintf(
			"%s has no promoted world: every sandbox boots the stock profile, so "+
				"any accounts, connections or fixtures the tests need are rebuilt "+
				"each run. `run --promote-on-success`, or `promote --sandbox <id>` "+
				"before ending a long session, keeps the world once",
			environmentID)
	}
	when := "at an unrecorded time"
	if env.Baseline.PromotedAt != nil {
		when = "on " + env.Baseline.PromotedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"%s starts from a promoted world (captured %s from sandbox %s), so the "+
			"state it carries does not need rebuilding",
		environmentID, when, env.Baseline.SourceSandbox)
}

func describeServices(names []string) string {
	if len(names) == 0 {
		return "no services"
	}
	return strings.Join(names, ", ")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no output"
	}
	return s
}
