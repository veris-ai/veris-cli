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
