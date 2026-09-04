package direct

import (
	"net/http"
	"testing"
)

// The control plane and the sandbox gateway share a hostname, so a veris
// command run inside `up --proxy` reached its own plane through its own
// proxy and was handed a certificate the Veris CA minted. A proxy the
// developer set for their own network is a different thing and is kept.
func TestOurOwnProxyIsRefusedAndTheDevelopersIsKept(t *testing.T) {
	req, err := http.NewRequest("GET", "https://svc.api.veris.ai/v1/sandboxes/abc", nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:8080")
	t.Setenv("VERIS_PROXY_URL", "http://127.0.0.1:8080")
	got, err := proxyExceptOurOwn(req)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a veris proxy was used for a control-plane call: %s", got)
	}

	t.Setenv("VERIS_PROXY_URL", "")
	got, err = proxyExceptOurOwn(req)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Host != "127.0.0.1:8080" {
		t.Errorf("the developer's own proxy was dropped: %v", got)
	}
}
