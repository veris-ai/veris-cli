// Package api is the veris CLI's client for the Veris control plane: the
// environments, sandboxes, snapshots, keys and device pairing that
// service-sandbox's api/app serves under /v1.
//
// Every type mirrors api/app/models.py field for field, so a response can be
// printed raw under --json and still match what a reader of the Python sees.
// The transport is deliberately plain: JSON in and out, one header for the
// key, and a small retry for GETs only -- a POST that provisioned a sandbox
// and then timed out must not be sent twice.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/veris-ai/veris-cli/internal/direct"
)

// DefaultBase is the control plane this client talks to unless told otherwise.
const DefaultBase = "https://svc.api.veris.ai"

// maxBody bounds what is read of any answer; the largest real one (a sandbox
// with every service and its routes) is a few tens of kilobytes.
const maxBody = 4 << 20

// Client talks to one control plane with one credential.
type Client struct {
	// Base is the control plane's origin, no trailing slash.
	Base string
	// Key is sent as X-API-Key when non-empty. It is never written to a log
	// or an error: the device routes are the one place a request legitimately
	// goes out without it.
	Key string
	// HTTP is the transport; New gives it a 30 s timeout.
	HTTP *http.Client
	// UserAgent names the binary and version to the control plane's logs.
	UserAgent string

	// sleep waits between GET attempts and sandbox polls; tests replace it
	// so a retry schedule of seconds runs in microseconds.
	sleep func(context.Context, time.Duration) error
}

// New returns a client for base with key. A trailing slash on base is
// trimmed so paths can be appended verbatim.
func New(base, key string) *Client {
	if base == "" {
		base = DefaultBase
	}
	return &Client{
		Base: strings.TrimSuffix(base, "/"),
		Key:  key,
		HTTP: &http.Client{Timeout: 30 * time.Second, Transport: direct.Transport()},
	}
}

// Error is any non-2xx answer from the control plane.
type Error struct {
	Status int
	Method string
	// URL is kept for diagnostics but stays out of Error(): the human line
	// is what the server said, not where it said it.
	URL string
	// Detail is the human line: the `detail` string, or a 422's reasons
	// joined, or the first 200 bytes of a body that was not JSON.
	Detail string
	// Reasons carries a 422's list, one entry per item: pydantic's
	// {loc, msg} objects render as "customers[0].email: must be a string",
	// bare strings pass through unchanged.
	Reasons []string
	// Code is the {"error": …} value of a device-token 400 (RFC 8628's
	// authorization_pending, slow_down, expired_token, access_denied,
	// invalid_grant); empty for every other refusal.
	Code string
	// Body is the raw answer, for --json and for shapes this package does
	// not recognise.
	Body []byte

	// retryAfter is the Retry-After header as sent, read by the GET retry.
	retryAfter string
}

// Error renders "[404] environment abc not found". The URL is deliberately
// absent: callers know what they asked for, and the line is shown to people.
func (e *Error) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Code
	}
	if msg == "" {
		msg = strings.ToLower(http.StatusText(e.Status))
	}
	return fmt.Sprintf("[%d] %s", e.Status, msg)
}

// IsStatus reports whether err is (or wraps) an *Error with that status.
func IsStatus(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == code
}

// retryBackoff is the wait before each GET re-attempt, in order; a
// Retry-After header on a 5xx replaces the scheduled wait for that attempt.
var retryBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// maxGetAttempts bounds retries of an idempotent read. Everything else is
// sent exactly once: a create or a promote that timed out may well have
// happened, and repeating it would do it twice.
const maxGetAttempts = 3

// do sends one request and decodes the answer into out (nil to discard).
//
// POST and PATCH always carry a JSON body -- `{}` when the call has nothing
// to say -- because the load balancer in front of the control plane answers
// 411 to a bodiless POST. A non-2xx answer becomes an *Error; a transport
// failure on a GET is retried, on anything else reported as it is.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	switch method {
	case http.MethodPost, http.MethodPatch:
		if in == nil {
			body = []byte("{}")
		} else {
			var err error
			if body, err = json.Marshal(in); err != nil {
				return fmt.Errorf("encode %s %s: %w", method, path, err)
			}
		}
	default:
		if in != nil {
			return fmt.Errorf("%s %s cannot carry a body", method, path)
		}
	}

	endpoint := c.Base + path
	attempts := 1
	if method == http.MethodGet {
		attempts = maxGetAttempts
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			wait := retryBackoff[attempt-1]
			if after, ok := retryAfter(last); ok {
				wait = after
			}
			if err := c.wait(ctx, wait); err != nil {
				return err
			}
		}
		resp, respBody, err := c.send(ctx, method, endpoint, body)
		if err != nil {
			last = err
			if ctx.Err() != nil {
				return err
			}
			continue
		}
		if resp.StatusCode/100 == 2 {
			return decode(method, path, respBody, out)
		}
		apiErr := parseError(method, endpoint, resp, respBody)
		last = apiErr
		if resp.StatusCode < 500 {
			return apiErr
		}
	}
	return last
}

// send performs one round trip and reads the whole body so the connection can
// be reused and a retry starts from nothing half-consumed.
func (c *Client) send(ctx context.Context, method, endpoint string, body []byte) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Key != "" {
		req.Header.Set("X-API-Key", c.Key)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach the control plane at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, nil, fmt.Errorf("read %s %s: %w", method, endpoint, err)
	}
	return resp, respBody, nil
}

// wait sleeps for d or until ctx is done, whichever is first.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// maxRetryAfter caps how long a Retry-After is honoured. A load balancer
// answering 503 with an hour would otherwise park a command in silence;
// past this the schedule's own step is used and the 503 surfaces sooner.
const maxRetryAfter = 30 * time.Second

// retryAfter reads the Retry-After seconds a 5xx carried, if any. The HTTP
// date form is ignored: the control plane's own answers use seconds, and a
// misread date could park a client for hours -- as would a large seconds
// value, which is why the wait is capped at maxRetryAfter.
func retryAfter(err error) (time.Duration, bool) {
	var e *Error
	if !errors.As(err, &e) || e.retryAfter == "" {
		return 0, false
	}
	secs, convErr := strconv.Atoi(strings.TrimSpace(e.retryAfter))
	if convErr != nil || secs < 0 {
		return 0, false
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter), true
}

// decode reads a 2xx body into out. A route that answers with nothing (a
// 204) passes out == nil; where a body was expected, an empty one is a
// proxy or load balancer answering for the control plane and is an error,
// not a zero-valued record that WaitSandbox would poll on until its deadline.
func decode(method, path string, body []byte, out any) error {
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("decode %s %s: empty body", method, path)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

// parseError turns a non-2xx answer into *Error, reading the three shapes
// FastAPI produces for `detail` -- a string, a list of strings, a list of
// pydantic {loc, msg, type} objects -- plus the object update_sandbox's 502
// sends ({service: reason}) and the bare {"error": code} of the device-token
// route. Anything else keeps its first 200 bytes as the human line.
func parseError(method, endpoint string, resp *http.Response, body []byte) *Error {
	e := &Error{
		Status:     resp.StatusCode,
		Method:     method,
		URL:        endpoint,
		Body:       body,
		retryAfter: resp.Header.Get("Retry-After"),
	}
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		e.Detail = truncate(body, 200)
		return e
	}
	if len(envelope.Error) > 0 {
		var code string
		if json.Unmarshal(envelope.Error, &code) == nil {
			e.Code = code
		}
	}
	if len(envelope.Detail) > 0 {
		e.Detail, e.Reasons = parseDetail(envelope.Detail)
	}
	if e.Detail == "" && e.Code == "" && len(envelope.Detail) == 0 && len(envelope.Error) == 0 {
		e.Detail = truncate(body, 200)
	}
	return e
}

// parseDetail reads one `detail` value. The list forms populate Reasons and
// join them for the human line; the string form is the line itself.
func parseDetail(raw json.RawMessage) (detail string, reasons []string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) == nil {
		for _, item := range items {
			var str string
			if json.Unmarshal(item, &str) == nil {
				reasons = append(reasons, str)
				continue
			}
			var obj struct {
				Msg string `json:"msg"`
				Loc []any  `json:"loc"`
			}
			if json.Unmarshal(item, &obj) == nil && obj.Msg != "" {
				if loc := renderLoc(obj.Loc); loc != "" {
					reasons = append(reasons, loc+": "+obj.Msg)
				} else {
					reasons = append(reasons, obj.Msg)
				}
				continue
			}
			reasons = append(reasons, string(item))
		}
		return strings.Join(reasons, "; "), reasons
	}
	var failures map[string]string
	if json.Unmarshal(raw, &failures) == nil {
		names := make([]string, 0, len(failures))
		for name := range failures {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			reasons = append(reasons, name+": "+failures[name])
		}
		return strings.Join(reasons, "; "), reasons
	}
	return truncate(raw, 200), nil
}

// renderLoc turns pydantic's loc ["body", "customers", 0, "email"] into
// "customers[0].email". The leading "body"/"query"/"path" is dropped: the
// caller sent one request and knows where its fields went.
func renderLoc(loc []any) string {
	var b strings.Builder
	for i, part := range loc {
		switch v := part.(type) {
		case float64:
			fmt.Fprintf(&b, "[%d]", int(v))
		case string:
			if i == 0 && (v == "body" || v == "query" || v == "path" || v == "header") {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(v)
		}
	}
	return b.String()
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// pathEscape is url.PathEscape under a name that says what it is for: ids
// are pattern-checked server-side, but a value that came from a flag still
// must not be able to rewrite the path.
func pathEscape(s string) string { return url.PathEscape(s) }

// Time is the API's ISO-8601 instant. pydantic writes tz-aware datetimes
// as "2026-01-02T03:04:05Z" or "…+00:00" and naive ones with no offset at
// all; time.Time's own decoder refuses the last, and a null (an unset
// expiry) must decode to the zero Time rather than fail the whole answer.
type Time struct{ time.Time }

// timeLayouts are tried in order; the naive forms are read as UTC, which is
// what the control plane's clock is.
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
}

// UnmarshalJSON accepts every form the control plane has been seen to write.
func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("time %s: %w", string(b), err)
	}
	if s == "" {
		t.Time = time.Time{}
		return nil
	}
	for _, layout := range timeLayouts {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	return fmt.Errorf("time %q is not ISO-8601", s)
}

// MarshalJSON writes RFC 3339 in UTC, and null for the zero Time, so a
// decoded answer re-encodes to what the server would have sent.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339Nano))
}
