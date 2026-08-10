package config

import "testing"

// Google fronts three services on one hostname, told apart only by prefix, and
// a Calendar sandbox always contains at least two of them: asking for
// google-calendar auto-adds google-identity, because Calendar issues no tokens
// and verifies them against its sibling. Host-only routing cannot express that.
func googleFamily() *Config {
	return &Config{
		Version:   1,
		Listen:    "127.0.0.1:8080",
		SandboxID: "sbx_google",
		Mode:      ModePassthrough,
		Upstream:  Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services: []Service{
			{Name: "google-calendar", Hosts: []string{"www.googleapis.com"},
				Paths: []string{"/calendar/v3"}},
			{Name: "google-drive", Hosts: []string{"www.googleapis.com"},
				Paths: []string{"/drive/v3", "/upload/drive/v3"}},
			{Name: "google-identity", Hosts: []string{"www.googleapis.com"},
				Paths: []string{"/oauth2", "/tokeninfo", "/userinfo"}},
			{Name: "google-gmail", Hosts: []string{"gmail.googleapis.com"}},
		},
	}
}

func resolveTo(t *testing.T, c *Config, host, path, want string) {
	t.Helper()
	got, ok := c.Resolve(host, path)
	if !ok {
		t.Fatalf("%s%s did not resolve; want %s", host, path, want)
	}
	if got.Service != want {
		t.Errorf("%s%s resolved to %q, want %q", host, path, got.Service, want)
	}
}

func TestOneHostnameSplitsAcrossServices(t *testing.T) {
	c := googleFamily()
	resolveTo(t, c, "www.googleapis.com", "/calendar/v3/users/me/calendarList", "google-calendar")
	resolveTo(t, c, "www.googleapis.com", "/drive/v3/files", "google-drive")
	resolveTo(t, c, "www.googleapis.com", "/oauth2/v3/certs", "google-identity")
	// A host with no prefixes still claims everything on it.
	resolveTo(t, c, "gmail.googleapis.com", "/gmail/v1/users/me/messages", "google-gmail")
}

func TestTheLongestPrefixWins(t *testing.T) {
	c := googleFamily()
	// /upload/drive/v3 must not be swallowed by a shorter sibling.
	resolveTo(t, c, "www.googleapis.com", "/upload/drive/v3/files", "google-drive")
}

func TestPrefixesMatchOnASegmentBoundary(t *testing.T) {
	c := googleFamily()
	// The prefix itself, and anything below it.
	resolveTo(t, c, "www.googleapis.com", "/userinfo", "google-identity")
	resolveTo(t, c, "www.googleapis.com", "/userinfo/v2/me", "google-identity")
	// But not a longer word that merely starts the same way -- raw string
	// prefixing would hand this to google-identity.
	if got, ok := c.Resolve("www.googleapis.com", "/userinfoXYZ"); ok {
		t.Errorf("/userinfoXYZ resolved to %q; want no match", got.Service)
	}
	if got, ok := c.Resolve("www.googleapis.com", "/calendar/v3beta/x"); ok {
		t.Errorf("/calendar/v3beta resolved to %q; want no match", got.Service)
	}
}

func TestAnUnclaimedPathOnAClaimedHostDoesNotResolve(t *testing.T) {
	c := googleFamily()
	// Gmail's batch endpoint lives on www.googleapis.com but no service here
	// owns it. Handing it to whichever entry matched the host would route it
	// to a service that cannot answer.
	if got, ok := c.Resolve("www.googleapis.com", "/batch/gmail/v1"); ok {
		t.Errorf("/batch/gmail/v1 resolved to %q; want no match", got.Service)
	}
}

func TestAnExactHostBeatsAWildcardEvenWithoutAPrefix(t *testing.T) {
	c := &Config{
		Version: 1, Listen: "127.0.0.1:8080", SandboxID: "sbx", Mode: ModePassthrough,
		Upstream: Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services: []Service{
			{Name: "catch-all", Hosts: []string{"*.googleapis.com"}},
			{Name: "google-calendar", Hosts: []string{"www.googleapis.com"},
				Paths: []string{"/calendar/v3"}},
		},
	}
	resolveTo(t, c, "www.googleapis.com", "/calendar/v3/x", "google-calendar")
	// The exact entry does not claim this path, so the wildcard takes it
	// rather than the request going unrouted.
	resolveTo(t, c, "www.googleapis.com", "/something/else", "catch-all")
	resolveTo(t, c, "sheets.googleapis.com", "/v4/spreadsheets", "catch-all")
}

func TestAPrefixOnAHostBeatsTheWholeHostEntry(t *testing.T) {
	c := &Config{
		Version: 1, Listen: "127.0.0.1:8080", SandboxID: "sbx", Mode: ModePassthrough,
		Upstream: Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services: []Service{
			{Name: "whole-host", Hosts: []string{"api.example.com"}},
			{Name: "narrow", Hosts: []string{"api.example.com"}, Paths: []string{"/v2"}},
		},
	}
	resolveTo(t, c, "api.example.com", "/v2/things", "narrow")
	resolveTo(t, c, "api.example.com", "/v1/things", "whole-host")
}

func TestTwoServicesMayShareAHostWithDifferentPrefixes(t *testing.T) {
	if err := googleFamily().Validate(); err != nil {
		t.Fatalf("the Google family should be a legal config: %v", err)
	}
}

func TestTheSameHostAndPrefixTwiceIsRejected(t *testing.T) {
	c := googleFamily()
	c.Services = append(c.Services, Service{
		Name: "impostor", Hosts: []string{"www.googleapis.com"},
		Paths: []string{"/calendar/v3"},
	})
	if err := c.Validate(); err == nil {
		t.Fatal("two services claiming one host and prefix should be rejected at load")
	}
}

func TestTheWholeHostTwiceIsStillRejected(t *testing.T) {
	c := &Config{
		Version: 1, Listen: "127.0.0.1:8080", SandboxID: "sbx", Mode: ModePassthrough,
		Upstream: Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services: []Service{
			{Name: "a", Hosts: []string{"api.example.com"}},
			{Name: "b", Hosts: []string{"api.example.com"}},
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatal("two services claiming the whole host should be rejected at load")
	}
}

func TestTheSameHostAndPrefixSpelledDifferentlyIsStillRejected(t *testing.T) {
	c := googleFamily()
	// "/calendar/v3/" routes identically to "/calendar/v3", so declaring both
	// is the same collision spelled two ways.
	c.Services = append(c.Services, Service{
		Name: "impostor", Hosts: []string{"www.googleapis.com"},
		Paths: []string{" /calendar/v3/ "},
	})
	if err := c.Validate(); err == nil {
		t.Fatal("a prefix differing only in whitespace and trailing slash should still collide")
	}
}

// A root prefix means the same thing as declaring no prefix at all, so the two
// spellings must collide rather than leaving Resolve to pick by declaration
// order.
func TestARootPrefixCollidesWithAWholeHostEntry(t *testing.T) {
	c := &Config{
		Version: 1, Listen: "127.0.0.1:8080", SandboxID: "sbx", Mode: ModePassthrough,
		Upstream: Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services: []Service{
			{Name: "whole-host", Hosts: []string{"api.example.com"}},
			{Name: "root-prefix", Hosts: []string{"api.example.com"}, Paths: []string{"/"}},
		},
	}
	if err := c.Validate(); err == nil {
		t.Fatal(`a "/" prefix claims the whole host, so it must collide with a pathless entry`)
	}
}

func TestThePortAndCaseOfTheHostDoNotAffectRouting(t *testing.T) {
	c := googleFamily()
	resolveTo(t, c, "WWW.GoogleAPIs.com:443", "/drive/v3/files", "google-drive")
}

func TestTheMatchedPrefixIsReported(t *testing.T) {
	c := googleFamily()
	got, ok := c.Resolve("www.googleapis.com", "/upload/drive/v3/files")
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Prefix != "/upload/drive/v3" {
		t.Errorf("matched prefix %q, want %q", got.Prefix, "/upload/drive/v3")
	}
	// A whole-host entry reports root rather than an empty string, so a receipt
	// key is always well formed.
	got, _ = c.Resolve("gmail.googleapis.com", "/gmail/v1/users/me/messages")
	if got.Prefix != "/" {
		t.Errorf("whole-host prefix %q, want %q", got.Prefix, "/")
	}
}

func TestTheUpstreamIsTheSandboxServiceRoute(t *testing.T) {
	c := googleFamily()
	got, _ := c.Resolve("www.googleapis.com", "/calendar/v3/users/me/calendarList")
	want := "https://sandbox.veris.ai/s/sbx_google/google-calendar"
	if got.Upstream.String() != want {
		t.Errorf("upstream %q, want %q", got.Upstream, want)
	}
}

func TestAnEmptyPathIsTreatedAsRoot(t *testing.T) {
	c := &Config{
		Version: 1, Listen: "127.0.0.1:8080", SandboxID: "sbx", Mode: ModePassthrough,
		Upstream: Upstream{BaseURL: "https://sandbox.veris.ai"},
		Services: []Service{{Name: "svc", Hosts: []string{"api.example.com"}}},
	}
	// A CONNECT-style flow can reach us before any path is known.
	resolveTo(t, c, "api.example.com", "", "svc")
}
