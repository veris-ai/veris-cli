package api

import (
	"context"
	"net/http"
)

// DeviceCode mints a pairing (POST /v1/device/code). Unauthenticated: the
// device is here because it has no credential yet.
func (c *Client) DeviceCode(ctx context.Context, clientName string) (*DeviceCodeResponse, error) {
	var out DeviceCodeResponse
	if err := c.do(ctx, http.MethodPost, "/v1/device/code", DeviceCodeRequest{ClientName: clientName}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeviceToken polls a pairing (POST /v1/device/token). Until the human
// approves, the answer is a 400 whose *Error.Code is one of RFC 8628's
// authorization_pending, slow_down, expired_token, access_denied or
// invalid_grant; the caller drives its loop on Code, not on Status.
func (c *Client) DeviceToken(ctx context.Context, deviceCode string) (*DeviceTokenResponse, error) {
	var out DeviceTokenResponse
	if err := c.do(ctx, http.MethodPost, "/v1/device/token", DeviceTokenRequest{DeviceCode: deviceCode}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Me answers who the presented credential is (GET /v1/me).
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var out Me
	if err := c.do(ctx, http.MethodGet, "/v1/me", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListAPIKeys lists the active organisation's keys, oldest first, revoked
// ones included (GET /v1/api-keys).
func (c *Client) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	var out []APIKey
	if err := c.do(ctx, http.MethodGet, "/v1/api-keys", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RevokeAPIKey revokes one key (POST /v1/api-keys/{id}/revoke). Idempotent;
// a key may revoke itself as the last step of a rotation.
func (c *Client) RevokeAPIKey(ctx context.Context, id string) (*APIKey, error) {
	var out APIKey
	if err := c.do(ctx, http.MethodPost, "/v1/api-keys/"+pathEscape(id)+"/revoke", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
