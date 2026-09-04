package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/veris-ai/veris-cli/internal/routes"
)

// A Calendar sandbox always contains google-identity too: Calendar issues no
// tokens and verifies them against its sibling. Both must end up routed, and
// they share a hostname, so this is the case the whole route table exists for.
func googleSandbox(base string) *Snapshot {
	return &Snapshot{
		SandboxID:     "sbx_google",
		EnvironmentID: "env_1",
		Status:        "ready",
		Services: []Service{
			{Name: "google-calendar", URL: base + "/s/sbx_google/google-calendar", Status: "ready"},
			{Name: "google-identity", URL: base + "/s/sbx_google/google-identity", Status: "ready"},
		},
	}
}

func TestAConfigIsDerivedWithNoFileAuthored(t *testing.T) {
	cfg, skipped, err := ToConfig(googleSandbox("http://sandbox.test"), nil)
	if err != nil {
		t.Fatalf("ToConfig: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("unexpectedly skipped: %+v", skipped)
	}
	if cfg.SandboxID != "sbx_google" {
		t.Errorf("sandbox_id = %q", cfg.SandboxID)
	}

	// Every entry's upstream is the URL the control plane gave, verbatim.
	for _, svc := range cfg.Services {
		want := "http://sandbox.test/s/sbx_google/" + svc.Name
		if svc.Upstream != want {
			t.Errorf("%s upstream = %q, want %q", svc.Name, svc.Upstream, want)
		}
	}
}

// The measured record puts /tokeninfo on oauth2.googleapis.com. A hand-written
// table had it on www.googleapis.com, which would route a client's token
// introspection at a host that does not serve it.
func TestTokeninfoIsRoutedWhereGoogleServesIt(t *testing.T) {
	cfg, _, err := ToConfig(googleSandbox("http://sandbox.test"), nil)
	if err != nil {
		t.Fatal(err)
	}

	var host string
	for _, svc := range cfg.Services {
		for _, p := range svc.Paths {
			if p == "/tokeninfo" {
				host = svc.Hosts[0]
			}
		}
	}
	if host != "oauth2.googleapis.com" {
		t.Errorf("/tokeninfo routed to %q, want oauth2.googleapis.com", host)
	}
}

// Three services answer on www.googleapis.com. The derived config must give
// each a prefix, and must load -- the proxy rejects two services claiming one
// host and prefix.
func TestTheDerivedConfigResolvesTheSharedGoogleHost(t *testing.T) {
	cfg, _, err := ToConfig(googleSandbox("http://sandbox.test"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("derived config does not validate: %v", err)
	}

	got, ok := cfg.Resolve("www.googleapis.com", "/calendar/v3/users/me/calendarList")
	if !ok || got.Service != "google-calendar" {
		t.Errorf("calendar path resolved to %+v", got)
	}
	got, ok = cfg.Resolve("www.googleapis.com", "/oauth2/v3/certs")
	if !ok || got.Service != "google-identity" {
		t.Errorf("certs path resolved to %+v", got)
	}
}

// A sandbox can run things this proxy cannot intercept. Saying so beats
// letting a client believe a dependency was covered.
func TestANonHTTPServiceIsReportedRatherThanDropped(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_mixed",
		Services: []Service{
			{Name: "stripe", URL: "http://sandbox.test/s/sbx_mixed/stripe"},
			{Name: "pg", URL: "postgres://user:pw@10.0.0.2:5432/db"},
		},
	}
	cfg, skipped, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Name != "stripe" {
		t.Errorf("services = %+v", cfg.Services)
	}
	if len(skipped) != 1 || skipped[0].Service != "pg" {
		t.Fatalf("skipped = %+v, want the postgres service", skipped)
	}
}

func TestAServiceWithNoMeasuredHostIsReported(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_x",
		Services: []Service{
			{Name: "stripe", URL: "http://sandbox.test/s/sbx_x/stripe"},
			{Name: "not-a-real-vendor", URL: "http://sandbox.test/s/sbx_x/nope"},
		},
	}
	_, skipped, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Service != "not-a-real-vendor" {
		t.Fatalf("skipped = %+v", skipped)
	}
}

// Nothing would be intercepted, so a run would prove nothing. Better to refuse
// than to start a proxy that cannot route a single request.
func TestASandboxWithNothingRoutableIsAnError(t *testing.T) {
	_, _, err := ToConfig(&Snapshot{
		SandboxID: "sbx_empty",
		Services:  []Service{{Name: "pg", URL: "postgres://h/db"}},
	}, nil)
	if err == nil {
		t.Fatal("a sandbox with nothing routable should be an error")
	}
}

// Routes served by the control plane beat the embedded table: they are the
// same measured record, but current rather than frozen at this binary's
// release.
func TestControlPlaneRoutesBeatTheEmbeddedTable(t *testing.T) {
	snapshot := googleSandbox("http://sandbox.test")
	snapshot.Services[0].Routes = []routes.Entry{
		{Host: "calendar.fresh.example", Paths: []string{"/v9"}},
	}
	cfg, _, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calendarHosts []string
	for _, svc := range cfg.Services {
		if svc.Name == "google-calendar" {
			calendarHosts = append(calendarHosts, svc.Hosts...)
		}
	}
	if len(calendarHosts) != 1 || calendarHosts[0] != "calendar.fresh.example" {
		t.Errorf("calendar hosts = %v, want only the served route", calendarHosts)
	}
	// The sibling served no routes, so it still resolves from the table.
	var identityRouted bool
	for _, svc := range cfg.Services {
		if svc.Name == "google-identity" {
			identityRouted = true
		}
	}
	if !identityRouted {
		t.Error("google-identity lost its embedded-table routes")
	}
}

// A newer control plane with one malformed row must not take down every run
// against it; a service whose rows all drop falls back to the table.
func TestMalformedServedRoutesFallBackToTheTable(t *testing.T) {
	snapshot := googleSandbox("http://sandbox.test")
	snapshot.Services[0].Routes = []routes.Entry{{Host: "  "}}
	cfg, _, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range cfg.Services {
		if svc.Name == "google-calendar" && svc.Hosts[0] != "www.googleapis.com" {
			t.Errorf("calendar host = %q, want the embedded table's", svc.Hosts[0])
		}
	}
}

// --route replaces every derived route for the service it names, and can route
// a service neither the control plane nor the table knows -- the "measurement
// has not landed yet" case the flag exists for.
func TestARouteOverrideReplacesAndEnables(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_o",
		Services: []Service{
			{Name: "stripe", URL: "http://sandbox.test/s/sbx_o/stripe"},
			{Name: "brand-new", URL: "http://sandbox.test/s/sbx_o/brand-new"},
		},
	}
	overrides := map[string][]routes.Entry{
		"stripe":    {{Host: "api.stripe.dev"}},
		"brand-new": {{Host: "api.newvendor.example", Paths: []string{"/v1"}}},
	}
	cfg, skipped, err := ToConfig(snapshot, overrides)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("unexpectedly skipped: %+v", skipped)
	}
	hosts := map[string]string{}
	for _, svc := range cfg.Services {
		hosts[svc.Name] = svc.Hosts[0]
	}
	if hosts["stripe"] != "api.stripe.dev" {
		t.Errorf("stripe host = %q, want the override", hosts["stripe"])
	}
	if hosts["brand-new"] != "api.newvendor.example" {
		t.Errorf("brand-new host = %q, want the override", hosts["brand-new"])
	}
}

// A typo'd --route silently ignored would leave its author believing a
// dependency was covered. It is an error naming what the sandbox does run.
func TestAnOverrideForAnAbsentServiceIsAnError(t *testing.T) {
	_, _, err := ToConfig(googleSandbox("http://sandbox.test"),
		map[string][]routes.Entry{"sptire": {{Host: "api.stripe.com"}}})
	if err == nil || !strings.Contains(err.Error(), "sptire") {
		t.Fatalf("err = %v, want a refusal naming the typo", err)
	}
	if err != nil && !strings.Contains(err.Error(), "google-calendar") {
		t.Errorf("err = %v, want it to list what the sandbox runs", err)
	}
}

func TestFetchReadsTheControlPlane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "k_test" {
			t.Errorf("API key header = %q", got)
		}
		if r.URL.Path != "/v1/sandboxes/sbx_abc" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             "sbx_abc",
			"environment_id": "env_9",
			"status":         "ready",
			// A field this binary does not know about must not break it: a
			// control plane has to be able to add response fields.
			"some_future_field": 42,
			"services": []map[string]any{
				{"name": "stripe", "url": srvURL(r) + "/s/sbx_abc/stripe", "status": "ready"},
			},
		})
	}))
	defer srv.Close()

	client, err := NewClient(srv.URL, "k_test")
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.Fetch(context.Background(), "sbx_abc")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if sandbox.EnvironmentID != "env_9" || len(sandbox.Services) != 1 {
		t.Fatalf("sandbox = %+v", sandbox)
	}
}

func TestFetchExplainsARefusedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client, _ := NewClient(srv.URL, "k_wrong")
	_, err := client.Fetch(context.Background(), "sbx_abc")
	if err == nil {
		t.Fatal("a 401 should be an error")
	}
}

func TestAMissingAPIKeyIsRefusedBeforeAnyRequest(t *testing.T) {
	t.Setenv(EnvAPIKey, "")
	if _, err := NewClient("http://example.test", ""); err == nil {
		t.Fatal("no API key should be refused")
	}
}

func srvURL(r *http.Request) string { return "http://" + r.Host }

// Two ids that sanitise to one filename must not read each other's cache: the
// second would be routed to the first sandbox's services with nothing said.
func TestACollidingSandboxIDIsACacheMissNotAWrongAnswer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first := &Snapshot{SandboxID: "team/a", Services: []Service{{Name: "stripe"}}}
	if err := writeJSON(snapshotPath(first.SandboxID), first); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadSnapshot("team/a"); err != nil || got.SandboxID != "team/a" {
		t.Fatalf("its own id should load: %v %v", got, err)
	}
	if _, err := LoadSnapshot("team_a"); err == nil {
		t.Fatal("team_a shares the filename of team/a and must not load it")
	}
}

func TestADatabaseServiceIsHandedOverNotRouted(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_db", EnvironmentID: "env_db",
		Services: []Service{
			{Name: "stripe", URL: "http://gw/s/sbx_db/stripe", EnvHint: "STRIPE_API_BASE"},
			{Name: "postgres", URL: "postgres://user:pw@10.0.0.2:5432/app",
				EnvHint: "DATABASE_URL"},
		},
	}
	cfg, skipped, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PassEnv) != 1 || cfg.PassEnv[0].Name != "DATABASE_URL" ||
		cfg.PassEnv[0].Value != "postgres://user:pw@10.0.0.2:5432/app" ||
		cfg.PassEnv[0].Service != "postgres" {
		t.Fatalf("pass_env = %+v, want the postgres DSN under DATABASE_URL", cfg.PassEnv)
	}
	// The note names the variable, so "why isn't my database proxied" answers
	// itself in the startup output.
	found := false
	for _, s := range skipped {
		if s.Service == "postgres" && strings.Contains(s.Reason, "$DATABASE_URL") {
			found = true
		}
	}
	if !found {
		t.Fatalf("skipped notes %+v never name $DATABASE_URL", skipped)
	}
}

func TestADatabaseOnlySandboxStillYieldsAConfig(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_dbonly", EnvironmentID: "env_db",
		Services: []Service{
			{Name: "postgres", URL: "postgres://u:p@h:5432/app", EnvHint: "DATABASE_URL"},
		},
	}
	cfg, _, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatalf("a lone database is a real sandbox shape, got: %v", err)
	}
	if len(cfg.Services) != 0 || len(cfg.PassEnv) != 1 {
		t.Fatalf("want zero routed services and one handoff, got %d/%d",
			len(cfg.Services), len(cfg.PassEnv))
	}
}

// An http twin with no hostname to intercept -- no route from the control
// plane, none in the table -- is the DSN case again: handed over under its
// env hint rather than silently left unreachable. One with no hint is still
// only reported, as before.
func TestAnHTTPTwinWithNoRouteIsHandedOverLikeADSN(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_yente", EnvironmentID: "env_y",
		Services: []Service{
			{Name: "stripe", URL: "http://gw/s/sbx_yente/stripe", EnvHint: "STRIPE_API_BASE"},
			{Name: "yente", URL: "http://gw/s/sbx_yente/yente", EnvHint: "YENTE_API_BASE"},
			{Name: "nameless", URL: "http://gw/s/sbx_yente/nameless"},
		},
	}
	cfg, skipped, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Name != "stripe" {
		t.Errorf("routed services = %+v, want stripe alone", cfg.Services)
	}
	if len(cfg.PassEnv) != 1 || cfg.PassEnv[0].Name != "YENTE_API_BASE" ||
		cfg.PassEnv[0].Value != "http://gw/s/sbx_yente/yente" || cfg.PassEnv[0].Service != "yente" {
		t.Fatalf("pass_env = %+v, want yente's URL under YENTE_API_BASE", cfg.PassEnv)
	}
	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[s.Service] = s.Reason
	}
	if !strings.Contains(reasons["yente"], "handed to the command as $YENTE_API_BASE") {
		t.Errorf("yente's note %q never names $YENTE_API_BASE", reasons["yente"])
	}
	if !strings.HasPrefix(reasons["nameless"], "no route:") {
		t.Errorf("a twin with no hint is reported, not handed: %q", reasons["nameless"])
	}

	// The same rule, asked about one service at a time.
	for _, c := range []struct {
		svc  Service
		want bool
	}{
		{Service{Name: "stripe", URL: "http://gw/stripe"}, false},
		{Service{Name: "yente", URL: "http://gw/yente"}, true},
		{Service{Name: "postgres", URL: "postgres://u:p@h/db"}, true},
		{Service{Name: "yente", URL: "http://gw/yente", Routes: []routes.Entry{{Host: "api.yente.test"}}}, false},
	} {
		if got := NotProxied(c.svc, nil); got != c.want {
			t.Errorf("NotProxied(%s %s) = %v, want %v", c.svc.Name, c.svc.URL, got, c.want)
		}
	}
	// A --route override makes an otherwise unroutable twin proxied.
	if NotProxied(Service{Name: "yente", URL: "http://gw/yente"}, map[string][]routes.Entry{"yente": {{Host: "api.yente.test"}}}) {
		t.Error("an override supplies the hostname, so yente is proxied")
	}
}
