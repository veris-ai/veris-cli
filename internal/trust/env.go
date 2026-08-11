// Package trust emits the environment a process under test needs in order to
// route through the Veris proxy and trust its CA.
//
// There is no standard for HTTP_PROXY or for CA-bundle environment variables.
// Every runtime picked its own convention, or none. This package encodes the
// full matrix in one place so the CLI does not have to.
package trust

import (
	"fmt"
	"net"
	"strings"
)

// Var is a single environment variable with the reason it is being set. The
// reason is surfaced by `veris proxy env --explain`, because a developer
// staring at twenty exported variables deserves to know why each one is there.
type Var struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
	// Append marks a variable that must be appended to any existing value
	// rather than replacing it. NODE_OPTIONS is the case that matters.
	Append bool `json:"append,omitempty"`
}

// PassThrough is a variable the sandbox hands to the command verbatim — a
// database DSN under the name the client's code already reads. Not routing
// and not trust, so every tier emits it, including trust-only.
type PassThrough struct {
	Name    string
	Value   string
	Service string
}

// Options describes the running proxy.
type Options struct {
	ProxyURL string
	// CACertPath is the bare Veris certificate, for the variables that ADD to a
	// runtime's own roots.
	CACertPath string
	// CABundlePath is the public roots plus the Veris certificate, for the
	// variables that REPLACE them. Empty falls back to CACertPath, which trusts
	// intercepted hosts and rejects every passthrough one -- so callers publish
	// a bundle rather than leaning on that.
	CABundlePath string
	// JavaTrustStore is the path to a JKS copy of the JDK's cacerts with the
	// Veris CA added. Empty if one has not been built, in which case the Java
	// variables are omitted rather than emitted broken.
	JavaTrustStore     string
	JavaTrustStorePass string
	SandboxID          string
	CanaryToken        string
	// NoProxy is appended to the built-in loopback and private-range list.
	NoProxy []string

	// NodeAcceptsEnvProxy reports whether the node on PATH tolerates
	// --use-env-proxy inside NODE_OPTIONS. Node did not gain it until 22.21 and
	// 24.5, and an older one does NOT ignore the unknown flag: it refuses to
	// start at all, so setting it unconditionally breaks every Node command the
	// run was supposed to instrument.
	NodeAcceptsEnvProxy bool

	// PublicURL is where the sandbox will deliver callbacks. Handed to the
	// code under test so it can register its own webhook THROUGH THE VENDOR
	// API -- which is the code path that ships, and the reason nothing here
	// writes a service's target_url on the client's behalf.
	PublicURL string

	// TrustOnly emits the CA variables and nothing that routes.
	//
	// For the kernel-redirect tiers, where the routing is already done below
	// every library. Setting HTTP_PROXY there is worse than redundant: a
	// client that honours it starts cooperating, so it takes the explicit
	// proxy path instead of the redirect -- a different transport from the one
	// being claimed -- and a redirect that silently stopped working would be
	// masked by the variables rather than showing up as a failure.
	TrustOnly bool

	// PassThrough is the non-HTTP sandbox surface (database DSNs), emitted in
	// EVERY mode including TrustOnly: the kernel redirect cannot route a wire
	// protocol the proxy does not speak, so the handoff is the coverage.
	PassThrough []PassThrough
}

// loopback plus the RFC1918 ranges. Without these, a test that starts its own
// service and calls it over the network would be routed to Veris.
var defaultNoProxy = []string{
	"localhost", "127.0.0.1", "::1", "0.0.0.0",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
	"host.docker.internal",
}

// Build returns every variable to set, in a stable order.
func Build(o Options) []Var {
	noProxy := strings.Join(append(append([]string{}, defaultNoProxy...), o.NoProxy...), ",")

	if o.TrustOnly {
		return append(buildTrustOnly(o), passThroughVars(o)...)
	}

	bundle := o.CABundlePath
	if bundle == "" {
		bundle = o.CACertPath
	}

	vars := []Var{
		// Proxy routing. Both cases are set because curl accepts only the
		// lowercase http_proxy (a consequence of the httpoxy CGI bug), while
		// other runtimes look for the uppercase form.
		{"HTTP_PROXY", o.ProxyURL, "Go net/http, .NET, axios, most SDKs", false},
		{"http_proxy", o.ProxyURL, "curl, git, PHP; lowercase-only for historical CGI-safety reasons", false},
		{"HTTPS_PROXY", o.ProxyURL, "same set, for https targets", false},
		{"https_proxy", o.ProxyURL, "curl and git, for https targets", false},
		// Deliberately an http:// URL. httpx rejects socks5h:// unless its
		// socks extra is installed, and this proxy is not a SOCKS proxy.
		{"ALL_PROXY", o.ProxyURL, "httpx and a few SDKs that read only this", false},
		{"all_proxy", o.ProxyURL, "lowercase variant", false},
		{"NO_PROXY", noProxy, "keep loopback and private ranges away from the sandbox", false},
		{"no_proxy", noProxy, "lowercase variant", false},

		// CA trust. Each runtime reads a different variable; none of them reads
		// another's.
		{"NODE_EXTRA_CA_CERTS", o.CACertPath, "Node, including global fetch and undici; ADDS to its own roots", false},
	}
	vars = append(vars, replacingCAVars(bundle)...)
	// Veris identity, for the canary assertion. Note the names avoid the
	// substrings KEY, SECRET and TOKEN: Codex CLI strips any inherited variable
	// containing them, which would silently break the probe.
	vars = append(vars,
		Var{"VERIS_PROXY_URL", o.ProxyURL, "used by the canary probe", false},
		Var{"VERIS_SANDBOX_ID", o.SandboxID, "identifies the dependency sandbox in use", false},
	)

	if o.NodeAcceptsEnvProxy {
		// Node's global fetch ignores HTTP_PROXY without this opt-in. Emitted
		// only when the node in play accepts it, because one that does not
		// refuses to start rather than ignoring it.
		vars = append(vars, Var{
			Name:   "NODE_OPTIONS",
			Value:  "--use-env-proxy",
			Reason: "Node's global fetch ignores HTTP_PROXY without this",
			Append: true,
		})
	}

	vars = append(vars, publicURLVar(o)...)

	if o.CanaryToken != "" {
		vars = append(vars, Var{
			Name:   "VERIS_CANARY",
			Value:  o.CanaryToken,
			Reason: "asserted by the probe to prove interception is live for this run",
		})
	}

	if o.JavaTrustStore != "" {
		// Java reads no proxy environment variable of any kind, and needs a
		// JKS rather than a PEM. JAVA_TOOL_OPTIONS is the documented injection
		// channel and propagates through Surefire and Gradle test forks.
		//
		// There is no https.nonProxyHosts; http.nonProxyHosts covers both.
		host, port := splitProxy(o.ProxyURL)
		opts := []string{
			"-Dhttp.proxyHost=" + host,
			"-Dhttp.proxyPort=" + port,
			"-Dhttps.proxyHost=" + host,
			"-Dhttps.proxyPort=" + port,
			"-Dhttp.nonProxyHosts=" + javaNonProxyHosts(o.NoProxy),
			"-Djavax.net.ssl.trustStore=" + o.JavaTrustStore,
			"-Djavax.net.ssl.trustStorePassword=" + o.JavaTrustStorePass,
		}
		vars = append(vars, Var{
			Name:   "JAVA_TOOL_OPTIONS",
			Value:  strings.Join(opts, " "),
			Reason: "Java reads no proxy env vars and needs a JKS truststore, not a PEM",
			Append: true,
		})
	}

	return append(vars, passThroughVars(o)...)
}

// passThroughVars hands over what interception cannot cover: each non-HTTP
// service's connection string, under the variable name the client's code
// already reads. Same value in every tier — this is configuration the twelve-
// factor way, not routing.
func passThroughVars(o Options) []Var {
	vars := make([]Var, 0, len(o.PassThrough))
	for _, p := range o.PassThrough {
		vars = append(vars, Var{
			Name:  p.Name,
			Value: p.Value,
			Reason: p.Service + " is not an HTTP service; its connection " +
				"string is handed over rather than proxied",
		})
	}
	return vars
}

// buildTrustOnly is the CA half: what a client needs in order to accept the
// proxy's certificate, and nothing that would tell it where to send anything.
func buildTrustOnly(o Options) []Var {
	bundle := o.CABundlePath
	if bundle == "" {
		bundle = o.CACertPath
	}
	vars := []Var{
		{"NODE_EXTRA_CA_CERTS", o.CACertPath, "Node, including global fetch and undici; ADDS to its own roots", false},
	}
	vars = append(vars, replacingCAVars(bundle)...)
	vars = append(vars, Var{
		Name: "VERIS_SANDBOX_ID", Value: o.SandboxID,
		Reason: "identifies the dependency sandbox in use",
	})
	vars = append(vars, publicURLVar(o)...)
	if o.CanaryToken != "" {
		vars = append(vars, Var{
			Name:   "VERIS_CANARY",
			Value:  o.CanaryToken,
			Reason: "asserted by the probe to prove interception is live for this run",
		})
	}
	if o.JavaTrustStore != "" {
		// The truststore flags only. The -Dhttp.proxyHost pair that Build
		// emits would route, which is the thing this deliberately does not do.
		vars = append(vars, Var{
			Name: "JAVA_TOOL_OPTIONS",
			Value: "-Djavax.net.ssl.trustStore=" + o.JavaTrustStore +
				" -Djavax.net.ssl.trustStorePassword=" + o.JavaTrustStorePass,
			Reason: "Java needs a JKS rather than a PEM, and reads no CA env var",
			Append: true,
		})
	}
	return vars
}

// publicURLVar tells the code under test where its callbacks will arrive, so
// its own registration call can name it.
func publicURLVar(o Options) []Var {
	if o.PublicURL == "" {
		return nil
	}
	return []Var{{
		Name: "VERIS_PUBLIC_URL", Value: o.PublicURL,
		Reason: "register this with the vendor as your webhook destination",
	}}
}

// replacingCAVars are the variables that REPLACE a runtime's trust roots rather
// than adding to them, so every one of them gets the bundle. Pointing any of
// them at the bare Veris certificate makes a client trust the intercepted hosts
// and reject the entire rest of the internet, which in passthrough mode is
// every unmapped vendor, package index and telemetry endpoint the run touches.
func replacingCAVars(bundle string) []Var {
	return []Var{
		{"REQUESTS_CA_BUNDLE", bundle, "Python requests; it does NOT read SSL_CERT_FILE", false},
		{"SSL_CERT_FILE", bundle, "Python httpx, Go on Linux, OpenSSL CLI", false},
		{"CURL_CA_BUNDLE", bundle, "curl, and requests' fallback", false},
		{"GIT_SSL_CAINFO", bundle, "git over https", false},
		{"AWS_CA_BUNDLE", bundle, "AWS SDKs and CLI", false},
		{"CARGO_HTTP_CAINFO", bundle, "cargo", false},
		{"DENO_CERT", bundle, "Deno", false},
		{"PIP_CERT", bundle, "pip", false},
		{"npm_config_cafile", bundle, "npm registry traffic", false},
	}
}

// javaNonProxyHosts renders the NO_PROXY list in http.nonProxyHosts syntax,
// which understands only "|"-separated hostname globs — no CIDR notation. The
// two lists must be derived from the same source, or Java processes would
// proxy hosts that everything else bypasses.
func javaNonProxyHosts(extra []string) string {
	var out []string
	for _, entry := range append(append([]string{}, defaultNoProxy...), extra...) {
		out = append(out, javaHostPatterns(entry)...)
	}
	return strings.Join(out, "|")
}

func javaHostPatterns(entry string) []string {
	entry = strings.TrimSpace(entry)
	switch {
	case entry == "":
		return nil
	case entry == "::1":
		return []string{"[::1]"}
	case entry == "127.0.0.1":
		// Broaden to the whole loopback block, matching Java's own default.
		return []string{"127.*"}
	case strings.Contains(entry, "/"):
		return cidrToJavaPatterns(entry)
	default:
		// Hostnames and *.wildcards are already valid nonProxyHosts syntax.
		return []string{entry}
	}
}

// cidrToJavaPatterns approximates an IPv4 CIDR with dotted wildcards. Octet-
// aligned prefixes map to a single pattern (10.0.0.0/8 -> 10.*); prefixes
// between /8 and /16 enumerate their /16s (172.16.0.0/12 -> 172.16.* through
// 172.31.*). Anything finer is dropped: Java's matcher cannot express it, and
// silently over-matching would be worse than not matching.
func cidrToJavaPatterns(entry string) []string {
	ip, ipnet, err := net.ParseCIDR(entry)
	if err != nil || ip.To4() == nil {
		return nil
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil
	}
	v4 := ip.To4()
	switch {
	case ones%8 == 0 && ones > 0 && ones <= 24:
		parts := strings.Split(v4.String(), ".")
		return []string{strings.Join(parts[:ones/8], ".") + ".*"}
	case ones > 8 && ones < 16:
		count := 1 << (16 - ones)
		out := make([]string, 0, count)
		for i := 0; i < count; i++ {
			out = append(out, fmt.Sprintf("%d.%d.*", v4[0], int(v4[1])+i))
		}
		return out
	default:
		return nil
	}
}

func splitProxy(proxyURL string) (host, port string) {
	s := strings.TrimPrefix(strings.TrimPrefix(proxyURL, "http://"), "https://")
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, ":"); i > 0 {
		return s[:i], s[i+1:]
	}
	return s, "80"
}
