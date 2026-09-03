package api

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// CreateSandbox deploys a sandbox of the environment (POST
// /v1/environments/{id}/sandboxes, 201). It returns at once with status
// "provisioning"; WaitSandbox follows it to "ready".
func (c *Client) CreateSandbox(ctx context.Context, envID string, req CreateSandboxRequest) (*Sandbox, error) {
	var out Sandbox
	if err := c.do(ctx, http.MethodPost, "/v1/environments/"+pathEscape(envID)+"/sandboxes", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSandbox reads one sandbox by id alone (GET /v1/sandboxes/{id}): the id
// is the whole capability, so no environment is needed to look it up.
func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	var out Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+pathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSandboxServices reads a sandbox's services (GET /v1/sandboxes/{id}/services).
func (c *Client) GetSandboxServices(ctx context.Context, id string) ([]ServiceInfo, error) {
	var out []ServiceInfo
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+pathEscape(id)+"/services", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListSandboxes lists an environment's sandboxes (GET …/sandboxes).
func (c *Client) ListSandboxes(ctx context.Context, envID string) ([]Sandbox, error) {
	var out []Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/environments/"+pathEscape(envID)+"/sandboxes", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteSandbox tears a sandbox down (DELETE …/sandboxes/{id}, 204).
func (c *Client) DeleteSandbox(ctx context.Context, envID, id string) error {
	return c.do(ctx, http.MethodDelete, sandboxPath(envID, id), nil, nil)
}

// ResetSandbox restores every service to its boot-profile seed (POST
// …/sandboxes/{id}/reset). Refused with 409 when the sandbox booted an
// image -- a snapshot or a promoted baseline -- since reseeding would
// silently replace that world.
func (c *Client) ResetSandbox(ctx context.Context, envID, id string) (*ResetResponse, error) {
	var out ResetResponse
	if err := c.do(ctx, http.MethodPost, sandboxPath(envID, id)+"/reset", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSandbox changes a running sandbox's mutable fields (PATCH
// …/sandboxes/{id}); see SandboxUpdate for what is sent when.
func (c *Client) UpdateSandbox(ctx context.Context, envID, id string, u SandboxUpdate) (*Sandbox, error) {
	var out Sandbox
	if err := c.do(ctx, http.MethodPatch, sandboxPath(envID, id), u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PromoteSandbox pins the sandbox's current world as the environment's
// baseline (POST …/sandboxes/{id}/promote). The sandbox is left frozen and
// scrubbed.
func (c *Client) PromoteSandbox(ctx context.Context, envID, id string, req PromoteRequest) (*PromoteResponse, error) {
	var out PromoteResponse
	if err := c.do(ctx, http.MethodPost, sandboxPath(envID, id)+"/promote", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EgressCredential mints the gateway-mode coordinates for a sandbox (POST
// …/sandboxes/{id}/egress-credential, 201). 404 also means the deployment
// does not offer gateway mode.
func (c *Client) EgressCredential(ctx context.Context, envID, id string) (*EgressCredential, error) {
	var out EgressCredential
	if err := c.do(ctx, http.MethodPost, sandboxPath(envID, id)+"/egress-credential", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func sandboxPath(envID, id string) string {
	return "/v1/environments/" + pathEscape(envID) + "/sandboxes/" + pathEscape(id)
}

// WaitOptions tunes WaitSandbox. Zero values mean the defaults below.
type WaitOptions struct {
	// Interval between polls; default 2 s.
	Interval time.Duration
	// Timeout is the whole wait's budget; default 5 min.
	Timeout time.Duration
	// OnPoll sees every answer, ready or not, so a spinner can say which
	// services are still pending.
	OnPoll func(*Sandbox)
}

const (
	defaultWaitInterval = 2 * time.Second
	defaultWaitTimeout  = 5 * time.Minute
)

// SandboxFailedError is a sandbox that will not become ready: status
// "failed" (Reason is the container's failure_reason) or "terminating".
type SandboxFailedError struct {
	Sandbox *Sandbox
	Reason  string
}

func (e *SandboxFailedError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("sandbox %s is %s", e.Sandbox.ID, e.Sandbox.Status)
	}
	return fmt.Sprintf("sandbox %s %s: %s", e.Sandbox.ID, e.Sandbox.Status, e.Reason)
}

// WaitTimeoutError is a wait that ran out of budget while the sandbox was
// still on its way; the caller maps it to exit 4 (indeterminate), since the
// sandbox may yet come up. Sandbox is the last answer seen.
type WaitTimeoutError struct {
	Sandbox *Sandbox
	Timeout time.Duration
}

func (e *WaitTimeoutError) Error() string {
	return fmt.Sprintf("sandbox %s still %s after %s", e.Sandbox.ID, e.Sandbox.Status, e.Timeout)
}

// WaitSandbox polls GetSandbox until the sandbox is ready. "degraded" keeps
// polling: it is a sandbox whose services are not all up yet. "failed" and
// "terminating" end the wait with *SandboxFailedError; the deadline with
// *WaitTimeoutError; a cancelled ctx with ctx.Err(). Any other error from
// the read (a 404, a control plane that stayed down through the GET
// retries) is returned as it is.
func (c *Client) WaitSandbox(ctx context.Context, id string, o WaitOptions) (*Sandbox, error) {
	interval := o.Interval
	if interval <= 0 {
		interval = defaultWaitInterval
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		sb, err := c.GetSandbox(ctx, id)
		if err != nil {
			return nil, err
		}
		if o.OnPoll != nil {
			o.OnPoll(sb)
		}
		switch sb.Status {
		case StatusReady:
			return sb, nil
		case StatusFailed, StatusTerminating:
			return nil, &SandboxFailedError{Sandbox: sb, Reason: sb.FailureReason}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &WaitTimeoutError{Sandbox: sb, Timeout: timeout}
		}
		// The last wait is cut to the deadline so a long interval cannot
		// overshoot a short budget.
		if err := c.wait(ctx, min(interval, remaining)); err != nil {
			return nil, err
		}
	}
}
