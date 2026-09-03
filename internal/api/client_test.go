package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// call is one request as the fake control plane saw it.
type call struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

// recorder keeps every call and the waits the client asked for, so a test
// can hold the client to a path, a body and a retry schedule.
type recorder struct {
	mu     sync.Mutex
	calls  []call
	sleeps []time.Duration
}

func (r *recorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{
		Method: req.Method, Path: req.URL.RequestURI(),
		Header: req.Header.Clone(), Body: string(body),
	})
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recorder) last() call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// serve starts a fake control plane behind h and a client pointed at it
// whose waits are recorded rather than slept.
func serve(t *testing.T, h http.HandlerFunc) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec.record(req)
		h(w, req)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL+"/", "vsk_testkey_0123456789")
	c.UserAgent = "veris/test"
	c.sleep = func(ctx context.Context, d time.Duration) error {
		rec.mu.Lock()
		rec.sleeps = append(rec.sleeps, d)
		rec.mu.Unlock()
		return ctx.Err()
	}
	return c, rec
}

func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestNewTrimsTheTrailingSlashAndDefaultsTheBase(t *testing.T) {
	if got := New("https://x.test/", "k").Base; got != "https://x.test" {
		t.Errorf("Base = %q", got)
	}
	c := New("", "")
	if c.Base != DefaultBase {
		t.Errorf("Base = %q, want the default", c.Base)
	}
	if c.HTTP == nil || c.HTTP.Timeout != 30*time.Second {
		t.Errorf("HTTP = %+v, want a 30 s timeout", c.HTTP)
	}
}

func TestEveryRequestCarriesTheHeaders(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `{"kind":"api_key"}`)
	})
	if _, err := c.Me(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := rec.last()
	for name, want := range map[string]string{
		"X-Api-Key":  "vsk_testkey_0123456789",
		"Accept":     "application/json",
		"User-Agent": "veris/test",
	} {
		if v := got.Header.Get(name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	if got.Header.Get("Content-Type") != "" {
		t.Errorf("a GET sent Content-Type %q", got.Header.Get("Content-Type"))
	}
	if got.Body != "" {
		t.Errorf("a GET sent a body %q", got.Body)
	}
}

// The device routes are unauthenticated by design: a client with no key must
// not send an empty header, which the control plane refuses as a bad key.
func TestNoKeyMeansNoKeyHeader(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `{"user_code":"ABCD-EFGH"}`)
	})
	c.Key = ""
	if _, err := c.DeviceCode(context.Background(), "veris"); err != nil {
		t.Fatal(err)
	}
	if _, present := rec.last().Header["X-Api-Key"]; present {
		t.Error("X-API-Key was sent with no key configured")
	}
}

// The load balancer answers 411 to a POST with no body, so a call that has
// nothing to say still says `{}`.
func TestBodilessPostAndPatchSendAnEmptyObject(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `{}`)
	})
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"revoke", func() error { _, err := c.RevokeAPIKey(ctx, "key_1"); return err }},
		{"reset sandbox", func() error { _, err := c.ResetSandbox(ctx, "env", "sb"); return err }},
		{"egress", func() error { _, err := c.EgressCredential(ctx, "env", "sb"); return err }},
		{"create sandbox defaults", func() error { _, err := c.CreateSandbox(ctx, "env", CreateSandboxRequest{}); return err }},
		{"update nothing", func() error { _, err := c.UpdateSandbox(ctx, "env", "sb", SandboxUpdate{}); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			got := rec.last()
			if got.Body != "{}" {
				t.Errorf("body = %q, want {}", got.Body)
			}
			if ct := got.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

func TestErrorDetailShapes(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		contentType string
		wantDetail  string
		wantReasons []string
		wantCode    string
		wantMsg     string
	}{
		{
			name: "string", status: 404,
			body:       `{"detail":"environment abc not found"}`,
			wantDetail: "environment abc not found",
			wantMsg:    "[404] environment abc not found",
		},
		{
			name: "list of strings", status: 422,
			body:        `{"detail":["ttl_minutes must be positive","name too long"]}`,
			wantDetail:  "ttl_minutes must be positive; name too long",
			wantReasons: []string{"ttl_minutes must be positive", "name too long"},
			wantMsg:     "[422] ttl_minutes must be positive; name too long",
		},
		{
			name: "pydantic objects", status: 422,
			body: `{"detail":[
				{"type":"string_type","loc":["body","customers",0,"email"],"msg":"must be a string","input":5},
				{"type":"missing","loc":["body","services"],"msg":"Field required"}]}`,
			wantDetail:  "customers[0].email: must be a string; services: Field required",
			wantReasons: []string{"customers[0].email: must be a string", "services: Field required"},
			wantMsg:     "[422] customers[0].email: must be a string; services: Field required",
		},
		{
			name: "pydantic object with an empty loc", status: 422,
			body:        `{"detail":[{"type":"value_error","loc":[],"msg":"unknown clock mode"}]}`,
			wantDetail:  "unknown clock mode",
			wantReasons: []string{"unknown clock mode"},
			wantMsg:     "[422] unknown clock mode",
		},
		{
			// update_sandbox's 502 names every refusing service in an object.
			name: "object of failures", status: 502,
			body:        `{"detail":{"stripe":"500 boom","beta":"timeout"}}`,
			wantDetail:  "beta: timeout; stripe: 500 boom",
			wantReasons: []string{"beta: timeout", "stripe: 500 boom"},
			wantMsg:     "[502] beta: timeout; stripe: 500 boom",
		},
		{
			name: "device token error code", status: 400,
			body:     `{"error":"authorization_pending"}`,
			wantCode: "authorization_pending",
			wantMsg:  "[400] authorization_pending",
		},
		{
			name: "not json", status: 502,
			body: "<html><body>bad gateway from the load balancer</body></html>", contentType: "text/html",
			wantDetail: "<html><body>bad gateway from the load balancer</body></html>",
			wantMsg:    "[502] <html><body>bad gateway from the load balancer</body></html>",
		},
		{
			name: "long non-json is cut at 200 bytes", status: 500,
			body:       strings.Repeat("x", 300),
			wantDetail: strings.Repeat("x", 200),
			wantMsg:    "[500] " + strings.Repeat("x", 200),
		},
		{
			name: "empty body falls back to the status text", status: 401,
			body:    "",
			wantMsg: "[401] unauthorized",
		},
		{
			name: "json without detail keeps the body", status: 409,
			body:       `{"message":"conflict"}`,
			wantDetail: `{"message":"conflict"}`,
			wantMsg:    `[409] {"message":"conflict"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			// POST: no retry, so a 5xx is seen once.
			_, err := c.DeviceToken(context.Background(), "vdc_x")
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v (%T), want *Error", err, err)
			}
			if e.Status != tc.status {
				t.Errorf("Status = %d, want %d", e.Status, tc.status)
			}
			if e.Method != http.MethodPost || !strings.HasSuffix(e.URL, "/v1/device/token") {
				t.Errorf("Method/URL = %s %s", e.Method, e.URL)
			}
			if e.Detail != tc.wantDetail {
				t.Errorf("Detail = %q, want %q", e.Detail, tc.wantDetail)
			}
			if !reflect.DeepEqual(e.Reasons, tc.wantReasons) {
				t.Errorf("Reasons = %q, want %q", e.Reasons, tc.wantReasons)
			}
			if e.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", e.Code, tc.wantCode)
			}
			if string(e.Body) != tc.body {
				t.Errorf("Body = %q, want the raw answer", e.Body)
			}
			if e.Error() != tc.wantMsg {
				t.Errorf("Error() = %q, want %q", e.Error(), tc.wantMsg)
			}
			if !IsStatus(err, tc.status) || IsStatus(err, tc.status+1) {
				t.Error("IsStatus does not match on the status")
			}
			wrapped := fmt.Errorf("outer: %w", err)
			if !IsStatus(wrapped, tc.status) {
				t.Error("IsStatus does not see through a wrap")
			}
		})
	}
	if IsStatus(errors.New("plain"), 500) || IsStatus(nil, 500) {
		t.Error("IsStatus matched a non-*Error")
	}
}

func TestGetRetriesOn5xxHonouringRetryAfter(t *testing.T) {
	var n int
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		switch n {
		case 1:
			w.Header().Set("Retry-After", "7")
			respond(w, 503, `{"detail":"warming up"}`)
		case 2:
			respond(w, 502, "gateway hiccup")
		default:
			respond(w, 200, `{"status":"ok","checkout":"abc"}`)
		}
	})
	h, err := c.Healthz(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" {
		t.Errorf("Status = %q", h.Status)
	}
	if rec.count() != 3 {
		t.Errorf("requests = %d, want 3", rec.count())
	}
	// Retry-After replaced the first scheduled wait; the second used the
	// schedule's next step.
	want := []time.Duration{7 * time.Second, time.Second}
	if !reflect.DeepEqual(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
}

// A Retry-After of an hour is honoured only up to the cap: the command
// should surface the 503, not hang for an hour with nothing said.
func TestRetryAfterIsCapped(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		respond(w, 503, `{"detail":"maintenance"}`)
	})
	_, err := c.ListEnvironments(context.Background())
	if !IsStatus(err, 503) {
		t.Fatalf("err = %v, want a 503 *Error", err)
	}
	want := []time.Duration{maxRetryAfter, maxRetryAfter}
	if !reflect.DeepEqual(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
}

// A 2xx with no body where a record was expected is not a zero-valued
// record: GetSandbox on an empty 200 must fail rather than hand WaitSandbox
// a sandbox whose status is "" to poll until its deadline.
func TestAnEmpty2xxBodyIsADecodeError(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	_, err := c.GetSandbox(context.Background(), "sbx_1")
	if err == nil || err.Error() != "decode GET /v1/sandboxes/sbx_1: empty body" {
		t.Errorf("err = %v, want the empty-body decode error", err)
	}
}

func TestGetGivesUpAfterThreeAttempts(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "not a number")
		respond(w, 503, `{"detail":"still down"}`)
	})
	_, err := c.ListEnvironments(context.Background())
	if !IsStatus(err, 503) {
		t.Fatalf("err = %v, want a 503 *Error", err)
	}
	if rec.count() != maxGetAttempts {
		t.Errorf("requests = %d, want %d", rec.count(), maxGetAttempts)
	}
	// An unparseable Retry-After falls back to the schedule.
	want := []time.Duration{500 * time.Millisecond, time.Second}
	if !reflect.DeepEqual(rec.sleeps, want) {
		t.Errorf("sleeps = %v, want %v", rec.sleeps, want)
	}
}

func TestGetDoesNotRetryA4xx(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 404, `{"detail":"sandbox x not found"}`)
	})
	_, err := c.GetSandbox(context.Background(), "x")
	if !IsStatus(err, 404) {
		t.Fatalf("err = %v", err)
	}
	if rec.count() != 1 || len(rec.sleeps) != 0 {
		t.Errorf("requests = %d, sleeps = %v; a 404 must not be retried", rec.count(), rec.sleeps)
	}
}

// A POST that provisioned something and then answered 503 may well have
// done its work; sending it again would do it twice.
func TestPostIsSentOnce(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		respond(w, 503, `{"detail":"images are not configured on this deployment"}`)
	})
	_, err := c.PromoteSandbox(context.Background(), "env", "sb", PromoteRequest{})
	if !IsStatus(err, 503) {
		t.Fatalf("err = %v", err)
	}
	if rec.count() != 1 || len(rec.sleeps) != 0 {
		t.Errorf("requests = %d, sleeps = %v; a POST must be sent once", rec.count(), rec.sleeps)
	}
	// DELETE is not idempotent from the caller's view either (the second
	// answers 404), so it is sent once as well.
	if err := c.DeleteSandbox(context.Background(), "env", "sb"); !IsStatus(err, 503) || rec.count() != 2 {
		t.Errorf("DELETE: err = %v, requests = %d", err, rec.count())
	}
}

// failOnce is a transport that drops the first request on the floor.
type failOnce struct {
	next   http.RoundTripper
	failed bool
}

func (f *failOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	if !f.failed {
		f.failed = true
		return nil, errors.New("connection reset by peer")
	}
	return f.next.RoundTrip(req)
}

func TestGetRetriesATransportError(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `[]`)
	})
	c.HTTP = &http.Client{Transport: &failOnce{next: http.DefaultTransport}}
	envs, err := c.ListEnvironments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if envs == nil || len(envs) != 0 {
		t.Errorf("envs = %#v, want an empty list", envs)
	}
	if rec.count() != 1 || !reflect.DeepEqual(rec.sleeps, []time.Duration{500 * time.Millisecond}) {
		t.Errorf("requests reaching the server = %d, sleeps = %v", rec.count(), rec.sleeps)
	}
}

func TestTransportErrorNamesTheControlPlane(t *testing.T) {
	c := New("http://127.0.0.1:1", "k")
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	_, err := c.DeviceCode(context.Background(), "veris")
	if err == nil || !strings.Contains(err.Error(), "cannot reach the control plane at http://127.0.0.1:1") {
		t.Errorf("err = %v", err)
	}
	var e *Error
	if errors.As(err, &e) {
		t.Error("a transport failure is not an *Error")
	}
}

func TestACancelledContextEndsTheRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		respond(w, 503, `{"detail":"down"}`)
	})
	_, err := c.ListSnapshots(ctx, "env")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if rec.count() != 1 {
		t.Errorf("requests = %d, want 1", rec.count())
	}
}

func TestA204IsSuccessWithNothingToDecode(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
	if err := c.DeleteEnvironment(context.Background(), "env_1"); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); got.Method != http.MethodDelete || got.Path != "/v1/environments/env_1" {
		t.Errorf("sent %s %s", got.Method, got.Path)
	}
}

func TestAMalformedAnswerIsADecodeError(t *testing.T) {
	c, _ := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `{"id":`)
	})
	_, err := c.GetEnvironment(context.Background(), "env_1")
	if err == nil || !strings.HasPrefix(err.Error(), "decode GET /v1/environments/env_1:") {
		t.Errorf("err = %v", err)
	}
}

func TestIdsAreEscapedInPaths(t *testing.T) {
	c, rec := serve(t, func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, `{}`)
	})
	if _, err := c.GetSandbox(context.Background(), "a/b?c"); err != nil {
		t.Fatal(err)
	}
	if got := rec.last().Path; got != "/v1/sandboxes/a%2Fb%3Fc" {
		t.Errorf("path = %q", got)
	}
}

func TestTimeDecodesEveryFormTheControlPlaneWrites(t *testing.T) {
	utc := time.Date(2026, 9, 3, 12, 30, 15, 0, time.UTC)
	cases := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{`"2026-09-03T12:30:15Z"`, utc, false},
		{`"2026-09-03T12:30:15+00:00"`, utc, false},
		{`"2026-09-03T12:30:15.123456+00:00"`, utc.Add(123456 * time.Microsecond), false},
		{`"2026-09-03T12:30:15.123456Z"`, utc.Add(123456 * time.Microsecond), false},
		{`"2026-09-03T14:30:15+02:00"`, utc, false},
		{`"2026-09-03T12:30:15"`, utc, false},
		{`"2026-09-03T12:30:15.5"`, utc.Add(500 * time.Millisecond), false},
		{`"2026-09-03 12:30:15+00:00"`, utc, false},
		{`null`, time.Time{}, false},
		{`""`, time.Time{}, false},
		{`"yesterday"`, time.Time{}, true},
		{`12345`, time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			var got struct {
				At Time `json:"at"`
			}
			err := json.Unmarshal([]byte(`{"at":`+tc.in+`}`), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decoded %v, want an error", got.At)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.At.Equal(tc.want) {
				t.Errorf("got %v, want %v", got.At.Time, tc.want)
			}
			if tc.want.IsZero() != got.At.IsZero() {
				t.Errorf("IsZero = %v", got.At.IsZero())
			}
		})
	}
}

func TestTimeMarshalsRFC3339OrNull(t *testing.T) {
	b, err := json.Marshal(struct {
		Set   Time `json:"set"`
		Unset Time `json:"unset"`
	}{Set: Time{time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))}})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"set":"2026-01-02T02:04:05Z","unset":null}`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestSandboxUpdateSendsExactlyWhatItNames(t *testing.T) {
	url := "https://3000-abc.e2b.app"
	cases := []struct {
		name string
		u    SandboxUpdate
		want string
	}{
		{"nothing", SandboxUpdate{}, `{}`},
		{"set", SandboxUpdate{ClientBaseURL: &url}, `{"client_base_url":"https://3000-abc.e2b.app"}`},
		{"clear", SandboxUpdate{Clear: true}, `{"client_base_url":null}`},
		{"clear wins over a value", SandboxUpdate{ClientBaseURL: &url, Clear: true}, `{"client_base_url":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.u)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("got %s, want %s", b, tc.want)
			}
		})
	}
}
