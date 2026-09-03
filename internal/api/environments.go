package api

import (
	"context"
	"net/http"
)

// ListServices lists the catalog: every service an environment may name,
// with the vendor hostnames each covers (GET /v1/services).
func (c *Client) ListServices(ctx context.Context) ([]CatalogService, error) {
	var out []CatalogService
	if err := c.do(ctx, http.MethodGet, "/v1/services", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateEnvironment defines a named set of services (POST /v1/environments).
// An empty name is omitted from the body and the server leaves it null.
func (c *Client) CreateEnvironment(ctx context.Context, name string, services []string) (*Environment, error) {
	var out Environment
	req := CreateEnvironmentRequest{Name: name, Services: services}
	if err := c.do(ctx, http.MethodPost, "/v1/environments", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListEnvironments lists the caller's environments (GET /v1/environments).
func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	var out []Environment
	if err := c.do(ctx, http.MethodGet, "/v1/environments", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetEnvironment reads one environment (GET /v1/environments/{id}).
func (c *Client) GetEnvironment(ctx context.Context, id string) (*Environment, error) {
	var out Environment
	if err := c.do(ctx, http.MethodGet, "/v1/environments/"+pathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteEnvironment deletes the configuration only; running sandboxes live
// out their TTL (DELETE /v1/environments/{id}, 204).
func (c *Client) DeleteEnvironment(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/environments/"+pathEscape(id), nil, nil)
}

// ResetEnvironment clears the environment's baseline pin when baselineImage
// is nil, or rolls back to one of its kept images (POST …/reset). The key is
// always sent: null is the clear, not an omission.
func (c *Client) ResetEnvironment(ctx context.Context, id string, baselineImage *string) (*Environment, error) {
	var out Environment
	req := ResetEnvironmentRequest{BaselineImage: baselineImage}
	if err := c.do(ctx, http.MethodPost, "/v1/environments/"+pathEscape(id)+"/reset", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListSnapshots lists the environment's saved worlds, newest first
// (GET /v1/environments/{id}/snapshots).
func (c *Client) ListSnapshots(ctx context.Context, envID string) ([]Snapshot, error) {
	var out []Snapshot
	if err := c.do(ctx, http.MethodGet, "/v1/environments/"+pathEscape(envID)+"/snapshots", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateSnapshot captures one sandbox's world as a new snapshot of its
// environment (POST /v1/environments/{id}/snapshots, 201). Nothing is
// pinned; the captured sandbox is left frozen and scrubbed, which is why
// the answer carries curator_clock_restored and scrubbed beside the record.
func (c *Client) CreateSnapshot(ctx context.Context, envID string, req CreateSnapshotRequest) (*SnapshotResponse, error) {
	var out SnapshotResponse
	if err := c.do(ctx, http.MethodPost, "/v1/environments/"+pathEscape(envID)+"/snapshots", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteSnapshot deletes a snapshot and its image (DELETE …/snapshots/{id},
// 204); refused with 409 while a running sandbox booted from it.
func (c *Client) DeleteSnapshot(ctx context.Context, envID, snapID string) error {
	return c.do(ctx, http.MethodDelete,
		"/v1/environments/"+pathEscape(envID)+"/snapshots/"+pathEscape(snapID), nil, nil)
}

// Healthz is the unauthenticated liveness route (GET /healthz).
func (c *Client) Healthz(ctx context.Context) (*Healthz, error) {
	var out Healthz
	if err := c.do(ctx, http.MethodGet, "/healthz", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GatewayHealth says whether this deployment offers BYOP gateway mode
// (GET /v1/gateway/health).
func (c *Client) GatewayHealth(ctx context.Context) (*GatewayHealth, error) {
	var out GatewayHealth
	if err := c.do(ctx, http.MethodGet, "/v1/gateway/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
