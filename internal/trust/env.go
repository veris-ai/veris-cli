// Package trust emits the environment a process under test needs in order to
// route through the Veris proxy and trust its CA.
//
// There is no standard for HTTP_PROXY or for CA-bundle environment variables.
// Every runtime picked its own convention, or none. This package encodes the
// full matrix in one place so the CLI does not have to.
package trust

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"runtime"
	"sort"
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

// Options describes the running proxy.
type Options struct {
	ProxyURL   string
	CACertPath string
	// JavaTrustStore is the path to a JKS copy of the JDK's cacerts with the
	// Veris CA added. Empty if one has not been built, in which case the Java
	// variables are omitted rather than emitted broken.
	JavaTrustStore     string
	JavaTrustStorePass string
	SandboxID          string
	CanaryToken        string
	// NoProxy is appended to the built-in loopback and private-range list.
	NoProxy []string
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
		{"NODE_EXTRA_CA_CERTS", o.CACertPath, "Node, including global fetch and undici; read once at startup", false},
		{"REQUESTS_CA_BUNDLE", o.CACertPath, "Python requests; it does NOT read SSL_CERT_FILE", false},
		{"SSL_CERT_FILE", o.CACertPath, "Python httpx, Go on Linux, OpenSSL CLI", false},
		{"CURL_CA_BUNDLE", o.CACertPath, "curl, and requests' fallback", false},
		{"GIT_SSL_CAINFO", o.CACertPath, "git over https", false},
		{"AWS_CA_BUNDLE", o.CACertPath, "AWS SDKs and CLI", false},
		{"CARGO_HTTP_CAINFO", o.CACertPath, "cargo", false},
		{"DENO_CERT", o.CACertPath, "Deno", false},
		{"PIP_CERT", o.CACertPath, "pip", false},
		{"npm_config_cafile", o.CACertPath, "npm registry traffic", false},

		// Node needs an explicit opt-in before its global fetch honours the
		// proxy variables above. Available from Node 22.21 and 24.5; harmless
		// on older versions, which ignore the unknown flag inside NODE_OPTIONS.
		{"NODE_OPTIONS", "--use-env-proxy", "Node's global fetch ignores HTTP_PROXY without this", true},

		// Veris identity, for the canary assertion. Note the names avoid the
		// substrings KEY, SECRET and TOKEN: Codex CLI strips any inherited
		// variable containing them, which would silently break the probe.
		{"VERIS_PROXY_URL", o.ProxyURL, "used by the canary probe", false},
		{"VERIS_SANDBOX_ID", o.SandboxID, "identifies the dependency sandbox in use", false},
	}

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

	return vars
}

// Warnings lists cases this environment cannot cover, so the CLI can print
// them rather than let a developer discover them as a mystery TLS error.
//
// Every entry here is a place where the environment-variable approach is known
// to fail and the container runner is the answer.
func Warnings(o Options) []string {
	var w []string

	if runtime.GOOS == "darwin" {
		w = append(w, "Go binaries on macOS ignore SSL_CERT_FILE and verify through Security.framework. "+
			"A Go service under test will not trust the Veris CA here. Run it under `veris test --container`, "+
			"or install the CA into the login keychain once.")
	}
	if o.JavaTrustStore == "" {
		w = append(w, "No Java truststore built, so JVM processes are not covered. "+
			"Run `veris-proxy trust --java` to create one from your JDK's cacerts.")
	}
	w = append(w,
		"Apache HttpClient built with HttpClients.createDefault() ignores the JVM proxy properties; "+
			"only createSystem() reads them. Such a service needs the container runner.",
		"Python aiohttp ignores proxy env vars unless the session is built with trust_env=True.",
		"The Stripe Python and Ruby SDKs ship their own CA bundle and ignore REQUESTS_CA_BUNDLE; "+
			"set stripe.ca_bundle_path, or use the container runner.",
	)
	return w
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

// Format renders vars in the requested shell format.
func Format(w io.Writer, vars []Var, format string, explain bool) error {
	switch format {
	case "", "posix", "sh", "bash", "zsh":
		return formatPosix(w, vars, explain)
	case "fish":
		return formatFish(w, vars, explain)
	case "powershell", "pwsh":
		return formatPowerShell(w, vars, explain)
	case "dotenv", "env":
		return formatDotenv(w, vars, explain)
	case "json":
		return json.NewEncoder(w).Encode(vars)
	case "github":
		// Writes to $GITHUB_ENV format.
		for _, v := range vars {
			if _, err := fmt.Fprintf(w, "%s=%s\n", v.Name, v.Value); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown format %q (want posix, fish, powershell, dotenv, json or github)", format)
	}
}

func formatPosix(w io.Writer, vars []Var, explain bool) error {
	for _, v := range vars {
		if explain {
			if _, err := fmt.Fprintf(w, "# %s\n", v.Reason); err != nil {
				return err
			}
		}
		var err error
		if v.Append {
			// Preserve whatever the developer or their CI already set.
			_, err = fmt.Fprintf(w, "export %s=\"${%s:+$%s }%s\"\n", v.Name, v.Name, v.Name, shellEscape(v.Value))
		} else {
			_, err = fmt.Fprintf(w, "export %s=%s\n", v.Name, shellQuote(v.Value))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func formatFish(w io.Writer, vars []Var, explain bool) error {
	for _, v := range vars {
		if explain {
			if _, err := fmt.Fprintf(w, "# %s\n", v.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "set -gx %s %s\n", v.Name, shellQuote(v.Value)); err != nil {
			return err
		}
	}
	return nil
}

func formatPowerShell(w io.Writer, vars []Var, explain bool) error {
	for _, v := range vars {
		if explain {
			if _, err := fmt.Fprintf(w, "# %s\n", v.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "$env:%s = %s\n", v.Name, psQuote(v.Value)); err != nil {
			return err
		}
	}
	return nil
}

func formatDotenv(w io.Writer, vars []Var, explain bool) error {
	sorted := append([]Var{}, vars...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, v := range sorted {
		if explain {
			if _, err := fmt.Fprintf(w, "# %s\n", v.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "%s=%s\n", v.Name, v.Value); err != nil {
			return err
		}
	}
	return nil
}

func shellQuote(s string) string  { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func shellEscape(s string) string { return strings.ReplaceAll(s, `"`, `\"`) }
func psQuote(s string) string     { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
