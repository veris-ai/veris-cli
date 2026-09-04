package callback

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterWaitsForSandboxDNSBeforeReturning(t *testing.T) {
	var probes atomic.Int32
	var patches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patches.Add(1)
			w.Write([]byte(`{}`))
			return
		}
		if probes.Add(1) == 1 {
			w.Write([]byte(`{"probe_state":"unreachable","last_probe_result":{"outcome":"connect_error","error":"gaierror"}}`))
			return
		}
		// The forwarding proxy is reachable, but the workload has not started yet.
		w.Write([]byte(`{"probe_state":"answered","last_probe_result":{"outcome":"http_response","status":502}}`))
	}))
	defer srv.Close()
	state, err := New(srv.URL, Options{}).Register(context.Background(), "https://fresh.trycloudflare.com")
	if err != nil || !state.Answered() || probes.Load() != 2 || patches.Load() != 1 {
		t.Fatalf("state=%+v err=%v probes=%d patches=%d", state, err, probes.Load(), patches.Load())
	}
}

func TestDNSReadinessIsBounded(t *testing.T) {
	c, _ := sandbox(t, map[string]any{"probe_state": "unreachable", "last_probe_result": map[string]any{"outcome": "connect_error", "error": "gaierror"}})
	_, err := c.probeResolved(context.Background(), 30*time.Millisecond, time.Millisecond)
	if !errors.Is(err, ErrDNSNotReady) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestDNSReadinessDoesNotWaitForAnUnstartedAppOrHideOtherFailures(t *testing.T) {
	for _, result := range []map[string]any{
		{"outcome": "connect_error", "error": "ConnectionRefusedError"},
		{"outcome": "tls_error", "error": "SSLCertVerificationError"},
		{"outcome": "http_response", "status": 530, "dead_tunnel_signature": "error code: 1033"},
	} {
		t.Run(result["outcome"].(string), func(t *testing.T) {
			var probes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				json.NewEncoder(w).Encode(map[string]any{"probe_state": "unreachable", "last_probe_result": result})
			}))
			defer srv.Close()
			state, err := New(srv.URL, Options{}).probeResolved(context.Background(), time.Second, time.Millisecond)
			if err != nil || state.Answered() || probes.Load() != 1 {
				t.Fatalf("state=%+v err=%v probes=%d", state, err, probes.Load())
			}
		})
	}
}

func TestDNSReadinessCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.Write([]byte(`{"probe_state":"unreachable","last_probe_result":{"outcome":"connect_error","error":"gaierror"}}`))
		cancel()
	}))
	defer srv.Close()
	_, err := New(srv.URL, Options{}).probeResolved(ctx, time.Minute, time.Minute)
	if !errors.Is(err, context.Canceled) || probes.Load() != 1 {
		t.Fatalf("err=%v probes=%d", err, probes.Load())
	}
}
