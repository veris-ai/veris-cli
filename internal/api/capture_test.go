package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCapturePollsDurableTerminalOutcomes(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "interrupted"} {
		t.Run(state, func(t *testing.T) {
			posts, gets := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-API-Key") != "key" {
					t.Error("missing auth")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					posts++
					var body CaptureRequest
					json.NewDecoder(r.Body).Decode(&body)
					if body.RequestID != "stable-id" || body.SandboxID != "source" {
						t.Error("lost request identity")
					}
					w.WriteHeader(202)
					json.NewEncoder(w).Encode(CaptureOperation{ID: "cap-one", Status: "running", Phase: "capture"})
					return
				}
				gets++
				json.NewEncoder(w).Encode(CaptureOperation{ID: "cap-one", Status: state, Phase: "complete", Result: json.RawMessage(`{"baseline":{"revision_id":"saved"}}`), Error: "capture failed"})
			}))
			defer server.Close()
			c := New(server.URL, "key")
			c.sleep = func(context.Context, time.Duration) error { return nil }
			result, supported, err := c.Capture(context.Background(), "env", CaptureRequest{SandboxID: "source", RequestID: "stable-id", Kind: "promote"}, nil)
			if !supported || posts != 1 || gets != 1 {
				t.Fatal("wrong request sequence", supported, posts, gets)
			}
			if state == "succeeded" {
				if err != nil || len(result) == 0 {
					t.Fatal(err)
				}
			} else {
				var captureErr *CaptureError
				if !errors.As(err, &captureErr) || captureErr.State != state {
					t.Fatal("terminal failure lost", err)
				}
			}
		})
	}
}

func TestCaptureFallsBackOnlyForAnAbsentRoute(t *testing.T) {
	for _, detail := range []string{"Not Found", "source sandbox not found"} {
		t.Run(detail, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(404)
				json.NewEncoder(w).Encode(map[string]string{"detail": detail})
			}))
			defer server.Close()
			_, supported, err := New(server.URL, "").Capture(context.Background(), "env", CaptureRequest{}, nil)
			if detail == "Not Found" {
				if supported || err != nil {
					t.Fatal("legacy route not recognized")
				}
			} else if !supported || err == nil {
				t.Fatal("scoped refusal permitted legacy replay")
			}
		})
	}
}

func TestCaptureDeadlinePreservesKnownOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(CaptureOperation{ID: "cap-still-running", Status: "running"})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, supported, err := New(server.URL, "").Capture(ctx, "env", CaptureRequest{}, nil)
	var pending *CaptureError
	if !supported || !errors.As(err, &pending) || pending.State != "unconfirmed" || pending.ID != "cap-still-running" {
		t.Fatal("lost uncertain outcome", err)
	}
}
