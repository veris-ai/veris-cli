package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// statusSequence answers GET /v1/sandboxes/{id} with each status in turn,
// repeating the last one for as long as the client keeps asking.
func statusSequence(statuses ...string) (http.HandlerFunc, *int) {
	var mu sync.Mutex
	polls := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := polls
		polls++
		mu.Unlock()
		if i >= len(statuses) {
			i = len(statuses) - 1
		}
		status := statuses[i]
		reason := "null"
		if status == StatusFailed {
			reason = `"container stripe exited: OOMKilled"`
		}
		respond(w, 200, fmt.Sprintf(`{"id":%q,"environment_id":"env","status":%q,"created_at":null,"expires_at":null,
			"services":[{"name":"stripe","status":"pending","url":"u","control_url":"u","env_hint":null,"routes":null}],
			"failure_reason":%s,"metadata":{},"snapshot_id":null}`, sandboxID, status, reason))
	}, &polls
}

func TestWaitSandboxFollowsProvisioningThroughDegradedToReady(t *testing.T) {
	h, polls := statusSequence(StatusProvisioning, StatusDegraded, StatusProvisioning, StatusReady)
	c, rec := serve(t, h)
	var seen []string
	sb, err := c.WaitSandbox(context.Background(), sandboxID, WaitOptions{
		Interval: 3 * time.Second,
		OnPoll:   func(s *Sandbox) { seen = append(seen, s.Status) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusReady || sb.ID != sandboxID {
		t.Errorf("got %+v", sb)
	}
	if *polls != 4 {
		t.Errorf("polls = %d, want 4", *polls)
	}
	want := []string{StatusProvisioning, StatusDegraded, StatusProvisioning, StatusReady}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Errorf("OnPoll saw %v, want %v", seen, want)
	}
	// Three waits of the configured interval between four polls.
	if len(rec.sleeps) != 3 || rec.sleeps[0] != 3*time.Second {
		t.Errorf("sleeps = %v", rec.sleeps)
	}
	if got := rec.last(); got.Method != http.MethodGet || got.Path != "/v1/sandboxes/"+sandboxID {
		t.Errorf("polled %s %s", got.Method, got.Path)
	}
}

func TestWaitSandboxReturnsAtOnceWhenAlreadyReady(t *testing.T) {
	h, polls := statusSequence(StatusReady)
	c, rec := serve(t, h)
	if _, err := c.WaitSandbox(context.Background(), sandboxID, WaitOptions{}); err != nil {
		t.Fatal(err)
	}
	if *polls != 1 || len(rec.sleeps) != 0 {
		t.Errorf("polls = %d, sleeps = %v", *polls, rec.sleeps)
	}
}

func TestWaitSandboxStopsOnFailedWithTheReason(t *testing.T) {
	h, polls := statusSequence(StatusProvisioning, StatusFailed, StatusReady)
	c, _ := serve(t, h)
	_, err := c.WaitSandbox(context.Background(), sandboxID, WaitOptions{})
	var failed *SandboxFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v (%T), want *SandboxFailedError", err, err)
	}
	if failed.Reason != "container stripe exited: OOMKilled" || failed.Sandbox.Status != StatusFailed {
		t.Errorf("got %+v", failed)
	}
	if want := "sandbox " + sandboxID + " failed: container stripe exited: OOMKilled"; failed.Error() != want {
		t.Errorf("Error() = %q, want %q", failed.Error(), want)
	}
	// "failed" is terminal: the ready that would have followed is never asked for.
	if *polls != 2 {
		t.Errorf("polls = %d, want 2", *polls)
	}
}

func TestWaitSandboxStopsOnTerminating(t *testing.T) {
	h, _ := statusSequence(StatusTerminating)
	c, _ := serve(t, h)
	_, err := c.WaitSandbox(context.Background(), sandboxID, WaitOptions{})
	var failed *SandboxFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v, want *SandboxFailedError", err)
	}
	if failed.Reason != "" || failed.Error() != "sandbox "+sandboxID+" is terminating" {
		t.Errorf("got %q (%+v)", failed.Error(), failed)
	}
}

func TestWaitSandboxTimesOutWithTheLastAnswer(t *testing.T) {
	h, polls := statusSequence(StatusProvisioning, StatusDegraded)
	c, _ := serve(t, h)
	// Real sleeps here: the deadline is wall-clock, and the point is that a
	// wait longer than the budget ends with the sandbox still on its way.
	c.sleep = nil
	start := time.Now()
	_, err := c.WaitSandbox(context.Background(), sandboxID, WaitOptions{
		Interval: time.Millisecond, Timeout: 30 * time.Millisecond,
	})
	var timeout *WaitTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v (%T), want *WaitTimeoutError", err, err)
	}
	if timeout.Sandbox == nil || timeout.Sandbox.Status != StatusDegraded {
		t.Errorf("last answer = %+v", timeout.Sandbox)
	}
	if timeout.Timeout != 30*time.Millisecond {
		t.Errorf("Timeout = %v", timeout.Timeout)
	}
	if want := "sandbox " + sandboxID + " still degraded after 30ms"; timeout.Error() != want {
		t.Errorf("Error() = %q, want %q", timeout.Error(), want)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("gave up after %v", elapsed)
	}
	if *polls < 2 {
		t.Errorf("polls = %d, want at least 2", *polls)
	}
}

func TestWaitSandboxReturnsTheContextError(t *testing.T) {
	h, polls := statusSequence(StatusProvisioning)
	c, _ := serve(t, h)
	c.sleep = nil
	ctx, cancel := context.WithCancel(context.Background())
	_, err := c.WaitSandbox(ctx, sandboxID, WaitOptions{
		Interval: time.Hour,
		OnPoll:   func(*Sandbox) { cancel() },
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if *polls != 1 {
		t.Errorf("polls = %d, want 1", *polls)
	}
}

func TestWaitSandboxReportsAReadFailure(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 404, `{"detail":"sandbox `+sandboxID+` not found"}`)
	})
	_, err := c.WaitSandbox(context.Background(), sandboxID, WaitOptions{})
	if !IsStatus(err, 404) {
		t.Errorf("err = %v, want the 404", err)
	}
	var failed *SandboxFailedError
	var timeout *WaitTimeoutError
	if errors.As(err, &failed) || errors.As(err, &timeout) {
		t.Error("a read failure is neither a failed sandbox nor a timeout")
	}
}
