package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veris-ai/veris-proxy/internal/api"
	"github.com/veris-ai/veris-proxy/internal/cfg"
	"github.com/veris-ai/veris-proxy/internal/cli"
)

const (
	orgID      = "org_7f3k2m9x1p5q8r4t6v0w2y"
	userCode   = "8FZS-YQKQ"
	deviceCode = "vdc_polling_secret"
	console    = "https://studio.example"
	// The key the pairing mints, and the one CI hands over on stdin. Both
	// are long enough that MaskKey shows the 12-character prefix the list
	// route matches on.
	mintedKey = "vsk_mi4pa0uo7c9d2e4f6g8h0j1k3l5m7n9p"
	ciKey     = "vsk_ci0f7k3mz1x2c3v4b5n6m7a8s9d0f1g2"
	mintedID  = "key_b8bx3hw6bx929g7eg1hmo"
	ciKeyID   = "key_q2w9x4v7b1n5m8k2j6h0g3f"
)

// tokenStep is one scripted answer of POST /v1/device/token: an RFC 8628
// error code, or the redemption.
type tokenStep struct {
	code string
	tok  *api.DeviceTokenResponse
}

// fakePlane is a control plane serving the identity routes with scripted
// answers, and remembering what it was asked.
type fakePlane struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// scripting
	codeStatus   int         // POST /v1/device/code: 0 is 200
	codeDetail   string      // its detail when refused
	expiresIn    int         // 0 means 900
	tokens       []tokenStep // consumed in order; the last repeats
	dropToken    bool        // close the connection instead of answering the poll
	meStatus     int         // GET /v1/me: 0 is 200
	me           api.Me
	meRaw        []byte // when set, GET /v1/me's body verbatim
	keys         []api.APIKey
	revokeStatus int // POST …/revoke: 0 is 200
	rec          record
}

// record is what the fake was asked, read through seen() so a handler still
// finishing on its own goroutine (the dropped connection) cannot race the
// test's look.
type record struct {
	codeCalls  int
	clientName string
	polls      int
	meKeys     []string // X-API-Key on every /v1/me
	meOnDisk   []bool   // per /v1/me: whether the profiles file already held that key
	listCalls  int
	revoked    []string // key ids
	revokeKeys []string // X-API-Key on every revoke
}

func (f *fakePlane) seen() record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rec
}

func newFakePlane(t *testing.T) *fakePlane {
	t.Helper()
	f := &fakePlane{t: t, expiresIn: 900}
	f.me = api.Me{
		Kind:           "api_key",
		OrganizationID: orgID,
		Organizations:  []api.Organization{{ID: orgID, Name: "Acme", Slug: "acme", Kind: "team"}},
	}
	f.keys = []api.APIKey{
		{ID: mintedID, Name: "veris on victor-mbp · device", KeyPrefix: mintedKey[:12], Status: "active"},
		{ID: ciKeyID, Name: "ci · github", KeyPrefix: ciKey[:12], Status: "active"},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePlane) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	answer := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	refuse := func(status int, detail string) { answer(status, map[string]string{"detail": detail}) }
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/device/code":
		f.rec.codeCalls++
		var req api.DeviceCodeRequest
		_ = json.Unmarshal(body, &req)
		f.rec.clientName = req.ClientName
		if f.codeStatus != 0 {
			refuse(f.codeStatus, f.codeDetail)
			return
		}
		answer(200, api.DeviceCodeResponse{
			UserCode: userCode, DeviceCode: deviceCode,
			VerificationURL:         console + "/connect",
			VerificationURLComplete: console + "/connect?code=" + userCode,
			ExpiresIn:               f.expiresIn, Interval: 5,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/device/token":
		f.rec.polls++
		if f.dropToken {
			// The transport failure: nothing answers, the connection dies.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		var req api.DeviceTokenRequest
		_ = json.Unmarshal(body, &req)
		if req.DeviceCode != deviceCode {
			answer(400, map[string]string{"error": "invalid_grant"})
			return
		}
		step := f.tokens[min(f.rec.polls, len(f.tokens))-1]
		if step.tok != nil {
			answer(200, step.tok)
			return
		}
		answer(400, map[string]string{"error": step.code})
	case r.Method == http.MethodGet && r.URL.Path == "/v1/me":
		f.rec.meKeys = append(f.rec.meKeys, r.Header.Get("X-API-Key"))
		onDisk, _ := os.ReadFile(cfg.GlobalPath())
		f.rec.meOnDisk = append(f.rec.meOnDisk, strings.Contains(string(onDisk), r.Header.Get("X-API-Key")))
		if f.meStatus != 0 {
			refuse(f.meStatus, "me is broken")
			return
		}
		if r.Header.Get("X-API-Key") == "" {
			refuse(401, "invalid or missing API key")
			return
		}
		if f.meRaw != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write(f.meRaw)
			return
		}
		answer(200, f.me)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/api-keys":
		f.rec.listCalls++
		answer(200, f.keys)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/api-keys/") && strings.HasSuffix(r.URL.Path, "/revoke"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/api-keys/"), "/revoke")
		f.rec.revoked = append(f.rec.revoked, id)
		f.rec.revokeKeys = append(f.rec.revokeKeys, r.Header.Get("X-API-Key"))
		if f.revokeStatus != 0 {
			refuse(f.revokeStatus, "not found")
			return
		}
		answer(200, api.APIKey{ID: id, Status: "revoked"})
	default:
		refuse(404, "Not Found")
	}
}

// minted is the redemption the fake hands out.
func minted() *api.DeviceTokenResponse {
	return &api.DeviceTokenResponse{
		APIKey: mintedKey, OrganizationID: orgID, KeyID: mintedID,
		KeyName: "veris on victor-mbp · device",
	}
}

// fastPolls replaces the poll's sleep with one that records what was asked
// and returns at once, so a fifteen minute flow runs in a blink.
func fastPolls(t *testing.T) *[]time.Duration {
	t.Helper()
	var sleeps []time.Duration
	prev := pollSleep
	pollSleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	t.Cleanup(func() { pollSleep = prev })
	return &sleeps
}

// noBrowser fails the test if the flow tries to open one: every test here
// runs off a terminal, where it must not.
func noBrowser(t *testing.T) {
	t.Helper()
	prev := openBrowser
	openBrowser = func(url string) error {
		t.Errorf("openBrowser(%q) called off a TTY", url)
		return nil
	}
	t.Cleanup(func() { openBrowser = prev })
}

// feedStdin is what the command reads as stdin.
func feedStdin(t *testing.T, s string) {
	t.Helper()
	prev := stdin
	stdin = strings.NewReader(s)
	t.Cleanup(func() { stdin = prev })
}

// forceTTY makes every session built during the test believe stdin is a
// terminal, so prompts run against the fed stdin.
func forceTTY(t *testing.T) {
	t.Helper()
	prev := newSessionHook
	newSessionHook = func(s *session) { s.ui.TTY = true }
	t.Cleanup(func() { newSessionHook = prev })
}

// veris runs one command line through the tree, the way main does, and
// returns stdout, stderr (with main's own report appended) and the exit
// status.
func veris(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var so, se bytes.Buffer
	err := cli.Execute(root(), &cli.Globals{}, args, &so, &se)
	code = exitStatusTo(&se, err)
	return so.String(), se.String(), code
}

// loginBench is a fresh machine (no profiles file) and a fake plane that
// VERIS_API_BASE points at, so a bare `veris login` pairs with the fake.
func loginBench(t *testing.T) (*bench, *fakePlane) {
	t.Helper()
	b := newBench(t)
	f := newFakePlane(t)
	t.Setenv(cfg.EnvAPIBase, f.srv.URL)
	noBrowser(t)
	return b, f
}

// savedProfile reads the profiles file back.
func savedProfile(t *testing.T, name string) (*cfg.Global, cfg.Profile) {
	t.Helper()
	g, err := cfg.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := g.Profiles[name]
	if !ok {
		t.Fatalf("profile %q not in %s (have %v)", name, g.Path, profileNames(g))
	}
	return g, p
}

func noProfilesFile(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(cfg.GlobalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists (stat: %v); nothing should have been saved", cfg.GlobalPath(), err)
	}
}

func expectEqual(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n--- got ---\n%s--- want ---\n%s", what, got, want)
	}
}

func expectContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("%s lacks %q:\n%s", what, w, got)
		}
	}
}

// localWarn is the first line of every device flow here: the fake plane
// is on loopback, which is exactly the case the warning exists for.
const localWarn = "! A local plane usually has nobody who can approve a pairing (approval needs a login session); use --key-stdin\n"

func TestLoginDeviceFlowSavesTheKeyAndReportsIt(t *testing.T) {
	b, f := loginBench(t)
	f.tokens = []tokenStep{{code: "authorization_pending"}, {code: "authorization_pending"}, {tok: minted()}}
	sleeps := fastPolls(t)

	stdout, stderr, code := veris(t, "login")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty, got %q", stdout)
	}
	path := cfg.GlobalPath()
	expectEqual(t, "stderr", stderr, localWarn+
		"Pairing this machine with Veris\n"+
		"  Profile  default\n"+
		"  API      "+f.srv.URL+"\n"+
		"  Client   "+clientName()+"\n"+
		"\n"+
		"  Open   "+console+"/connect?code="+userCode+"\n"+
		"  Code   "+userCode+"\n"+
		"\n"+
		"Waiting for approval in the browser · expires in 15:00 · polling every 5 s\n"+
		"✓ Approved for Acme ("+orgID+")\n"+
		"✓ Logged in as key 'veris on victor-mbp · device' ("+mintedID+", vsk_mi4pa0uo…)\n"+
		"✓ API key saved to "+path+" (profile 'default', mode 0600)\n"+
		"→ "+console+"/overview\n"+
		"→ Next: veris env create\n")

	// Every wait is the server's interval plus a second of slack.
	if want := []time.Duration{6 * time.Second, 6 * time.Second, 6 * time.Second}; fmt.Sprint(*sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps %v, want %v", *sleeps, want)
	}
	if f.seen().polls != 3 || f.seen().codeCalls != 1 {
		t.Errorf("polls %d (want 3), code mints %d (want 1)", f.seen().polls, f.seen().codeCalls)
	}
	if f.seen().clientName != clientName() || !strings.HasPrefix(f.seen().clientName, "veris") {
		t.Errorf("client_name %q", f.seen().clientName)
	}
	if len(f.seen().meKeys) != 1 || f.seen().meKeys[0] != mintedKey {
		t.Errorf("/v1/me was asked with keys %q, want the minted one once", f.seen().meKeys)
	}

	g, p := savedProfile(t, "default")
	want := cfg.Profile{APIBase: f.srv.URL, APIKey: mintedKey, ConsoleURL: console,
		KeyID: mintedID, KeyName: "veris on victor-mbp · device", OrganizationID: orgID, OrganizationName: "Acme"}
	if p != want {
		t.Errorf("saved profile %+v, want %+v", p, want)
	}
	if g.ActiveProfile != "default" {
		t.Errorf("active_profile %q, want default", g.ActiveProfile)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("%s mode %v (err %v), want 0600", path, info.Mode(), err)
	}
	if !strings.Contains(path, b.home) {
		t.Errorf("profiles file %s is not under the temp HOME %s", path, b.home)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "api_key: "+mintedKey) {
		t.Errorf("file does not carry the key:\n%s", raw)
	}
}

func TestLoginWritesTheProfileBeforeAskingWhoItIs(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{tok: minted()}}
	f.meStatus = 500
	fastPolls(t)

	_, stderr, code := veris(t, "login", "--profile", "dev")
	if code != 0 {
		t.Fatalf("exit %d: a broken /v1/me must not lose the key\n%s", code, stderr)
	}
	g, p := savedProfile(t, "dev")
	if p.APIKey != mintedKey || p.KeyID != mintedID || p.OrganizationID != orgID {
		t.Errorf("saved %+v", p)
	}
	if g.ActiveProfile != "dev" {
		t.Errorf("active_profile %q, want dev", g.ActiveProfile)
	}
	expectContains(t, "stderr", stderr,
		"! Could not read the organisation's name ([500] me is broken); the key is saved regardless\n",
		"✓ Approved for organisation "+orgID+"\n",
		"✓ Active profile switched to 'dev'\n")
	if len(f.seen().meKeys) == 0 || f.seen().meKeys[0] != mintedKey {
		t.Errorf("/v1/me keys %q", f.seen().meKeys)
	}
	// Not just the end state: the file already held the key when /v1/me
	// was asked, so a reordering to Me-then-save would be caught here.
	if len(f.seen().meOnDisk) == 0 || !f.seen().meOnDisk[0] {
		t.Errorf("the profile was not on disk when /v1/me was called: %v", f.seen().meOnDisk)
	}
}

func TestLoginSlowDownReArmsForTheRestOfTheFlow(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{code: "slow_down"}, {code: "authorization_pending"}, {tok: minted()}}
	sleeps := fastPolls(t)

	_, stderr, code := veris(t, "login")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	expectContains(t, "stderr", stderr,
		"! The control plane asked us to slow down; polling every 10 s now.\n"+
			"Waiting for approval in the browser · expires in 15:00 · polling every 10 s\n")
	if want := []time.Duration{6 * time.Second, 11 * time.Second, 11 * time.Second}; fmt.Sprint(*sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps %v, want %v", *sleeps, want)
	}
}

func TestLoginSettledPairingsSaveNothing(t *testing.T) {
	cases := []struct {
		name      string
		steps     []tokenStep
		expiresIn int
		want      string
		polls     int
	}{
		{name: "denied", steps: []tokenStep{{code: "authorization_pending"}, {code: "access_denied"}},
			want: "✗ Pairing " + userCode + " was denied on the console. Nothing was saved.\n", polls: 2},
		{name: "expired on the server", steps: []tokenStep{{code: "expired_token"}},
			want: "✗ Pairing " + userCode + " expired after 15:00 without approval. Run veris login again.\n", polls: 1},
		{name: "expired by the local deadline", steps: []tokenStep{{code: "authorization_pending"}}, expiresIn: -1,
			want: "✗ Pairing " + userCode + " expired after 00:00 without approval. Run veris login again.\n", polls: 0},
		{name: "forgotten", steps: []tokenStep{{code: "invalid_grant"}},
			want: "✗ The control plane no longer recognises this pairing (invalid_grant). Run veris login again.\n", polls: 1},
		{name: "an unexpected refusal", steps: []tokenStep{{code: "something_else"}},
			want: "✗ Failed to poll the pairing: [400] something_else\n", polls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, f := loginBench(t)
			f.tokens = tc.steps
			if tc.expiresIn != 0 {
				f.expiresIn = 0
			}
			fastPolls(t)
			_, stderr, code := veris(t, "login")
			if code != 1 {
				t.Errorf("exit %d, want 1", code)
			}
			if !strings.HasSuffix(stderr, tc.want) {
				t.Errorf("stderr does not end with %q:\n%s", tc.want, stderr)
			}
			if f.seen().polls != tc.polls {
				t.Errorf("polls %d, want %d", f.seen().polls, tc.polls)
			}
			noProfilesFile(t)
		})
	}
}

func TestLoginRefusedAtTheMint(t *testing.T) {
	t.Run("no device login", func(t *testing.T) {
		_, f := loginBench(t)
		f.codeStatus, f.codeDetail = 404, "Not Found"
		_, stderr, code := veris(t, "login", "--profile", "ci")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		expectEqual(t, "stderr", stderr, localWarn+
			"✗ This control plane has no device login ("+f.srv.URL+"/v1/device/code answered 404).\n"+
			"→ Next: veris login --key-stdin --profile ci\n")
		noProfilesFile(t)
	})
	t.Run("too many pairings", func(t *testing.T) {
		_, f := loginBench(t)
		f.codeStatus = 429
		f.codeDetail = "too many outstanding pairing requests; try again in a few minutes"
		_, stderr, code := veris(t, "login")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		expectEqual(t, "stderr", stderr, localWarn+
			"✗ Failed to start login: [429] too many outstanding pairing requests; try again in a few minutes\n")
		if f.seen().codeCalls != 1 || f.seen().polls != 0 {
			t.Errorf("mints %d polls %d: a 429 is not retried", f.seen().codeCalls, f.seen().polls)
		}
		noProfilesFile(t)
	})
}

func TestLoginGivesUpAfterTwelveTransportFailures(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{tok: minted()}}
	f.dropToken = true
	fastPolls(t)

	_, stderr, code := veris(t, "login")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	for k := 1; k < maxPollFailures; k++ {
		expectContains(t, "stderr", stderr,
			fmt.Sprintf("! Could not reach the control plane; retrying in 5 s (%d of 12)\n", k))
	}
	if strings.Contains(stderr, "(12 of 12)") {
		t.Errorf("the twelfth failure is the end, not a retry:\n%s", stderr)
	}
	expectContains(t, "stderr", stderr, "✗ Could not reach the control plane after 12 attempts (")
	if !strings.HasSuffix(stderr, "). Nothing was saved; code "+userCode+" expires on its own.\n") {
		t.Errorf("stderr ends %q", stderr[max(0, len(stderr)-120):])
	}
	if f.seen().polls != maxPollFailures {
		t.Errorf("polls %d, want %d", f.seen().polls, maxPollFailures)
	}
	noProfilesFile(t)
}

func TestLoginStopsQuietlyOnCtrlC(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{tok: minted()}}
	prev := pollSleep
	pollSleep = func(context.Context, time.Duration) error { return context.Canceled }
	t.Cleanup(func() { pollSleep = prev })

	_, stderr, code := veris(t, "login")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.HasSuffix(stderr, "Stopped waiting. Nothing was saved; code "+userCode+" expires on its own.\n") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if strings.Contains(stderr, "veris:") {
		t.Errorf("an interrupt earns no 'veris:' line:\n%s", stderr)
	}
	if f.seen().polls != 0 {
		t.Errorf("polled %d times after the interrupt", f.seen().polls)
	}
	noProfilesFile(t)
}

func TestLoginOffATerminalRepeatsTheURL(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{code: "authorization_pending"}, {code: "authorization_pending"}, {tok: minted()}}
	fastPolls(t)
	prev := urlReprintEvery
	urlReprintEvery = 0
	t.Cleanup(func() { urlReprintEvery = prev })

	_, stderr, code := veris(t, "login")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	line := "  Open   " + console + "/connect?code=" + userCode + "\n"
	if n := strings.Count(stderr, line); n != 4 {
		t.Errorf("the URL line printed %d times, want 4 (once, then before each of 3 polls):\n%s", n, stderr)
	}
}

func TestLoginQuietStillShowsTheCode(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{tok: minted()}}
	fastPolls(t)
	_, stderr, code := veris(t, "-q", "login", "--no-browser")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	expectEqual(t, "stderr", stderr, localWarn+
		"  Open   "+console+"/connect?code="+userCode+"\n"+
		"  Code   "+userCode+"\n")
}

func TestLoginConsoleURLFlagOverridesTheLearnedOne(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{tok: minted()}}
	fastPolls(t)
	_, stderr, code := veris(t, "login", "--console-url", "https://console.override/")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	_, p := savedProfile(t, "default")
	if p.ConsoleURL != "https://console.override" {
		t.Errorf("console_url %q", p.ConsoleURL)
	}
	expectContains(t, "stderr", stderr, "→ https://console.override/overview\n")
}

func TestLoginWithAKeyOnStdin(t *testing.T) {
	t.Run("saves a verified key with its name", func(t *testing.T) {
		b, f := loginBench(t)
		// An existing profile keeps what login does not own.
		b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
			"default": {APIBase: "https://plane.example", APIKey: "vsk_old"},
			"ci":      {DefaultEnvironment: "staging"},
		}})
		feedStdin(t, ciKey+"\n")

		stdout, stderr, code := veris(t, "login", "--key-stdin", "--profile", "ci")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout %q", stdout)
		}
		expectEqual(t, "stderr", stderr,
			"✓ Logged in as key 'ci · github' ("+ciKeyID+", vsk_ci0f7k3m…) on Acme ("+orgID+")\n"+
				"✓ API key saved to "+cfg.GlobalPath()+" (profile 'ci', mode 0600)\n"+
				"✓ Active profile switched to 'ci'\n"+
				"→ Next: veris env create\n")
		g, p := savedProfile(t, "ci")
		want := cfg.Profile{APIBase: f.srv.URL, APIKey: ciKey, KeyID: ciKeyID, KeyName: "ci · github",
			OrganizationID: orgID, OrganizationName: "Acme", DefaultEnvironment: "staging"}
		if p != want {
			t.Errorf("saved %+v, want %+v", p, want)
		}
		if g.ActiveProfile != "ci" || g.Profiles["default"].APIKey != "vsk_old" {
			t.Errorf("active %q, default profile %+v", g.ActiveProfile, g.Profiles["default"])
		}
		if f.seen().codeCalls != 0 || f.seen().polls != 0 {
			t.Errorf("a key on stdin must not start a pairing (mints %d, polls %d)", f.seen().codeCalls, f.seen().polls)
		}
		if len(f.seen().meKeys) != 1 || f.seen().meKeys[0] != ciKey {
			t.Errorf("/v1/me keys %q", f.seen().meKeys)
		}
		if strings.Contains(stderr, ciKey) {
			t.Errorf("the whole key leaked to stderr")
		}
	})
	t.Run("a key on the command line is warned about", func(t *testing.T) {
		_, f := loginBench(t)
		f.keys = nil // a plane that lists nothing for this key
		_, stderr, code := veris(t, "login", ciKey)
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr,
			"! a key on the command line lands in shell history; prefer --key-stdin\n"+
				"✓ Logged in with key vsk_ci0f7k3m… on Acme ("+orgID+")\n"+
				"✓ API key saved to "+cfg.GlobalPath()+" (profile 'default', mode 0600)\n"+
				"→ Next: veris env create\n")
		_, p := savedProfile(t, "default")
		if p.APIKey != ciKey || p.KeyID != "" || p.OrganizationID != orgID {
			t.Errorf("saved %+v", p)
		}
	})
	t.Run("a profile that already holds a key is warned about", func(t *testing.T) {
		b, _ := loginBench(t)
		b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{
			"default": {APIBase: "https://plane.example", APIKey: mintedKey},
		}})
		feedStdin(t, ciKey+"\n")
		_, stderr, code := veris(t, "login", "--key-stdin")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectContains(t, "stderr", stderr,
			"! Profile 'default' already holds key vsk_mi4pa0uo…; it stays valid on the server after this login. Revoke it first with veris logout --profile default, or later on the console\n")
		if _, p := savedProfile(t, "default"); p.APIKey != ciKey {
			t.Errorf("saved %+v", p)
		}
	})
	t.Run("a rejected key saves nothing", func(t *testing.T) {
		_, f := loginBench(t)
		f.meStatus = 401
		feedStdin(t, "vsk_nope\n")
		_, stderr, code := veris(t, "login", "--key-stdin")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		expectEqual(t, "stderr", stderr,
			"✗ The control plane at "+f.srv.URL+" rejected this key: [401] me is broken. Nothing was saved.\n")
		noProfilesFile(t)
	})
	t.Run("empty stdin", func(t *testing.T) {
		loginBench(t)
		feedStdin(t, "")
		_, stderr, code := veris(t, "login", "--key-stdin")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		expectContains(t, "stderr", stderr, "✗ No key on stdin\n", "→ Next: ")
		noProfilesFile(t)
	})
	t.Run("both forms at once is a usage error", func(t *testing.T) {
		loginBench(t)
		_, stderr, code := veris(t, "login", "--key-stdin", ciKey)
		if code != 1 || !strings.Contains(stderr, "veris: pass the key either as KEY or with --key-stdin, not both") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
	})
}

// logoutBench is a machine logged in as the minted key, on the fake plane.
func logoutBench(t *testing.T, p cfg.Profile) (*bench, *fakePlane) {
	t.Helper()
	b := newBench(t)
	f := newFakePlane(t)
	noBrowser(t)
	if p.APIBase == "" {
		p.APIBase = f.srv.URL
	}
	b.global(cfg.Global{ActiveProfile: "default", Profiles: map[string]cfg.Profile{"default": p}})
	return b, f
}

func TestLogoutRevokesTheKeyAndStripsTheProfile(t *testing.T) {
	full := cfg.Profile{APIKey: mintedKey, ConsoleURL: console, KeyID: mintedID,
		KeyName: "veris on victor-mbp · device", OrganizationID: orgID}
	t.Run("with --yes", func(t *testing.T) {
		_, f := logoutBench(t, full)
		_, stderr, code := veris(t, "logout", "--yes")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr,
			"Revoke key 'veris on victor-mbp · device' (key_b8bx…) on Acme and forget it locally? y\n"+
				"✓ Key revoked (takes effect within 30 s on every replica)\n"+
				"✓ Removed the credentials of profile 'default' from "+cfg.GlobalPath()+"\n")
		if fmt.Sprint(f.seen().revoked) != "["+mintedID+"]" || f.seen().revokeKeys[0] != mintedKey {
			t.Errorf("revoked %v with keys %v", f.seen().revoked, f.seen().revokeKeys)
		}
		_, p := savedProfile(t, "default")
		if want := (cfg.Profile{APIBase: f.srv.URL, ConsoleURL: console}); p != want {
			t.Errorf("profile after logout %+v, want %+v", p, want)
		}
		if raw, _ := os.ReadFile(cfg.GlobalPath()); strings.Contains(string(raw), mintedKey) {
			t.Errorf("the key is still in the file:\n%s", raw)
		}
	})
	t.Run("--keep-key skips the revoke", func(t *testing.T) {
		_, f := logoutBench(t, full)
		_, stderr, code := veris(t, "logout", "--keep-key", "--yes")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr,
			"Forget the credentials of profile 'default' locally (key vsk_mi4pa0uo… stays valid)? y\n"+
				"✓ Removed the credentials of profile 'default' from "+cfg.GlobalPath()+"\n")
		if len(f.seen().revoked) != 0 || len(f.seen().meKeys) != 0 {
			t.Errorf("revoked %v, /v1/me asked %d times", f.seen().revoked, len(f.seen().meKeys))
		}
		_, p := savedProfile(t, "default")
		if p.APIKey != "" || p.KeyID != "" {
			t.Errorf("credentials kept: %+v", p)
		}
	})
	t.Run("declined at the prompt", func(t *testing.T) {
		_, f := logoutBench(t, full)
		forceTTY(t)
		feedStdin(t, "n\n")
		_, stderr, code := veris(t, "logout")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		if !strings.HasSuffix(stderr, "veris: declined\n") || !strings.Contains(stderr, "[y/N] ") {
			t.Errorf("stderr:\n%s", stderr)
		}
		if len(f.seen().revoked) != 0 {
			t.Errorf("revoked %v after a no", f.seen().revoked)
		}
		_, p := savedProfile(t, "default")
		if p.APIKey != mintedKey {
			t.Errorf("profile changed after a no: %+v", p)
		}
	})
	t.Run("off a terminal without --yes", func(t *testing.T) {
		_, f := logoutBench(t, full)
		_, stderr, code := veris(t, "logout")
		if code != 1 || !strings.Contains(stderr, "Pass --yes instead") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
		if len(f.seen().revoked) != 0 {
			t.Errorf("revoked %v", f.seen().revoked)
		}
	})
	t.Run("a profile without key_id is matched by prefix", func(t *testing.T) {
		_, f := logoutBench(t, cfg.Profile{APIKey: ciKey})
		_, stderr, code := veris(t, "logout", "--yes")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectContains(t, "stderr", stderr, "Revoke key 'ci · github' (key_q2w9…) on Acme")
		if fmt.Sprint(f.seen().revoked) != "["+ciKeyID+"]" || f.seen().listCalls != 1 {
			t.Errorf("revoked %v, list calls %d", f.seen().revoked, f.seen().listCalls)
		}
	})
	t.Run("an unidentifiable key is not revoked but still forgotten", func(t *testing.T) {
		_, f := logoutBench(t, cfg.Profile{APIKey: "vsk_unlisted0000000000"})
		_, stderr, code := veris(t, "logout", "--yes")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr,
			"! Cannot tell which key profile 'default' holds (no key_id stored, no prefix match on /v1/api-keys); it is not revoked. Revoke it on the console.\n"+
				"Forget the credentials of profile 'default' locally (key vsk_unlisted… stays valid)? y\n"+
				"✓ Removed the credentials of profile 'default' from "+cfg.GlobalPath()+"\n")
		if len(f.seen().revoked) != 0 {
			t.Errorf("revoked %v", f.seen().revoked)
		}
		_, p := savedProfile(t, "default")
		if p.APIKey != "" {
			t.Errorf("key kept: %+v", p)
		}
	})
	t.Run("a key the plane no longer accepts", func(t *testing.T) {
		_, f := logoutBench(t, full)
		f.meStatus = 401
		_, stderr, code := veris(t, "logout", "--yes")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectContains(t, "stderr", stderr,
			"! The control plane no longer accepts this key ([401] me is broken); nothing to revoke\n",
			"✓ Removed the credentials of profile 'default'")
		if len(f.seen().revoked) != 0 {
			t.Errorf("revoked %v", f.seen().revoked)
		}
	})
	t.Run("a failed revoke keeps the profile", func(t *testing.T) {
		_, f := logoutBench(t, full)
		f.revokeStatus = 503
		_, stderr, code := veris(t, "logout", "--yes")
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		expectContains(t, "stderr", stderr, "✗ Failed to revoke key key_b8bx…: [503] not found\n")
		_, p := savedProfile(t, "default")
		if p.APIKey != mintedKey {
			t.Errorf("profile stripped over a failed revoke: %+v", p)
		}
	})
	t.Run("nothing to log out of", func(t *testing.T) {
		_, f := logoutBench(t, cfg.Profile{ConsoleURL: console})
		_, stderr, code := veris(t, "logout", "--yes")
		if code != 0 {
			t.Errorf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr, "! Profile 'default' holds no API key; nothing to log out of\n")
		if len(f.seen().revoked) != 0 {
			t.Errorf("revoked %v", f.seen().revoked)
		}
	})
}

func TestWhoamiNamesTheCredentialAndWhereItCameFrom(t *testing.T) {
	t.Run("an env key, named by prefix", func(t *testing.T) {
		_ = newBench(t)
		f := newFakePlane(t)
		t.Setenv(cfg.EnvAPIBase, f.srv.URL)
		t.Setenv(cfg.EnvAPIKey, ciKey)
		stdout, stderr, code := veris(t, "whoami")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout %q", stdout)
		}
		expectEqual(t, "stderr", stderr,
			"Profile  env VERIS_API_KEY → "+f.srv.URL+"\n"+
				"Key      vsk_ci0f7k3m… · 'ci · github' ("+ciKeyID+") · active\n"+
				"Org      Acme ("+orgID+") · team\n")
		if f.seen().meKeys[0] != ciKey || f.seen().listCalls != 1 {
			t.Errorf("/v1/me keys %v, list calls %d", f.seen().meKeys, f.seen().listCalls)
		}
	})
	t.Run("a profile key, named by what login stored", func(t *testing.T) {
		b := newBench(t)
		f := newFakePlane(t)
		b.global(cfg.Global{ActiveProfile: "dev", Profiles: map[string]cfg.Profile{
			"dev": {APIBase: f.srv.URL, APIKey: mintedKey, ConsoleURL: console, KeyID: mintedID,
				KeyName: "veris on victor-mbp · device", OrganizationID: orgID},
		}})
		_, stderr, code := veris(t, "whoami")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr,
			"Profile  profile 'dev' → "+f.srv.URL+"\n"+
				"Key      vsk_mi4pa0uo… · 'veris on victor-mbp · device' ("+mintedID+") · active\n"+
				"Org      Acme ("+orgID+") · team\n"+
				"Studio   "+console+"\n")
		if f.seen().listCalls != 0 {
			t.Errorf("the list route was asked %d times; the profile already knows", f.seen().listCalls)
		}
	})
	t.Run("an env key beside a profile is not the profile's key", func(t *testing.T) {
		b := newBench(t)
		f := newFakePlane(t)
		b.global(cfg.Global{ActiveProfile: "dev", Profiles: map[string]cfg.Profile{
			"dev": {APIBase: f.srv.URL, APIKey: mintedKey, KeyID: mintedID, KeyName: "laptop"},
		}})
		t.Setenv(cfg.EnvAPIKey, ciKey)
		_, stderr, code := veris(t, "whoami")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectContains(t, "stderr", stderr,
			"Profile  env VERIS_API_KEY → "+f.srv.URL+"\n",
			"Key      vsk_ci0f7k3m… · 'ci · github' ("+ciKeyID+") · active\n")
		if strings.Contains(stderr, "laptop") {
			t.Errorf("the stored name belongs to another key:\n%s", stderr)
		}
	})
	t.Run("the server's own key field wins", func(t *testing.T) {
		b := newBench(t)
		f := newFakePlane(t)
		f.me.Key = &api.APIKey{ID: "key_fromserver", Name: "server says", KeyPrefix: mintedKey[:12], Status: "active"}
		b.global(cfg.Global{Profiles: map[string]cfg.Profile{
			"default": {APIBase: f.srv.URL, APIKey: mintedKey, KeyID: mintedID, KeyName: "stored"},
		}})
		_, stderr, code := veris(t, "whoami")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectContains(t, "stderr", stderr, "Key      vsk_mi4pa0uo… · 'server says' (key_fromserver) · active\n")
	})
	t.Run("an unlisted key", func(t *testing.T) {
		_ = newBench(t)
		f := newFakePlane(t)
		f.keys = nil
		t.Setenv(cfg.EnvAPIBase, f.srv.URL)
		t.Setenv(cfg.EnvAPIKey, "vsk_unlisted0000000000")
		_, stderr, code := veris(t, "whoami")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectContains(t, "stderr", stderr, "Key      vsk_unlisted… · (unknown key)\n")
	})
	t.Run("--json is the /v1/me body on stdout", func(t *testing.T) {
		_ = newBench(t)
		f := newFakePlane(t)
		t.Setenv(cfg.EnvAPIBase, f.srv.URL)
		t.Setenv(cfg.EnvAPIKey, ciKey)
		stdout, stderr, code := veris(t, "whoami", "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		var me api.Me
		if err := json.Unmarshal([]byte(stdout), &me); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
		}
		if me.Kind != "api_key" || me.OrganizationID != orgID || len(me.Organizations) != 1 {
			t.Errorf("body %+v", me)
		}
		if f.seen().listCalls != 0 {
			t.Errorf("--json asked the list route %d times", f.seen().listCalls)
		}
		// The body is the plane's, not a re-encoding: an empty list stays
		// [] and a field this binary does not model is still there.
		f.meRaw = []byte(`{"kind":"operator","user":null,"organization_id":"","organizations":[],"tenancy":"pre"}`)
		stdout, _, code = veris(t, "whoami", "--json")
		if code != 0 {
			t.Fatal(code)
		}
		expectContains(t, "stdout", stdout, `"organizations": []`, `"tenancy": "pre"`)
	})
	t.Run("not logged in", func(t *testing.T) {
		_ = newBench(t)
		stdout, stderr, code := veris(t, "whoami")
		if code != 1 || stdout != "" {
			t.Errorf("exit %d, stdout %q", code, stdout)
		}
		expectEqual(t, "stderr", stderr,
			"✗ Not logged in for profile 'default' (no API key)\n→ Next: veris login --profile default\n")
	})
	t.Run("a refused key", func(t *testing.T) {
		b := newBench(t)
		f := newFakePlane(t)
		f.meStatus = 401
		b.global(cfg.Global{Profiles: map[string]cfg.Profile{"default": {APIBase: f.srv.URL, APIKey: mintedKey}}})
		_, stderr, code := veris(t, "whoami")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
		expectEqual(t, "stderr", stderr,
			"✗ Not logged in for profile 'default': [401] me is broken\n→ Next: veris login --profile default\n")
	})
}

// profileBench is a machine with three profiles, dev active.
func profileBench(t *testing.T) *bench {
	t.Helper()
	b := newBench(t)
	b.global(cfg.Global{ActiveProfile: "dev", Profiles: map[string]cfg.Profile{
		"default": {APIBase: "https://svc.api.veris.ai", APIKey: mintedKey, OrganizationID: orgID, ConsoleURL: "https://studio.veris.ai"},
		"dev": {APIBase: "https://svc.dev.api.veris.ai", APIKey: "vsk_9a7b3c1d5e7f9g1h3j5k7l9m", OrganizationID: "org_devdevdevdevdevdevdevdev", OrganizationName: "Acme dev",
			ConsoleURL: "https://studio.dev.veris.ai", KeyID: "key_dev", KeyName: "veris on victor-mbp · device", DefaultEnvironment: "staging"},
		"local": {APIBase: "http://127.0.0.1:8100", APIKey: "localtest-secret"},
	}})
	return b
}

func TestProfileListMarksTheActiveOne(t *testing.T) {
	profileBench(t)
	stdout, stderr, code := veris(t, "profile", "list")
	if code != 0 || stdout != "" {
		t.Fatalf("exit %d, stdout %q\n%s", code, stdout, stderr)
	}
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and three rows:\n%s", stderr)
	}
	wantPrefix := []string{"  Profile  API", "  default  https://svc.api.veris.ai", "* dev      https://svc.dev.api.veris.ai", "  local    http://127.0.0.1:8100"}
	for i, w := range wantPrefix {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("line %d %q, want prefix %q", i, lines[i], w)
		}
	}
	// The Org column is the organisation's name where login learned one
	// and the short id where it did not (a profile written by hand).
	expectContains(t, "table", stderr, "vsk_mi4pa0uo…", "vsk_9a7b3c1d…", "localtest-se…", "org_7f3k…", "Acme dev", "—")
	for _, whole := range []string{mintedKey, "vsk_9a7b3c1d5e7f9g1h3j5k7l9m", "localtest-secret"} {
		if strings.Contains(stderr, whole) {
			t.Errorf("a whole key is in the table: %s", whole)
		}
	}

	t.Run("--json masks the keys too", func(t *testing.T) {
		stdout, _, code := veris(t, "profile", "list", "--json")
		if code != 0 {
			t.Fatal(code)
		}
		var views []profileView
		if err := json.Unmarshal([]byte(stdout), &views); err != nil {
			t.Fatalf("%v\n%s", err, stdout)
		}
		if len(views) != 3 || views[1].Name != "dev" || !views[1].Active || views[0].Active {
			t.Errorf("views %+v", views)
		}
		if views[1].Key != "vsk_9a7b3c1d…" || strings.Contains(stdout, mintedKey) {
			t.Errorf("keys are not masked:\n%s", stdout)
		}
	})
	t.Run("no profiles yet", func(t *testing.T) {
		newBench(t)
		_, stderr, code := veris(t, "profile", "list")
		if code != 0 {
			t.Errorf("exit %d", code)
		}
		expectEqual(t, "stderr", stderr, "No profiles yet\n→ Next: veris login\n")
	})
}

func TestProfileGetShowsTheFieldsWithTheKeyMasked(t *testing.T) {
	profileBench(t)
	_, stderr, code := veris(t, "profile", "get", "dev")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	expectEqual(t, "stderr", stderr,
		"Profile   dev\n"+
			"Active    yes\n"+
			"API       https://svc.dev.api.veris.ai\n"+
			"Console   https://studio.dev.veris.ai\n"+
			"Org       org_devdevdevdevdevdevdevdev\n"+
			"Key       vsk_9a7b3c1d…\n"+
			"Key name  veris on victor-mbp · device\n"+
			"Key id    key_dev\n"+
			"Default environment  staging\n")

	t.Run("defaults to the active profile, or --profile", func(t *testing.T) {
		_, stderr, _ := veris(t, "profile", "get")
		expectContains(t, "stderr", stderr, "Profile   dev\n")
		_, stderr, _ = veris(t, "--profile", "local", "profile", "get")
		expectContains(t, "stderr", stderr, "Profile   local\n", "Active    no\n", "Key       localtest-se…\n")
	})
	t.Run("--json", func(t *testing.T) {
		stdout, stderr, code := veris(t, "profile", "get", "default", "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		var v profileView
		if err := json.Unmarshal([]byte(stdout), &v); err != nil {
			t.Fatal(err)
		}
		if v.Name != "default" || v.Active || v.Key != "vsk_mi4pa0uo…" || v.OrganizationID != orgID {
			t.Errorf("view %+v", v)
		}
	})
	t.Run("unknown profile", func(t *testing.T) {
		_, stderr, code := veris(t, "profile", "get", "nope")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
		expectEqual(t, "stderr", stderr,
			"✗ No profile 'nope' in "+cfg.GlobalPath()+"\n→ Next: veris login --profile nope\n")
	})
}

func TestProfileUseSwitchesTheActiveProfile(t *testing.T) {
	t.Run("by name", func(t *testing.T) {
		profileBench(t)
		_, stderr, code := veris(t, "profile", "use", "default")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr, "✓ Active profile set to 'default'\n")
		if g, _ := savedProfile(t, "default"); g.ActiveProfile != "default" {
			t.Errorf("active_profile %q", g.ActiveProfile)
		}
	})
	t.Run("picked on a terminal", func(t *testing.T) {
		profileBench(t)
		forceTTY(t)
		feedStdin(t, "local\n")
		_, stderr, code := veris(t, "profile", "use")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectContains(t, "stderr", stderr, "? Select a profile:\n", "  1) default", "  2) dev", "  3) local",
			"✓ Active profile set to 'local'\n")
		if g, _ := savedProfile(t, "local"); g.ActiveProfile != "local" {
			t.Errorf("active_profile %q", g.ActiveProfile)
		}
	})
	t.Run("off a terminal it names what to pass", func(t *testing.T) {
		profileBench(t)
		_, stderr, code := veris(t, "profile", "use")
		if code != 1 || !strings.Contains(stderr, "Pass a profile NAME instead") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
		if g, _ := savedProfile(t, "dev"); g.ActiveProfile != "dev" {
			t.Errorf("active_profile changed to %q", g.ActiveProfile)
		}
	})
	t.Run("unknown name", func(t *testing.T) {
		profileBench(t)
		_, stderr, code := veris(t, "profile", "use", "nope")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
		expectContains(t, "stderr", stderr, "✗ No profile 'nope' in ")
	})
}

func TestProfileDeleteRefusesTheActiveOne(t *testing.T) {
	t.Run("the active profile", func(t *testing.T) {
		profileBench(t)
		_, stderr, code := veris(t, "profile", "delete", "dev", "--yes")
		if code != 1 {
			t.Errorf("exit %d", code)
		}
		expectEqual(t, "stderr", stderr, "✗ 'dev' is the active profile; run veris profile use OTHER first\n")
		savedProfile(t, "dev")
	})
	t.Run("another profile, confirmed", func(t *testing.T) {
		profileBench(t)
		_, stderr, code := veris(t, "profile", "delete", "local", "--yes")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		expectEqual(t, "stderr", stderr,
			"Delete profile 'local'? Its key localtest-se… stays valid; veris logout --profile local revokes it. y\n"+
				"✓ Deleted profile 'local' from "+cfg.GlobalPath()+"\n")
		g, _ := savedProfile(t, "dev")
		if _, ok := g.Profiles["local"]; ok {
			t.Errorf("local still in the file")
		}
	})
	t.Run("declined", func(t *testing.T) {
		profileBench(t)
		forceTTY(t)
		feedStdin(t, "\n")
		_, stderr, code := veris(t, "profile", "delete", "local")
		if code != 1 || !strings.HasSuffix(stderr, "veris: declined\n") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
		savedProfile(t, "local")
	})
	t.Run("needs a name", func(t *testing.T) {
		profileBench(t)
		_, stderr, code := veris(t, "profile", "delete")
		if code != 1 || !strings.Contains(stderr, "profile delete takes exactly one NAME") {
			t.Errorf("exit %d, stderr:\n%s", code, stderr)
		}
	})
}

func TestLoginOnATerminalOpensTheBrowserAndTicks(t *testing.T) {
	_, f := loginBench(t)
	f.tokens = []tokenStep{{code: "authorization_pending"}, {tok: minted()}}
	sleeps := fastPolls(t)
	forceTTY(t)
	var opened []string
	openBrowser = func(url string) error { opened = append(opened, url); return nil }

	_, stderr, code := veris(t, "login")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if want := []string{console + "/connect?code=" + userCode}; fmt.Sprint(opened) != fmt.Sprint(want) {
		t.Errorf("opened %v, want %v", opened, want)
	}
	expectContains(t, "stderr", stderr, "  (opened in your browser — pass --no-browser to skip)\n",
		"✓ Approved for Acme ("+orgID+")\n")
	// Two polls at six seconds each, slept a second at a time so the
	// countdown can tick between them.
	if len(*sleeps) != 12 {
		t.Errorf("%d sleeps, want 12 one-second slices: %v", len(*sleeps), *sleeps)
	}
	for _, d := range *sleeps {
		if d != time.Second {
			t.Errorf("slept %v, want 1s slices on a terminal", d)
		}
	}
	t.Run("--no-browser", func(t *testing.T) {
		_, f := loginBench(t)
		f.tokens = []tokenStep{{tok: minted()}}
		fastPolls(t)
		forceTTY(t)
		_, stderr, code := veris(t, "login", "--no-browser")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, stderr)
		}
		if strings.Contains(stderr, "opened in your browser") {
			t.Errorf("--no-browser still claimed to open one:\n%s", stderr)
		}
	})
}
