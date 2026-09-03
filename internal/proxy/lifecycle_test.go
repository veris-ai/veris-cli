package proxy

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/ca"
	"github.com/veris-ai/veris-cli/internal/config"
)

func startedProxy(t *testing.T) *Running {
	t.Helper()
	cfg := &config.Config{
		Version:     1,
		Listen:      "127.0.0.1:0",
		Mode:        config.ModeStrict,
		SandboxID:   "sbx_lifecycle",
		EnvID:       "env_lifecycle",
		Upstream:    config.Upstream{BaseURL: "https://sandbox.example.invalid"},
		Services:    []config.Service{{Name: "stripe", Hosts: []string{"api.stripe.com"}}},
		CanaryToken: "canary-lifecycle",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	authority, err := ca.Load(t.TempDir())
	if err != nil {
		t.Fatalf("ca.Load: %v", err)
	}
	running, err := New(cfg, authority, slog.New(slog.DiscardHandler), "test").
		Start(ListenOptions{
			Proxy:            "127.0.0.1:0",
			TransparentHTTP:  "127.0.0.1:0",
			TransparentHTTPS: "127.0.0.1:0",
		})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return running
}

// Wait released as soon as Shutdown began, so `serve` returned from main and
// the process exited while requests were still in flight -- the grace period
// was advertised and never actually taken.
func TestWaitReturnsOnlyAfterEveryListenerHasDrained(t *testing.T) {
	running := startedProxy(t)

	waited := make(chan error, 1)
	go func() { waited <- running.Wait() }()

	// Nothing has asked it to stop, so Wait must still be blocked.
	select {
	case err := <-waited:
		t.Fatalf("Wait returned before any shutdown: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := running.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("a clean shutdown should be a nil Wait, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait never returned after Shutdown drained")
	}
}

// The second caller used to be told the shutdown was done while the first was
// still draining.
func TestASecondShutdownWaitsForTheFirst(t *testing.T) {
	running := startedProxy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			_ = running.Shutdown(ctx)
			// Every listener is closed by the time any caller is released, so
			// Wait cannot block here.
			if err := running.Wait(); err != nil {
				t.Errorf("Wait after Shutdown returned %v", err)
			}
			done <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a concurrent Shutdown never returned")
		}
	}
}
