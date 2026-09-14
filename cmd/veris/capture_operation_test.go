package main

import (
	"encoding/json"
	"github.com/veris-ai/veris-cli/internal/api"
	"net/http"
	"strings"
	"testing"
)

func TestDurableCaptureFailureRetainsSource(t *testing.T) {
	for _, state := range []string{"failed", "interrupted", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			p := newCapturePlane(t)
			captureBench(t, p)
			requests := 0
			p.operation = func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body api.CaptureRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.RequestID != "stable-request" || body.SandboxID != sbID || body.Kind != "promote" {
					t.Errorf("wrong request: %+v", body)
				}
				sbJSON(w, 202, map[string]any{"id": "cap-proof", "status": state, "phase": state, "error": "capture refused", "result": map[string]any{}})
			}
			code, _, stderr := runSandboxCLI(t, "baseline", "promote", "--yes", "--request-id", "stable-request")
			expected := 1
			if state == "succeeded" {
				expected = 4
			} // missing image is an uncertain outcome
			if code != expected {
				t.Fatalf("exit %d expected %d: %s", code, expected, stderr)
			}
			if requests != 1 || len(p.promoteBodies()) != 0 || len(p.sandboxDeletes()) != 0 {
				t.Fatal("failed capture replayed or source deleted")
			}
			if !strings.Contains(stderr, "stable-request") {
				t.Fatal("missing resume identity")
			}
		})
	}
}
