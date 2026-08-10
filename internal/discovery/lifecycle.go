package discovery

// Creating a sandbox rather than attaching to one.
//
// `client.default_base_url` is a sandbox-wide singleton, so two concurrent runs
// sharing a sandbox overwrite each other's callback URL and the first run's
// webhooks are delivered to the second run's app -- silently, and with no way
// for either to notice. A sandbox per run removes that class rather than
// warning about it.
//
// It also removes the registration window: the tunnel needs only a local port,
// so it can be opened first and its URL passed as `client_base_url` at
// creation. The sandbox is then never alive without knowing where to deliver.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CreateOptions describes the sandbox to deploy.
type CreateOptions struct {
	// ClientBaseURL seeds the callback registration. The platform's own words:
	// it seeds the sandbox's client registration and is PATCHable later.
	ClientBaseURL string
	// TTLMinutes bounds how long the sandbox lives if we never delete it --
	// a crashed run must not leak one forever.
	TTLMinutes int
}

// Create deploys a sandbox for an environment and waits for it to be usable.
func (c *Client) Create(ctx context.Context, environmentID string, opts CreateOptions) (*Sandbox, error) {
	body, err := json.Marshal(map[string]any{
		"ttl_minutes":     opts.TTLMinutes,
		"client_base_url": nullIfEmpty(opts.ClientBaseURL),
	})
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/v1/environments/%s/sandboxes",
		c.APIBase, url.PathEscape(environmentID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	var sandbox Sandbox
	if err := c.call(req, &sandbox, "create a sandbox for environment "+environmentID); err != nil {
		return nil, err
	}
	return &sandbox, nil
}

// Delete removes a sandbox. Best-effort by nature: it runs during teardown,
// and a failure here is reported rather than fatal, since the TTL is the
// backstop.
func (c *Client) Delete(ctx context.Context, environmentID, sandboxID string) error {
	endpoint := fmt.Sprintf("%s/v1/environments/%s/sandboxes/%s",
		c.APIBase, url.PathEscape(environmentID), url.PathEscape(sandboxID))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	return c.call(req, nil, "delete sandbox "+sandboxID)
}

// WaitReady polls until the sandbox reports ready AND a service route answers.
//
// Both, because they are not the same thing. Measured against the dev control
// plane: a sandbox reporting "ready" still answered 502 "sandbox service
// unreachable" on /s/<id>/<service> for some seconds afterwards. Trusting the
// status field alone sends the first requests of a run -- callbacks and vendor
// traffic alike -- into that window.
func (c *Client) WaitReady(ctx context.Context, sandboxID string, timeout time.Duration) (*Sandbox, error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		sandbox, err := c.Fetch(ctx, sandboxID)
		if err != nil {
			return nil, err
		}
		if sandbox.Status == "ready" || sandbox.Status == "" {
			if err := c.serviceRoutesAnswer(ctx, sandbox); err == nil {
				return sandbox, nil
			}
			last = "ready, but its service routes are not serving yet"
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("sandbox %s is %s", sandboxID, last)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		// Terminal states cannot become ready, so polling one for the full
		// timeout only delays the report and the cleanup.
		if sandbox.Status == "failed" || sandbox.Status == "terminating" {
			return nil, fmt.Errorf("sandbox %s is %q and will not become ready",
				sandboxID, sandbox.Status)
		}
		last = sandbox.Status
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"sandbox %s was still %q after %s", sandboxID, last, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// serviceRoutesAnswer reports whether the sandbox's own HTTP surface is
// serving, which is what a run actually depends on.
//
// One service is enough: they share a pod, so the first to answer means the
// route is live. The health path is the one the registry declares.
func (c *Client) serviceRoutesAnswer(ctx context.Context, sandbox *Sandbox) error {
	if len(sandbox.Services) == 0 {
		return nil
	}
	svc := sandbox.Services[0]
	if svc.URL == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, strings.TrimSuffix(svc.URL, "/")+"/veris/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%s answered %s", svc.Name, resp.Status)
	}
	return nil
}

func (c *Client) call(req *http.Request, out any, what string) error {
	req.Header.Set("X-API-Key", c.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the control plane at %s: %w", c.APIBase, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s said %s: %s", what, c.APIBase, resp.Status, snippet(raw))
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: unreadable answer: %w", what, err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func snippet(b []byte) string {
	s := string(bytes.TrimSpace(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
