package api

import (
	"context"
	"encoding/json"
	"net/http"
)

// Clock modes, as models.py's ClockMode literal spells them: "live" advances
// with real time (plus offset_seconds), "frozen" holds at frozen_time.
const (
	ClockModeLive   = "live"
	ClockModeFrozen = "frozen"
)

// SandboxClock is the one virtual clock every service in a sandbox reads
// (GET …/sandboxes/{id}/clock). ID is always 1: it is a singleton row.
// FrozenTime is the exact Unix second the clock holds at while frozen, and
// nil while live.
type SandboxClock struct {
	ID            int    `json:"id"`
	Mode          string `json:"mode"`
	OffsetSeconds int64  `json:"offset_seconds"`
	FrozenTime    *int64 `json:"frozen_time"`
}

// SetSandboxClockRequest is PATCH …/sandboxes/{id}/clock's body: a partial
// update, and the server reads which fields were SENT (model_fields_set).
// Mode and OffsetSeconds are sent when non-nil. FrozenTime is sent when
// non-nil, and as an explicit null when ClearFrozenTime is set, which is
// how a frozen clock's hold time is dropped on the way back to live; a
// request with nothing set encodes as `{}` and the server refuses it.
type SetSandboxClockRequest struct {
	Mode            *string
	OffsetSeconds   *int64
	FrozenTime      *int64
	ClearFrozenTime bool
}

// MarshalJSON writes exactly the fields the update names.
func (r SetSandboxClockRequest) MarshalJSON() ([]byte, error) {
	body := map[string]any{}
	if r.Mode != nil {
		body["mode"] = *r.Mode
	}
	if r.OffsetSeconds != nil {
		body["offset_seconds"] = *r.OffsetSeconds
	}
	switch {
	case r.FrozenTime != nil:
		body["frozen_time"] = *r.FrozenTime
	case r.ClearFrozenTime:
		body["frozen_time"] = nil
	}
	return json.Marshal(body)
}

// SetSandboxClockResponse is what a clock update returns: the clock as it
// now reads, and the warnings core raised on the way (moving time backwards
// is allowed, and explained).
type SetSandboxClockResponse struct {
	Clock    SandboxClock `json:"clock"`
	Warnings []string     `json:"warnings"`
}

// GetSandboxClock reads the sandbox's shared virtual clock (GET
// /v1/environments/{env}/sandboxes/{id}/clock).
func (c *Client) GetSandboxClock(ctx context.Context, envID, id string) (*SandboxClock, error) {
	var out SandboxClock
	if err := c.do(ctx, http.MethodGet, sandboxPath(envID, id)+"/clock", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetSandboxClock changes the sandbox's shared virtual clock once for every
// service (PATCH /v1/environments/{env}/sandboxes/{id}/clock). Only the
// fields the request names change.
func (c *Client) SetSandboxClock(ctx context.Context, envID, id string, req SetSandboxClockRequest) (*SetSandboxClockResponse, error) {
	var out SetSandboxClockResponse
	if err := c.do(ctx, http.MethodPatch, sandboxPath(envID, id)+"/clock", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
