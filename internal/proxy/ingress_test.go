package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// appOn stands in for the code under test, recording what the vendor sent it.
func appOn(t *testing.T, h http.HandlerFunc) (port int, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port, srv
}

func TestACallbackReachesTheAppAndIsCounted(t *testing.T) {
	var gotBody, gotHost, gotMethod string
	port, _ := appOn(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotHost, gotMethod = string(b), r.Host, r.Method
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	in, err := NewIngress("", port)
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	req, _ := http.NewRequest("POST", edge.URL+"/hooks/stripe",
		strings.NewReader(`{"type":"charge.succeeded"}`))
	req.Host = "odd-forest-1a2b.trycloudflare.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		t.Errorf("status %d", res.StatusCode)
	}
	if gotBody != `{"type":"charge.succeeded"}` {
		t.Errorf("the app saw body %q", gotBody)
	}
	if gotMethod != "POST" {
		t.Errorf("the app saw method %q", gotMethod)
	}
	// A client verifying a signature over the request has to see what the
	// vendor sent, not the hostname the tunnel edge rewrote it to.
	if gotHost != "odd-forest-1a2b.trycloudflare.com" {
		t.Errorf("Host reached the app as %q, want the sender's", gotHost)
	}

	r := in.Receipt()
	if r.Total != 1 || r.Delivered != 1 || r.Failed != 0 {
		t.Fatalf("receipt = %+v", r)
	}
	if len(r.Callbacks) != 1 || r.Callbacks[0].Path != "/hooks/stripe" ||
		r.Callbacks[0].Status != 200 || r.Callbacks[0].Count != 1 {
		t.Errorf("callbacks = %+v", r.Callbacks)
	}
	if r.ByPath["/hooks/stripe"] != 1 {
		t.Errorf("ByPath = %+v", r.ByPath)
	}
}

// A callback the app rejected still ARRIVED. Conflating the two would let a
// broken handler read as a vendor that never sent anything.
func TestARejectedCallbackCountsAsArrivedButNotDelivered(t *testing.T) {
	port, _ := appOn(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	in, _ := NewIngress("", port)
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	res, err := http.Post(edge.URL+"/hooks/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	r := in.Receipt()
	if r.Total != 1 {
		t.Errorf("an arriving callback must be counted: %+v", r)
	}
	if r.Delivered != 0 {
		t.Errorf("a 500 from the app is not a delivered callback: %+v", r)
	}
	if r.Callbacks[0].Status != 500 {
		t.Errorf("the app's status should be recorded: %+v", r.Callbacks)
	}
}

// The app not listening yet is the ordinary case, and it must not look like a
// vendor that sent nothing.
func TestAnUnreachableAppIsReportedRatherThanSwallowed(t *testing.T) {
	// A port nothing is on.
	in, err := NewIngress("", 9)
	if err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	res, err := http.Post(edge.URL+"/hooks/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", res.StatusCode)
	}
	if !strings.Contains(string(body), "veris_app_unreachable") {
		t.Errorf("the failure should name itself: %s", body)
	}
	r := in.Receipt()
	if r.Total != 1 || r.Failed != 1 || r.Delivered != 0 {
		t.Errorf("receipt = %+v", r)
	}
}

func TestAPortThatIsNotAPortIsRefused(t *testing.T) {
	for _, p := range []int{0, -1, 70000} {
		if _, err := NewIngress("", p); err == nil {
			t.Errorf("NewIngress(%d) was accepted", p)
		}
	}
}

// Repeats aggregate rather than filling the receipt with one row each.
func TestRepeatedCallbacksAggregate(t *testing.T) {
	port, _ := appOn(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	in, _ := NewIngress("", port)
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	for range 3 {
		res, err := http.Post(edge.URL+"/hooks/dup", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}

	r := in.Receipt()
	if r.Total != 3 || len(r.Callbacks) != 1 || r.Callbacks[0].Count != 3 {
		t.Errorf("receipt = %+v", r)
	}
}

// The proxy usually runs in a container while the app runs beside it in a
// shared namespace, where loopback is right. An app on the HOST is the other
// case, and loopback there is the proxy container's own -- measured, connection
// refused -- so the origin has to be nameable.
func TestTheOriginHostIsNameableForAnAppOnTheHost(t *testing.T) {
	in, err := NewIngress("host.docker.internal", 3222)
	if err != nil {
		t.Fatal(err)
	}
	if in.origin.Host != "host.docker.internal:3222" {
		t.Errorf("origin = %q", in.origin.Host)
	}
	def, err := NewIngress("", 3000)
	if err != nil {
		t.Fatal(err)
	}
	if def.origin.Host != "127.0.0.1:3000" {
		t.Errorf("default origin = %q, want loopback", def.origin.Host)
	}
}

// An app that accepts a protocol upgrade must still get one. ReverseProxy
// hijacks through http.ResponseController, which needs Unwrap to reach the real
// writer; without it the upgrade becomes a 502.
func TestAnUpgradeSurvivesTheRecorder(t *testing.T) {
	port, _ := appOn(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Connection"), "Upgrade") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "veris-echo")
		w.WriteHeader(http.StatusSwitchingProtocols)
	})

	in, _ := NewIngress("", port)
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	req, _ := http.NewRequest("GET", edge.URL+"/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "veris-echo")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101 -- the wrapper broke the upgrade", res.StatusCode)
	}
}

// 103 Early Hints arrives before the real answer. Latching it would make the
// receipt report a hint rather than whether the callback was accepted.
func TestAnInformationalResponseDoesNotBecomeTheRecordedStatus(t *testing.T) {
	port, _ := appOn(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusCreated)
	})

	in, _ := NewIngress("", port)
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	res, err := http.Post(edge.URL+"/hooks/hinted", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	r := in.Receipt()
	if len(r.Callbacks) != 1 || r.Callbacks[0].Status != http.StatusCreated {
		t.Errorf("recorded %+v, want the final 201", r.Callbacks)
	}
	if r.Delivered != 1 {
		t.Errorf("a 201 is a delivered callback: %+v", r)
	}
}

// Baseline that is recorded but not subtracted is worse than none: the
// confirmation probe then makes Delivered nonzero and satisfies
// --require-callback '*' with no real callback at all.
func TestABaselinedProbeDoesNotSatisfyARequirement(t *testing.T) {
	port, _ := appOn(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	in, _ := NewIngress("", port)
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	// the sandbox's probe
	res, err := http.Get(edge.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	in.Baseline()

	r := in.Receipt()
	if r.Total != 0 || r.Delivered != 0 {
		t.Fatalf("the probe still counts as run traffic: %+v", r)
	}
	if len(r.Callbacks) != 0 {
		t.Errorf("callbacks = %+v, want none", r.Callbacks)
	}

	// a real callback afterwards must count
	res, err = http.Post(edge.URL+"/hooks/real", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	r = in.Receipt()
	if r.Total != 1 || r.Delivered != 1 || r.DeliveredByPath["/hooks/real"] != 1 {
		t.Errorf("a real callback after the baseline must count: %+v", r)
	}
}

// The confirmation probe runs when the app is already listening, so a real
// callback can race it. Discounting the whole receipt there loses that
// callback; only the probe itself may be excluded.
func TestOnlyTheProbeIsDiscountedNotTheCallbacksRacingIt(t *testing.T) {
	port, _ := appOn(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	in, _ := NewIngress("", port)
	edge := httptest.NewServer(in.Handler())
	defer edge.Close()

	// a genuine callback arrives, then the confirmation probe
	for _, path := range []string{"/hooks/real", "/"} {
		res, err := http.Post(edge.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	in.DiscountOne("POST", "/", 200, true)

	r := in.Receipt()
	if r.Total != 1 || r.Delivered != 1 {
		t.Fatalf("the racing callback was lost: %+v", r)
	}
	if r.DeliveredByPath["/hooks/real"] != 1 {
		t.Errorf("the real callback must survive: %+v", r.DeliveredByPath)
	}
}

// An IPv6 origin without brackets produces http://::1:8080, which nothing can
// dial.
func TestAnIPv6OriginIsBracketed(t *testing.T) {
	in, err := NewIngress("::1", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if in.origin.Host != "[::1]:8080" {
		t.Errorf("origin = %q, want a bracketed IPv6 authority", in.origin.Host)
	}
}

// The probe is discounted before it is issued, so a receipt read while it is
// still in flight must not report negative counts.
func TestAReceiptIsNeverNegative(t *testing.T) {
	in, _ := NewIngress("", 9)
	in.DiscountOne("GET", "/", 200, true)

	r := in.Receipt()
	if r.Total < 0 || r.Delivered < 0 || r.Failed < 0 {
		t.Fatalf("negative receipt: %+v", r)
	}
}

// `failed` counts transport failures only, so discounting an app's 4xx must
// not drive it negative.
func TestAnAppRejectionDoesNotDriveFailedNegative(t *testing.T) {
	in, _ := NewIngress("", 9)
	in.DiscountOne("GET", "/", 404, false)

	if r := in.Receipt(); r.Failed != 0 {
		t.Errorf("Failed = %d, want 0", r.Failed)
	}
}
