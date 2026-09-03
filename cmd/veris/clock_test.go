package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-cli/internal/api"
	"github.com/veris-ai/veris-cli/internal/cfg"
)

// clockPlane is a control plane with the sandbox record the clock verbs
// resolve the environment from, and the clock route itself. The PATCH
// answer is scripted; every PATCH body is kept.
type clockPlane struct {
	srv *httptest.Server
	mu  sync.Mutex

	clock       api.SandboxClock
	warnings    []string
	patchStatus int // 0 → 200 with the scripted clock
	patchBody   any // a 4xx's body when patchStatus is set
	patches     []string
	gets        int
}

func newClockPlane(t *testing.T) *clockPlane {
	t.Helper()
	p := &clockPlane{clock: api.SandboxClock{ID: 1, Mode: api.ClockModeLive}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != sbID {
			sbJSON(w, 404, map[string]string{"detail": "sandbox " + r.PathValue("id") + " not found"})
			return
		}
		sbJSON(w, 200, readySandbox(nil, time.Now().Add(time.Hour)))
	})
	path := "/v1/environments/{env}/sandboxes/{id}/clock"
	mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if r.PathValue("env") != ciID || r.PathValue("id") != sbID {
			sbJSON(w, 404, map[string]string{"detail": "sandbox " + r.PathValue("id") + " not found"})
			return
		}
		p.gets++
		sbJSON(w, 200, p.clock)
	})
	mux.HandleFunc("PATCH "+path, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var body json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		p.patches = append(p.patches, string(body))
		if p.patchStatus != 0 {
			sbJSON(w, p.patchStatus, p.patchBody)
			return
		}
		// The fake applies the fields the way the world does: only what
		// was sent changes.
		var fields struct {
			Mode          *string `json:"mode"`
			OffsetSeconds *int64  `json:"offset_seconds"`
			FrozenTime    *int64  `json:"frozen_time"`
		}
		_ = json.Unmarshal(body, &fields)
		if fields.Mode != nil {
			p.clock.Mode = *fields.Mode
		}
		if fields.OffsetSeconds != nil {
			p.clock.OffsetSeconds = *fields.OffsetSeconds
		}
		if strings.Contains(string(body), `"frozen_time"`) {
			p.clock.FrozenTime = fields.FrozenTime
		}
		sbJSON(w, 200, api.SetSandboxClockResponse{Clock: p.clock, Warnings: p.warnings})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		sbJSON(w, 404, map[string]string{"detail": "Not Found"})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *clockPlane) script(fn func(p *clockPlane)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fn(p)
}

func (p *clockPlane) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.patches...)
}

// clockBench is a logged-in bench whose folder points at sbID in ci.
func clockBench(t *testing.T, plane *clockPlane) *bench {
	t.Helper()
	b := sandboxBench(t, plane.srv.URL)
	b.twoEnvs()
	b.local(cfg.Local{Sandbox: &cfg.SandboxRef{ID: sbID, EnvironmentID: ciID}})
	return b
}

func int64p(n int64) *int64 { return &n }

const march1 = int64(1772355600) // 2026-03-01T09:00:00Z

func TestSandboxClockGet(t *testing.T) {
	plane := newClockPlane(t)
	clockBench(t, plane)

	t.Run("frozen", func(t *testing.T) {
		plane.script(func(p *clockPlane) {
			p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeFrozen, FrozenTime: int64p(march1)}
		})
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "clock")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		sbInOrder(t, stderr, "Clock  frozen at 2026-03-01T09:00:00Z", "! Deliveries paused while the clock is frozen")
	})

	t.Run("live with an offset", func(t *testing.T) {
		plane.script(func(p *clockPlane) {
			p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeLive, OffsetSeconds: 7 * 86400}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "clock", "--id", sbID)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		if !strings.Contains(stderr, "Clock  live (+7d)\n") || strings.Contains(stderr, "paused") {
			t.Errorf("stderr:\n%s", stderr)
		}
	})

	t.Run("live at real time", func(t *testing.T) {
		plane.script(func(p *clockPlane) { p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeLive, OffsetSeconds: -5400} })
		code, _, stderr := runSandboxCLI(t, "sandbox", "clock")
		if code != 0 || !strings.Contains(stderr, "Clock  live (-1h30m)\n") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
		plane.script(func(p *clockPlane) { p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeLive} })
		code, _, stderr = runSandboxCLI(t, "sandbox", "clock")
		if code != 0 || !strings.Contains(stderr, "Clock  live\n") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		plane.script(func(p *clockPlane) {
			p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeFrozen, FrozenTime: int64p(march1)}
		})
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "clock", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		var got api.SandboxClock
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("stdout is not a clock: %v\n%s", err, stdout)
		}
		if got.Mode != api.ClockModeFrozen || got.FrozenTime == nil || *got.FrozenTime != march1 {
			t.Errorf("got %+v", got)
		}
		if strings.Contains(stderr, "Clock  ") {
			t.Errorf("the human line printed under --json:\n%s", stderr)
		}
	})

	t.Run("no sandbox", func(t *testing.T) {
		code, _, stderr := runSandboxCLI(t, "sandbox", "clock", "--id", otherSbID)
		if code != 1 || !strings.Contains(stderr, "✗ Failed to read sandbox "+otherSbID+": [404]") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
	})
}

func TestSandboxClockSet(t *testing.T) {
	t.Run("freeze-at enters frozen and says what that pauses", func(t *testing.T) {
		plane := newClockPlane(t)
		clockBench(t, plane)
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "clock", "set", "--freeze-at", "2026-03-01T09:00:00Z", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout:\n%s", stdout)
		}
		sbInOrder(t, stderr,
			"Freeze the clock at 2026-03-01T09:00:00Z of sandbox "+sbID+" (now live)? y",
			"! The clock is now frozen: outbound webhook delivery is paused until veris sandbox clock set --live",
			"✓ Clock frozen at 2026-03-01T09:00:00Z (was live, ",
		)
		if got := plane.sent(); len(got) != 1 || got[0] != `{"frozen_time":1772355600,"mode":"frozen"}` {
			t.Errorf("sent %q", got)
		}
	})

	t.Run("a bare unix second, the server's warning, and a frozen clock moved", func(t *testing.T) {
		plane := newClockPlane(t)
		clockBench(t, plane)
		plane.script(func(p *clockPlane) {
			p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeFrozen, FrozenTime: int64p(march1 + 86400)}
			p.warnings = []string{"clock: time moved backwards (1772442000 -> 1772355600). Data created before this change now lies in the future, which can confuse the code under test."}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "clock", "set", "--freeze-at", "1772355600", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"(now frozen at 2026-03-02T09:00:00Z)? y",
			"! Server: clock: time moved backwards (1772442000 -> 1772355600).",
			"✓ Clock frozen at 2026-03-01T09:00:00Z (was frozen at 2026-03-02T09:00:00Z)",
		)
		// Already frozen: the "now frozen" line is for entering the mode.
		if strings.Contains(stderr, "The clock is now frozen") {
			t.Errorf("a clock that was already frozen announced freezing:\n%s", stderr)
		}
	})

	t.Run("offset runs live from real time plus the offset", func(t *testing.T) {
		plane := newClockPlane(t)
		clockBench(t, plane)
		plane.script(func(p *clockPlane) {
			p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeFrozen, FrozenTime: int64p(march1)}
		})
		code, stdout, stderr := runSandboxCLI(t, "sandbox", "clock", "set", "--offset", "+1w", "--yes", "--json")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr,
			"Set the clock live at real time +7d of sandbox "+sbID+" (now frozen at 2026-03-01T09:00:00Z)? y",
			"✓ Clock live (+7d) (was frozen at 2026-03-01T09:00:00Z)",
		)
		if got := plane.sent(); len(got) != 1 || got[0] != `{"frozen_time":null,"mode":"live","offset_seconds":604800}` {
			t.Errorf("sent %q", got)
		}
		var res api.SetSandboxClockResponse
		if err := json.Unmarshal([]byte(stdout), &res); err != nil {
			t.Fatalf("stdout is not the response: %v\n%s", err, stdout)
		}
		if res.Clock.Mode != api.ClockModeLive || res.Clock.OffsetSeconds != 604800 || res.Clock.FrozenTime != nil {
			t.Errorf("clock %+v", res.Clock)
		}
	})

	t.Run("live returns to real time", func(t *testing.T) {
		plane := newClockPlane(t)
		clockBench(t, plane)
		plane.script(func(p *clockPlane) {
			p.clock = api.SandboxClock{ID: 1, Mode: api.ClockModeLive, OffsetSeconds: -36 * 3600}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "clock", "set", "--live", "--yes")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "Set the clock live at real time of sandbox "+sbID+" (now live (-1d12h))? y", "✓ Clock live (was live (-1d12h), ")
		if got := plane.sent(); len(got) != 1 || got[0] != `{"frozen_time":null,"mode":"live","offset_seconds":0}` {
			t.Errorf("sent %q", got)
		}
	})

	t.Run("refused by the world", func(t *testing.T) {
		plane := newClockPlane(t)
		clockBench(t, plane)
		plane.script(func(p *clockPlane) {
			p.patchStatus = 422
			p.patchBody = map[string]any{"detail": []string{"clock: mode=frozen requires frozen_time to be set"}}
		})
		code, _, stderr := runSandboxCLI(t, "sandbox", "clock", "set", "--offset", "-2d", "--yes")
		if code != 1 {
			t.Fatalf("exit %d, want 1:\n%s", code, stderr)
		}
		sbInOrder(t, stderr, "✗ Failed to set clock of sandbox "+sbID+": [422]", "  clock: mode=frozen requires frozen_time to be set")
	})

	t.Run("needs --yes off a TTY", func(t *testing.T) {
		plane := newClockPlane(t)
		clockBench(t, plane)
		code, _, stderr := runSandboxCLI(t, "sandbox", "clock", "set", "--live")
		if code != 1 || !strings.Contains(stderr, "Pass --yes instead") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
		if got := plane.sent(); len(got) != 0 {
			t.Errorf("a declined change was sent: %q", got)
		}
	})

	t.Run("usage", func(t *testing.T) {
		plane := newClockPlane(t)
		clockBench(t, plane)
		cases := []struct {
			args []string
			want string
		}{
			{nil, "✗ sandbox clock set takes exactly one of --freeze-at, --offset or --live"},
			{[]string{"--live", "--offset", "1h"}, "✗ sandbox clock set takes exactly one of --freeze-at, --offset or --live"},
			{[]string{"--freeze-at", "yesterday"}, "✗ --freeze-at: 'yesterday' is not RFC 3339 (2026-03-01T09:00:00Z) or a Unix second"},
			{[]string{"--offset", "7"}, "✗ --offset: '7' is not a duration (try +7d, -36h or 1w)"},
			{[]string{"--offset", "next week"}, "✗ --offset: 'next week' is not a duration (try +7d, -36h or 1w)"},
		}
		for _, tc := range cases {
			code, _, stderr := runSandboxCLI(t, append([]string{"sandbox", "clock", "set", "--yes"}, tc.args...)...)
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Errorf("%q: exit %d, stderr:\n%s", tc.args, code, stderr)
			}
		}
		if got := plane.sent(); len(got) != 0 {
			t.Errorf("a refused request was sent: %q", got)
		}
	})
}

func TestSandboxClockParseOffset(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"+7d", 7 * 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"-36h", -36 * time.Hour, true},
		{"1w", 7 * 24 * time.Hour, true},
		{"-2w", -14 * 24 * time.Hour, true},
		{"1d12h30m", 36*time.Hour + 30*time.Minute, true},
		{"1.5d", 36 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"45s", 45 * time.Second, true},
		{"0", 0, true},
		{"", 0, false},
		{"+", 0, false},
		{"7", 0, false},
		{"d", 0, false},
		{"1y", 0, false},
		{"7 d", 0, false},
	}
	for _, tc := range cases {
		got, err := parseOffset(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("parseOffset(%q): err %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if got != tc.want {
			t.Errorf("parseOffset(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestSandboxClockFormatOffset(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{604800, "+7d"}, {-129600, "-1d12h"}, {5400, "+1h30m"}, {45, "+45s"}, {0, "+0s"}, {86401, "+1d1s"},
	}
	for _, tc := range cases {
		if got := formatOffset(tc.secs); got != tc.want {
			t.Errorf("formatOffset(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

func TestSandboxClockParseFreezeAt(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"2026-03-01T09:00:00Z", march1, true},
		{"2026-03-01T10:00:00+01:00", march1, true},
		{"1772355600", march1, true},
		{"-1", 0, false},
		{"2026-03-01", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, err := parseFreezeAt(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseFreezeAt(%q) = %d, %v; want %d, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}
