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
// they share a hostname, so this is the case the routing exists for. The
// routes are the ones the control plane serves with the sandbox, which is
// where every hostname here comes from.
func googleSandbox(base string) *Snapshot {
	return &Snapshot{
		SandboxID:     "sbx_google",
		EnvironmentID: "env_1",
		Status:        "ready",
		Services: []Service{
			{Name: "google-calendar", URL: base + "/s/sbx_google/google-calendar", Status: "ready",
				Routes: []routes.Entry{
					{Host: "www.googleapis.com", Paths: []string{"/calendar/v3"}},
				}},
			{Name: "google-identity", URL: base + "/s/sbx_google/google-identity", Status: "ready",
				EnvHint: "GOOGLE_IDENTITY_BASE",
				Routes: []routes.Entry{
					{Host: "accounts.google.com", Paths: []string{"/o/oauth2/v2/auth"}},
					{Host: "oauth2.googleapis.com", Paths: []string{"/revoke", "/token", "/tokeninfo"}},
					{Host: "www.googleapis.com", Paths: []string{"/oauth2/v3"}},
				}},
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

// The measured record the control plane serves puts /tokeninfo on
// oauth2.googleapis.com. A hand-written table had it on www.googleapis.com,
// which would route a client's token introspection at a host that does not
// serve it.
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
			{Name: "stripe", URL: "http://sandbox.test/s/sbx_mixed/stripe",
				Routes: []routes.Entry{{Host: "api.stripe.com"}}},
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

func TestAServiceTheControlPlaneServedNoHostnameForIsReported(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_x",
		Services: []Service{
			{Name: "stripe", URL: "http://sandbox.test/s/sbx_x/stripe",
				Routes: []routes.Entry{{Host: "api.stripe.com"}}},
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

// The precedence, with the binary carrying no table of its own: what the
// control plane served is routed, and a service it served nothing for is not
// proxied at all -- handed over under its env hint rather than routed from
// some second, older copy of the hostnames.
func TestTheControlPlanesRoutesAreRoutedAndAServiceWithNoneIsHandedOver(t *testing.T) {
	snapshot := googleSandbox("http://sandbox.test")
	snapshot.Services[0].Routes = []routes.Entry{
		{Host: "calendar.fresh.example", Paths: []string{"/v9"}},
	}
	snapshot.Services[1].Routes = nil
	cfg, skipped, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	var calendarHosts []string
	for _, svc := range cfg.Services {
		if svc.Name == "google-calendar" {
			calendarHosts = append(calendarHosts, svc.Hosts...)
		}
		if svc.Name == "google-identity" {
			t.Errorf("google-identity was routed at %v, but the control plane served no hostname for it", svc.Hosts)
		}
	}
	if len(calendarHosts) != 1 || calendarHosts[0] != "calendar.fresh.example" {
		t.Errorf("calendar hosts = %v, want only the served route", calendarHosts)
	}
	if len(cfg.PassEnv) != 1 || cfg.PassEnv[0].Name != "GOOGLE_IDENTITY_BASE" ||
		cfg.PassEnv[0].Service != "google-identity" {
		t.Fatalf("pass_env = %+v, want google-identity handed over under its hint", cfg.PassEnv)
	}
	reason := ""
	for _, s := range skipped {
		if s.Service == "google-identity" {
			reason = s.Reason
		}
	}
	if !strings.Contains(reason, "the control plane served no route for it") ||
		!strings.Contains(reason, "$GOOGLE_IDENTITY_BASE") {
		t.Errorf("google-identity's note %q says neither why nor under what name", reason)
	}
}

// A newer control plane with one malformed row must not take down every run
// against it: the row is dropped. A service whose rows ALL drop is left with
// no hostname, which is the handed-over case, not a fall back to anything.
func TestAMalformedServedRouteIsDroppedRatherThanFailingTheRun(t *testing.T) {
	snapshot := googleSandbox("http://sandbox.test")
	snapshot.Services[0].Routes = []routes.Entry{
		{Host: "  "},
		{Host: "calendar.fresh.example"},
	}
	snapshot.Services[1].Routes = []routes.Entry{{Host: "  "}}
	cfg, _, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, svc := range cfg.Services {
		hosts = append(hosts, svc.Hosts...)
	}
	if len(hosts) != 1 || hosts[0] != "calendar.fresh.example" {
		t.Errorf("routed hosts = %v, want the one good row alone", hosts)
	}
	if len(cfg.PassEnv) != 1 || cfg.PassEnv[0].Service != "google-identity" {
		t.Errorf("pass_env = %+v, want the service whose rows all dropped handed over", cfg.PassEnv)
	}
}

// --route replaces the routes the control plane served for the service it
// names, and can route a service the control plane serves no hostname for --
// the "measurement has not landed yet" case the flag exists for.
func TestARouteOverrideReplacesAndEnables(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_o",
		Services: []Service{
			{Name: "stripe", URL: "http://sandbox.test/s/sbx_o/stripe",
				Routes: []routes.Entry{{Host: "api.stripe.com"}}},
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
	// One entry per service, not two: an override that MERGED with the served
	// route would leave stripe on api.stripe.com as well, and a map keyed by
	// name would hide it.
	if len(cfg.Services) != 2 {
		t.Fatalf("services = %+v, want one entry each -- the override replaces, never merges", cfg.Services)
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
			{Name: "stripe", URL: "http://gw/s/sbx_db/stripe", EnvHint: "STRIPE_API_BASE",
				Routes: []routes.Entry{{Host: "api.stripe.com"}}},
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

// The shape this binary's missing table makes possible: a control plane that
// serves no hostname for any http twin. Nothing is intercepted, every twin
// with a hint is handed over instead, and the run proceeds -- one note per
// twin is the whole signal. Pinning that here is what makes a later decision
// to refuse such a run, or to warn once for the whole run, a deliberate
// change rather than an accident.
func TestASandboxWithNoRoutedTwinHandsEveryTwinOverAndProceeds(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_none", EnvironmentID: "env_none",
		Services: []Service{
			{Name: "stripe", URL: "http://gw/s/sbx_none/stripe", EnvHint: "STRIPE_API_BASE"},
			{Name: "github", URL: "http://gw/s/sbx_none/github", EnvHint: "GITHUB_API_BASE"},
		},
	}
	cfg, skipped, err := ToConfig(snapshot, nil)
	if err != nil {
		t.Fatalf("a sandbox with no served hostnames still hands its twins over, got: %v", err)
	}
	if len(cfg.Services) != 0 {
		t.Errorf("routed services = %+v, want nothing intercepted", cfg.Services)
	}
	if len(cfg.PassEnv) != 2 {
		t.Fatalf("pass_env = %+v, want both twins handed over", cfg.PassEnv)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped = %+v, want one note per twin", skipped)
	}
	for _, s := range skipped {
		if !strings.Contains(s.Reason, "handed to the command as $") {
			t.Errorf("%s's note %q never names the variable it is handed under", s.Service, s.Reason)
		}
	}
}

// An http twin with no hostname to intercept -- the control plane served
// none, and there is no other source -- is the DSN case again: handed over
// under its env hint rather than silently left unreachable. One with no hint
// is still only reported, as before.
func TestAnHTTPTwinWithNoRouteIsHandedOverLikeADSN(t *testing.T) {
	snapshot := &Snapshot{
		SandboxID: "sbx_yente", EnvironmentID: "env_y",
		Services: []Service{
			{Name: "stripe", URL: "http://gw/s/sbx_yente/stripe", EnvHint: "STRIPE_API_BASE",
				Routes: []routes.Entry{{Host: "api.stripe.com"}}},
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
	// The full sentence: it is the only recourse a twin that can be neither
	// intercepted nor handed over ever gets, and --route is offered in it.
	if reasons["nameless"] != "no route: the control plane served no hostname "+
		"for it (--route nameless=<host> supplies one for this run)" {
		t.Errorf("a twin with no hint is reported, not handed: %q", reasons["nameless"])
	}

	// The same rule, asked about one service at a time.
	for _, c := range []struct {
		svc  Service
		want bool
	}{
		{Service{Name: "stripe", URL: "http://gw/stripe",
			Routes: []routes.Entry{{Host: "api.stripe.com"}}}, false},
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
