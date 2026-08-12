package trust

import (
	"strings"
	"testing"
)

func TestJavaNonProxyHostsMirrorsNoProxy(t *testing.T) {
	got := javaNonProxyHosts([]string{"repo.maven.apache.org", "*.internal.example.com"})

	for _, want := range []string{
		"localhost",
		"127.*",                // 127.0.0.1 broadened to Java's own loopback default
		"[::1]",                // IPv6 loopback needs brackets in nonProxyHosts syntax
		"10.*",                 // 10.0.0.0/8
		"172.16.*", "172.31.*", // the /12 enumerates its /16s, ends inclusive
		"192.168.*",
		"169.254.*",
		"host.docker.internal",
		"repo.maven.apache.org",
		"*.internal.example.com", // wildcard syntax is shared, passes through
	} {
		if !containsPattern(got, want) {
			t.Errorf("nonProxyHosts missing %q in %q", want, got)
		}
	}
	if containsPattern(got, "172.32.*") {
		t.Errorf("172.16.0.0/12 over-expanded past its range: %q", got)
	}
	if strings.Contains(got, "/") {
		t.Errorf("CIDR notation leaked into nonProxyHosts: %q", got)
	}
}

func TestJavaToolOptionsUsesSharedNoProxyList(t *testing.T) {
	vars := Build(Options{
		ProxyURL:           "http://127.0.0.1:8080",
		CACertPath:         "/tmp/ca.pem",
		JavaTrustStore:     "/tmp/store",
		JavaTrustStorePass: "changeit",
		NoProxy:            []string{"repo.maven.apache.org"},
	})
	var jto string
	for _, v := range vars {
		if v.Name == "JAVA_TOOL_OPTIONS" {
			jto = v.Value
		}
	}
	if jto == "" {
		t.Fatal("JAVA_TOOL_OPTIONS not emitted despite a truststore being set")
	}
	// The allow_passthrough host must reach Java too, or a build tool running
	// on the JVM would still send registry traffic into the strict proxy.
	if !strings.Contains(jto, "repo.maven.apache.org") {
		t.Errorf("nonProxyHosts in JAVA_TOOL_OPTIONS lacks the NoProxy host: %q", jto)
	}
	if !strings.Contains(jto, "-Djavax.net.ssl.trustStore=/tmp/store") {
		t.Errorf("truststore flag missing: %q", jto)
	}
}

func containsPattern(joined, pattern string) bool {
	for _, p := range strings.Split(joined, "|") {
		if p == pattern {
			return true
		}
	}
	return false
}

// Every variable that REPLACES a runtime's roots must carry the bundle, not
// the bare Veris certificate — the bare cert would break passthrough HTTPS.
// The additive NODE_EXTRA_CA_CERTS is the one exception.
func TestReplacingVarsGetTheBundleInEveryTier(t *testing.T) {
	opts := Options{
		ProxyURL:     "http://127.0.0.1:8080",
		CACertPath:   "/share/veris-ca.pem",
		CABundlePath: "/share/veris-ca-bundle.pem",
	}
	for _, name := range []string{
		"REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CURL_CA_BUNDLE",
		"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", "BUNDLE_SSL_CA_CERT",
		"COMPOSER_CAFILE", "HEX_CACERTS_PATH", "JULIA_SSL_CA_ROOTS_PATH",
		"NIX_SSL_CERT_FILE", "PERL_LWP_SSL_CA_FILE",
		"CLOUDSDK_CORE_CUSTOM_CA_CERTS_FILE",
	} {
		for _, trustOnly := range []bool{false, true} {
			opts.TrustOnly = trustOnly
			value := ""
			for _, v := range Build(opts) {
				if v.Name == name {
					value = v.Value
				}
			}
			if value != "/share/veris-ca-bundle.pem" {
				t.Errorf("trustOnly=%v: %s = %q, want the bundle", trustOnly, name, value)
			}
		}
	}
	for _, trustOnly := range []bool{false, true} {
		opts.TrustOnly = trustOnly
		for _, v := range Build(opts) {
			if v.Name == "NODE_EXTRA_CA_CERTS" && v.Value != "/share/veris-ca.pem" {
				t.Errorf("trustOnly=%v: NODE_EXTRA_CA_CERTS = %q, want the bare cert (it ADDS)", trustOnly, v.Value)
			}
		}
	}
}

func TestPassThroughIsEmittedInEveryTier(t *testing.T) {
	opts := Options{
		ProxyURL: "http://127.0.0.1:1", CACertPath: "/ca.pem",
		PassThrough: []PassThrough{
			{Name: "DATABASE_URL", Value: "postgres://u:p@h:5432/app", Service: "postgres"},
		},
	}
	for _, trustOnly := range []bool{false, true} {
		opts.TrustOnly = trustOnly
		var got *Var
		for _, v := range Build(opts) {
			if v.Name == "DATABASE_URL" {
				got = &v
				break
			}
		}
		if got == nil || got.Value != "postgres://u:p@h:5432/app" {
			t.Fatalf("trustOnly=%v: DATABASE_URL missing or wrong: %+v", trustOnly, got)
		}
	}
}
