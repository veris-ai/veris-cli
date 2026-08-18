package proxy

import (
	"net/url"
	"testing"

	"github.com/veris-ai/veris-proxy/internal/config"
)

// The anchoring is load-bearing: five services once hand-wrote a bare
// startswith("/veris") and made /verisign-callback and /version reachable as
// control plane. The proxy must not repeat the bug from the other side by
// counting those vendor paths as harness traffic.
func TestIsControlPlanePathAnchorsThePrefix(t *testing.T) {
	for path, want := range map[string]bool{
		"/veris":             true,
		"/veris/data":        true,
		"/veris/requests":    true,
		"/verisign-callback": false,
		"/version":           false,
		"/v1/charges":        false,
		"/":                  false,
	} {
		if got := IsControlPlanePath(path); got != want {
			t.Errorf("IsControlPlanePath(%q) = %v, want %v", path, got, want)
		}
	}
}

// Vendor-surface and control-plane hits on one service stay separate all the
// way through the snapshot: folding them together is how a harness's /veris/*
// seeding once satisfied --require-service while every SDK call failed TLS.
func TestReceiptCountsControlPlaneApart(t *testing.T) {
	upstream, _ := url.Parse("http://sandbox.internal")
	target := &config.Target{Service: "stripe", Upstream: upstream, Prefix: "/"}

	var r receipt
	r.record("api.stripe.com", target, false)
	r.record("api.stripe.com", target, true)
	r.record("api.stripe.com", target, true)

	got := r.snapshot()
	if got.Total != 1 || got.ByService["stripe"] != 1 || got.ByHost["api.stripe.com"] != 1 {
		t.Errorf("vendor-surface counts wrong: %+v", got)
	}
	if got.ControlTotal != 2 || got.ByServiceControl["stripe"] != 2 {
		t.Errorf("control-plane counts wrong: control_total=%d by_service_control=%+v",
			got.ControlTotal, got.ByServiceControl)
	}
	if len(got.Hits) != 2 {
		t.Fatalf("hits = %+v, want one vendor and one control entry", got.Hits)
	}
}
