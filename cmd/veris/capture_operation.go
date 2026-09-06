package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/veris-ai/veris-cli/internal/api"
)

func durableCapture(ctx context.Context, s *session, envID string, request api.CaptureRequest, out any) (bool, error) {
	if request.RequestID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return true, err
		}
		request.RequestID = hex.EncodeToString(id[:])
	}
	if request.ClockRestore == "" {
		request.ClockRestore = api.ClockToday
	}
	s.ui.Detail("Capture request ID: %s (reuse --request-id %s after an interrupted wait)", request.RequestID, request.RequestID)
	last := ""
	result, supported, err := s.plane().Capture(ctx, envID, request, func(op api.CaptureOperation) {
		state := op.ID + ":" + op.Status + ":" + op.Phase
		if state != last {
			s.ui.Detail("Capture %s: %s (%s)", op.ID, op.Status, op.Phase)
			last = state
		}
	})
	if err != nil || !supported {
		return supported, err
	}
	if err := json.Unmarshal(result, out); err != nil {
		return true, &api.CaptureError{ID: request.RequestID, State: "unconfirmed", Detail: fmt.Sprintf("invalid capture result: %v", err)}
	}
	valid := false
	switch r := out.(type) {
	case *api.PromoteResponse:
		valid = r.EnvironmentID == envID && r.SandboxID == request.SandboxID && r.Baseline.SourceSandbox == request.SandboxID && r.Baseline.Image != "" && r.Baseline.RevisionID != ""
	case *api.SnapshotResponse:
		valid = r.Snapshot.EnvironmentID == envID && r.Snapshot.SourceSandbox == request.SandboxID && r.Snapshot.ID != "" && r.Snapshot.Image != "" && r.Snapshot.RevisionID != ""
	}
	if !valid {
		return true, &api.CaptureError{ID: request.RequestID, State: "unconfirmed", Detail: "capture result does not identify the requested source and saved image; retain the source"}
	}
	return true, nil
}
