// Package callback registers the public callback URL with a sandbox.
//
// The sandbox will not deliver a webhook to a destination it does not know, and
// the destination is a tunnel hostname that did not exist a second ago. So the
// URL has to be written in at run time.
//
// `client.default_base_url` is world data, PATCHed through /veris/data like
// auth.mode or faults, and it is the single agent-writable field on that row --
// every probe field is server-owned behind a revision CAS. Creation-time
// `client_base_url` seeds the same row, which is why nothing here needs the
// sandbox to have been created differently.
//
// What this deliberately does NOT do is write any service's own webhook
// registration (Attio's `target_url`, and its equivalents). Registering a
// webhook is the client's code, the vendor validates it -- Attio answers a
// measured 400 for a non-https target -- and writing the row behind the client
// skips exactly the path production runs.
package callback

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ProbeState is the sandbox's verdict on whether it can reach the URL.
type ProbeState struct {
	State           string         `json:"probe_state"`
	BaseURL         string         `json:"default_base_url"`
	Revision        int            `json:"base_url_revision"`
	ProbedRevision  *int           `json:"probed_revision"`
	LastProbeResult map[string]any `json:"last_probe_result"`
}

// Answered reports whether an HTTP endpoint replied. It asserts only that
// something answered -- verifying it was the right app is the client's job,
// and the stored evidence is there for them to check.
func (p ProbeState) Answered() bool { return p.State == "answered" }

// DeadTunnel returns the signature when the sandbox recognised the tunnel edge
// answering for an absent origin, which is a different failure from an app that
// returned an error.
func (p ProbeState) DeadTunnel() string {
	if p.LastProbeResult == nil {
		return ""
	}
	sig, _ := p.LastProbeResult["dead_tunnel_signature"].(string)
	return sig
}

// Client talks to one service's control plane.
type Client struct {
	base string
	auth string
	http *http.Client
}

// Options carries the same access the proxy's own sandbox traffic uses. A
// callback client that authenticates differently from the rest of the proxy is
// one that works in development and is refused by a real sandbox.
type Options struct {
	// AuthValue is the credential sent as a bearer token, if the sandbox wants
	// one.
	AuthValue string
	// InsecureSkipVerify matches upstream.insecure_skip_verify, so a local
	// sandbox with a self-signed certificate is reachable here too.
	InsecureSkipVerify bool
}

// New addresses the control plane at a service's base URL.
func New(serviceBaseURL string, opts Options) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if opts.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // operator opt-in
	}
	return &Client{
		base: strings.TrimSuffix(serviceBaseURL, "/"),
		auth: opts.AuthValue,
		http: &http.Client{Timeout: 30 * time.Second, Transport: transport},
	}
}

// authorize adds the sandbox credential to every control-plane call, so PATCH,
// probe and clear are all reachable on an authenticated sandbox rather than
// only the ones that happened to be tested.
func (c *Client) authorize(req *http.Request) *http.Request {
	if c.auth != "" {
		req.Header.Set("Authorization", "Bearer "+c.auth)
	}
	return req
}

// Current reads the registration without changing it.
func (c *Client) Current(ctx context.Context) (ProbeState, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, c.base+"/veris/data?entity_type=client", nil)
	if err != nil {
		return ProbeState{}, err
	}
	res, err := c.http.Do(c.authorize(req))
	if err != nil {
		return ProbeState{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return ProbeState{}, fmt.Errorf("read the callback registration: %s", res.Status)
	}
	var body struct {
		Rows []ProbeState `json:"rows"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		// Not "nobody owns it". Teardown reads this as proof of ownership
		// before clearing, and a truncated body would let it erase another
		// run's destination.
		return ProbeState{}, fmt.Errorf("read the callback registration: %w", err)
	}
	if len(body.Rows) == 0 {
		return ProbeState{}, nil
	}
	return body.Rows[0], nil
}

// Register points the sandbox's callbacks at url and probes it.
//
// The probe is not optional politeness. It is the ingress twin of the canary:
// it proves the callback path was live BEFORE the run, rather than leaving a
// client to discover from an empty receipt that nothing could ever have
// arrived.
func (c *Client) Register(ctx context.Context, url string) (ProbeState, error) {
	if err := c.patch(ctx, url); err != nil {
		return ProbeState{}, err
	}
	return c.Probe(ctx)
}

// Clear unregisters the URL, so the next run does not inherit a dead hostname
// the dispatcher keeps trying.
func (c *Client) Clear(ctx context.Context) error { return c.patch(ctx, "") }

func (c *Client) patch(ctx context.Context, url string) error {
	row := map[string]any{"id": 1, "default_base_url": nil}
	if url != "" {
		row["default_base_url"] = url
	}
	body, err := json.Marshal(map[string]any{
		"data": map[string]any{"client": []any{row}},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPatch, c.base+"/veris/data", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, "register the callback URL")
}

// Probe asks the sandbox to try the registered URL now and report what it got.
func (c *Client) Probe(ctx context.Context) (ProbeState, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.base+"/veris/client/probe", nil)
	if err != nil {
		return ProbeState{}, err
	}
	res, err := c.http.Do(c.authorize(req))
	if err != nil {
		return ProbeState{}, fmt.Errorf("probe the callback URL: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return ProbeState{}, fmt.Errorf(
			"probe the callback URL: %s said %s: %s",
			c.base, res.Status, snippet(raw))
	}
	var out ProbeState
	if err := json.Unmarshal(raw, &out); err != nil {
		return ProbeState{}, fmt.Errorf("probe the callback URL: unreadable answer: %w", err)
	}
	return out, nil
}

func (c *Client) do(req *http.Request, what string) error {
	res, err := c.http.Do(c.authorize(req))
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s said %s: %s", what, c.base, res.Status, snippet(raw))
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
