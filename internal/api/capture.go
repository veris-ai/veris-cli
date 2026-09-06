package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type CaptureRequest struct {
	SandboxID                string `json:"sandbox_id"`
	Kind                     string `json:"kind"`
	RequestID                string `json:"request_id"`
	Name                     string `json:"name,omitempty"`
	ClockRestore             string `json:"clock_restore"`
	KeepExternalDestinations bool   `json:"keep_external_destinations"`
}

type CaptureOperation struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Phase  string          `json:"phase"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// CaptureError is an operation outcome, not a dropped legacy HTTP reply.
// Unconfirmed means the caller must retain the source and resume polling.
type CaptureError struct{ ID, State, Detail string }

func (e *CaptureError) Error() string {
	return fmt.Sprintf("capture %s %s: %s", e.ID, e.State, e.Detail)
}

// Capture uses the durable interface when available. Only an explicit
// missing-route response permits fallback to a legacy non-idempotent POST.
func (c *Client) Capture(ctx context.Context, envID string, request CaptureRequest, notify func(CaptureOperation)) (json.RawMessage, bool, error) {
	path := "/v1/environments/" + pathEscape(envID) + "/capture-operations"
	var op CaptureOperation
	err := c.do(ctx, http.MethodPost, path, request, &op)
	if err != nil {
		var ae *Error
		if errors.As(err, &ae) && ae.Status == 404 && (ae.Detail == "Not Found" || ae.Detail == "404 page not found") {
			return nil, false, nil
		}
		return nil, true, &CaptureError{request.RequestID, "unconfirmed", "start did not return an operation; retry with the same --request-id to recover it: " + err.Error()}
	}
	if op.ID == "" {
		return nil, true, &CaptureError{request.RequestID, "unconfirmed", "server returned no operation ID; retain the source"}
	}
	for {
		if notify != nil {
			notify(op)
		}
		switch op.Status {
		case "succeeded":
			if len(op.Result) == 0 || string(op.Result) == "null" {
				return nil, true, &CaptureError{op.ID, "unconfirmed", "completed operation has no result; retain the source"}
			}
			return op.Result, true, nil
		case "failed", "interrupted":
			return nil, true, &CaptureError{op.ID, op.Status, op.Error}
		case "running":
		default:
			return nil, true, &CaptureError{op.ID, "unconfirmed", "unknown operation state; retain the source"}
		}
		if err := c.wait(ctx, time.Second); err != nil {
			return nil, true, &CaptureError{op.ID, "unconfirmed", "stopped waiting; resume with the same --request-id"}
		}
		if err := c.do(ctx, http.MethodGet, path+"/"+pathEscape(op.ID), nil, &op); err != nil {
			return nil, true, &CaptureError{op.ID, "unconfirmed", "cannot read operation; retain the source and resume with the same --request-id: " + err.Error()}
		}
	}
}
