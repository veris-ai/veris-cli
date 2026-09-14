// Package direct is how this binary reaches the control plane and the twins:
// through a proxy the developer configured, never through one this binary
// started.
package direct

import (
	"net/http"
	"net/url"
	"os"
)

// Transport is the transport every client in this binary talks to the
// control plane with. It honours a proxy the developer configured, and
// refuses one this binary started.
//
// Inside `veris up --proxy` the session's own environment names the Veris
// proxy in HTTPS_PROXY, and that proxy intercepts EVERY host, including ones
// it has no mapping for -- deliberately, so an unmapped host can be seen and
// blocked rather than tunnelling out unobserved. A veris command run in that
// session therefore reached its own control plane through its own proxy and
// was handed a certificate minted by the Veris CA. Whether that verifies
// depends on the platform: Go honours SSL_CERT_FILE on Linux and ignores it
// on macOS, where roots come from the keychain, so `veris down` inside a
// session failed there with "certificate signed by unknown authority" while
// working elsewhere.
//
// The routing was circular either way. The control plane is not a dependency
// of the code under test; it is the instrument, and an instrument that
// measures itself through its own instrument has no business doing so. A
// proxy the developer set for their own network is left alone, because
// reaching the plane may genuinely depend on it.
func Transport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = proxyExceptOurOwn
	return t
}

// proxyExceptOurOwn is http.ProxyFromEnvironment unless the proxy in the
// environment is one of ours, which VERIS_PROXY_URL names.
func proxyExceptOurOwn(req *http.Request) (*url.URL, error) {
	if os.Getenv("VERIS_PROXY_URL") != "" {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}
