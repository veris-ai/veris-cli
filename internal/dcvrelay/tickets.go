package dcvrelay

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Ticket admits every request under BaseURL until ExpiresAt. BaseURL is a
// secret and is never printed.
type Ticket struct {
	// BaseURL ends in "/"; a request's path, without its leading "/", and
	// its query are appended to it.
	BaseURL string
	// ExpiresAt is when the bench relay stops admitting it; zero when the
	// bench API did not say.
	ExpiresAt time.Time
}

// StopError wraps an error from Server.Ticket after which no later request
// can be served either -- the session behind the relay has ended. Serve
// stops, closes every connection and returns it.
type StopError struct{ Err error }

func (e *StopError) Error() string { return e.Err.Error() }
func (e *StopError) Unwrap() error { return e.Err }

// defaultRefresh is how long before a ticket's expiry a fresh one replaces
// it, and how long a ticket with no expiry is used before it is replaced.
const defaultRefresh = time.Minute

// retryAfter is how long the refresher waits after a ticket request that
// failed for a reason other than the session ending.
const retryAfter = 15 * time.Second

// tickets holds the ticket new connections use, and replaces it before it
// expires. Existing connections are unaffected: the bench relay checks a
// ticket when a connection opens.
type tickets struct {
	fetch   func(context.Context) (Ticket, error)
	refresh time.Duration
	now     func() time.Time

	mu      sync.Mutex
	cur     Ticket
	renewAt time.Time // zero: no usable ticket held
}

// seed installs a ticket already in hand.
func (t *tickets) seed(tk Ticket) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.install(tk)
}

// minHold is the least time a ticket is used before it is replaced, however
// short its life reads: a short-lived ticket, or this machine's clock running
// ahead of the bench API's, must not turn the refresher into a busy loop.
const minHold = 10 * time.Second

// install records tk and when to replace it: refresh before it expires, or
// half way through a life shorter than twice that. The caller holds mu.
func (t *tickets) install(tk Ticket) {
	now := t.now()
	t.cur = tk
	renew := now.Add(t.refresh)
	if !tk.ExpiresAt.IsZero() {
		lead := min(t.refresh, tk.ExpiresAt.Sub(now)/2)
		renew = tk.ExpiresAt.Add(-lead)
	}
	t.renewAt = later(renew, now.Add(minHold))
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// current returns the base URL to use now, fetching a fresh ticket when the
// one held is due for replacement. Concurrent callers share one fetch.
func (t *tickets) current(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.renewAt.IsZero() && t.now().Before(t.renewAt) {
		return t.cur.BaseURL, nil
	}
	tk, err := t.fetch(ctx)
	if err != nil {
		return "", err
	}
	t.install(tk)
	return tk.BaseURL, nil
}

// invalidate drops base when it is still the ticket held, so the next
// connection fetches a fresh one: the bench relay refused it.
func (t *tickets) invalidate(base string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cur.BaseURL == base {
		t.renewAt = time.Time{}
	}
}

// due is how long until the held ticket must be replaced; zero when now.
func (t *tickets) due() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.renewAt.IsZero() {
		return 0
	}
	return max(t.renewAt.Sub(t.now()), 0)
}

// keepFresh replaces the ticket as it comes due, so a new connection never
// waits on a ticket request and an ended session is noticed within one
// ticket's life even when no one connects. It returns the *StopError that
// ended the session, or nil when ctx ends.
func (t *tickets) keepFresh(ctx context.Context) error {
	wait := t.due()
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		_, err := t.current(ctx)
		var stop *StopError
		switch {
		case errors.As(err, &stop):
			return stop
		case err != nil:
			wait = retryAfter
		default:
			wait = t.due()
		}
	}
}
