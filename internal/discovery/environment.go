package discovery

// The environment behind a run, as opposed to the sandbox in front of it.
//
// A run only ever needs one fact from here, and it is the one nothing else
// reports: whether this environment has a promoted baseline. Without one every
// sandbox boots the stock profile, so every run rebuilds the same accounts,
// connections and fixtures and pays for it again -- and nothing in the run
// says so, which is why it kept happening.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Baseline is a promoted sandbox state pinned as the environment's boot image.
type Baseline struct {
	// Image is a digest reference, never a tag, so a later promote cannot
	// silently change what an environment boots.
	Image         string     `json:"image"`
	RevisionID    string     `json:"revision_id"`
	PromotedAt    *time.Time `json:"promoted_at"`
	SourceSandbox string     `json:"source_sandbox"`
}

// Environment is the control plane's description of an environment.
type Environment struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Services []string `json:"services"`
	// Baseline is nil when the environment boots the stock profile, which is
	// also what an older control plane that does not serve the field looks
	// like. Both mean "nothing promised here", which is what callers report.
	Baseline *Baseline `json:"baseline"`
}

// Promoted reports whether this environment starts its sandboxes from a world
// somebody built, rather than from the boot profile.
func (e *Environment) Promoted() bool {
	return e != nil && e.Baseline != nil && e.Baseline.Image != ""
}

// Environment reads one environment.
func (c *Client) Environment(ctx context.Context, environmentID string) (*Environment, error) {
	endpoint := fmt.Sprintf("%s/v1/environments/%s",
		c.APIBase, url.PathEscape(environmentID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var env Environment
	if err := c.call(req, &env, "read environment "+environmentID); err != nil {
		return nil, err
	}
	if env.ID == "" {
		env.ID = environmentID
	}
	return &env, nil
}

// PromoteOptions are the capture's two decisions.
type PromoteOptions struct {
	// ClockRestore is "rebase" (virtual time starts at the captured instant
	// and then runs) or "frozen" (exact replay, delivery paused). Empty means
	// the control plane's own default, which is rebase.
	ClockRestore string
	// KeepExternalDestinations promotes callback destinations that were not
	// this run's own receiver. Off by default: baking a third party's URL into
	// an environment points every future sandbox at them.
	KeepExternalDestinations bool
}

// PromoteResult is what the capture produced.
type PromoteResult struct {
	EnvironmentID string   `json:"environment_id"`
	SandboxID     string   `json:"sandbox_id"`
	Baseline      Baseline `json:"baseline"`
	ClockRestore  string   `json:"clock_restore"`
	SizeBytes     int64    `json:"size_bytes"`
	// CuratorClockRestored is false when the promoted sandbox could not be
	// handed its clock back. The baseline is still good; that sandbox stays
	// frozen, which matters only if something meant to keep using it.
	CuratorClockRestored bool `json:"curator_clock_restored"`
	// Scrubbed is what the capture truncated per service -- run-scoped state
	// (deliveries, request logs) that belongs to the session, not the world.
	Scrubbed map[string][]string `json:"scrubbed"`
}

// Promote makes a sandbox's current state the environment's default world.
//
// The capture is a boundary, not a snapshot: the sandbox stops answering
// vendor requests and is left frozen and scrubbed. So this is the last thing
// a run does with a sandbox, never something it does mid-suite.
func (c *Client) Promote(
	ctx context.Context, environmentID, sandboxID string, opts PromoteOptions,
) (*PromoteResult, error) {
	body := map[string]any{
		"keep_external_destinations": opts.KeepExternalDestinations,
	}
	if opts.ClockRestore != "" {
		body["clock_restore"] = opts.ClockRestore
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/v1/environments/%s/sandboxes/%s/promote",
		c.APIBase, url.PathEscape(environmentID), url.PathEscape(sandboxID))
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	var result PromoteResult
	if err := c.call(req, &result,
		"promote sandbox "+sandboxID+" into environment "+environmentID); err != nil {
		return nil, err
	}
	if result.SandboxID == "" {
		result.SandboxID = sandboxID
	}
	if result.EnvironmentID == "" {
		result.EnvironmentID = environmentID
	}
	return &result, nil
}
