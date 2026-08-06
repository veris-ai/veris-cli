# veris-proxy

Routes outbound HTTP(S) from code under test at simulated services in a Veris
dependency sandbox. The code under test is never modified.

A single static binary with no runtime dependencies, so it drops into any
container image including Alpine and distroless.

## Why a proxy rather than a base-URL override

Overriding `stripe.api_base` or an equivalent means the code path under test is
not the code path that ships. For a product whose premise is catching
integration bugs that unit mocks miss, testing a modified code path
reintroduces the exact gap it exists to close. The proxy is what makes the test
faithful.

## Quick start

```sh
veris-proxy serve --config .veris/proxy.json &
eval "$(veris-proxy env --config .veris/proxy.json)"
veris-proxy check          # exits non-zero if interception is not live
pytest -q
```

## Commands

| Command | Purpose |
|---|---|
| `serve` | Run the proxy. Add `--transparent` for kernel-redirected traffic. |
| `env` | Print the environment the process under test needs. |
| `check` | Probe a running proxy. Exit 2 if interception is not live. |
| `trust` | Build a Java truststore (`--java`), or add the CA to an app-managed keystore (`--inject`). |
| `ca` | Create or inspect the local interception CA. |

## Config

JSON rather than YAML, because the Veris CLI owns the human-facing `veris.yaml`
and compiles it down to this. That keeps the binary dependency-free and the
wire format unambiguous. See `examples/proxy.json`.

```json
{
  "version": 1,
  "listen": "127.0.0.1:8080",
  "sandbox_id": "sbx_abc123",
  "mode": "strict",
  "upstream": { "base_url": "https://sandbox.veris.ai" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com", "*.stripe.com"] }
  ]
}
```

Host matching is exact or a single leading `*.` wildcard. Exact always beats
wildcard, so `api.stripe.com` can route differently from `*.stripe.com`. Two
services claiming the same host is rejected at load rather than resolved by
declaration order.

`allow_passthrough` accepts the preset `"@build"`, which expands to the
package registries a build tool needs (Maven Central, Gradle, npm, PyPI, Go,
crates.io, RubyGems, NuGet, Packagist). Without it, running tests behind the
strict proxy means resolving dependencies with interception off and then
forcing the build tool offline; with it, dependency traffic flows around the
proxy while the strict-mode guarantee stays auditable — these exact hosts and
nothing else. The list is first-party registry hosts only; a project on a
private registry adds its own host next to the preset.

An unresolved service upstream is derived as
`{base_url}/s/{sandbox_id}/{service}`.

## Two design decisions worth knowing

### Strict mode is the default

An unmapped host is blocked with a 502 and an actionable error. It is not
forwarded to the real internet.

This is the property the whole product depends on. If the code under test can
still reach real Stripe, a green run proves nothing, and a green run that
proves nothing is worse than a red one. `mode: passthrough` exists for
discovering a service's dependencies, and logs a warning on every use.

### The canary makes a silent no-op impossible

`veris-proxy check` asserts on a per-run token before your tests run. It fails
if the proxy is unreachable, if it is not a Veris proxy, or if it belongs to a
different run.

That last case is the one worth having: a proxy left running from an earlier
run, pointing at a different sandbox, would otherwise let tests pass against
the wrong simulated data. Without this check, "interception silently did not
happen" and "everything worked" look identical.

## Two tiers of interception

### Explicit proxy, on the host

`veris-proxy env` emits the full matrix of proxy and CA variables. There is no
standard for any of them, so each runtime needs its own:

| Runtime | Proxy | CA |
|---|---|---|
| Python requests / httpx | env | `REQUESTS_CA_BUNDLE` / `SSL_CERT_FILE` |
| Go | env | `SSL_CERT_FILE` (Linux only) |
| Node | needs `--use-env-proxy` | `NODE_EXTRA_CA_CERTS` |
| .NET | env | `SSL_CERT_FILE` (Linux only) |
| Java | `JAVA_TOOL_OPTIONS`, after `trust --java` | JKS truststore, not a PEM |

Java deserves its own paragraph because it reads none of the usual variables.
`veris-proxy trust --java` copies the JDK's cacerts and imports the Veris CA;
`env` finds the result and emits `JAVA_TOOL_OPTIONS` with the `-D` proxy,
`nonProxyHosts` and truststore flags, which every JVM — including Gradle and
Maven test forks — picks up from the environment. An app that loads its own
keystore from disk (the k8s-mounted `keystore.p12` pattern) never consults the
JVM default truststore, and custom trust managers wrapping such a keystore
have been observed to break outright when it is empty rather than fall back;
`trust --inject path/to/keystore.p12` puts the CA where the app actually
looks.

`env` prints what it cannot cover to stderr rather than letting you discover it
as a mystery TLS failure. Four cases are genuinely out of reach here: Go on
macOS ignores `SSL_CERT_FILE` and verifies through Security.framework; Apache
HttpClient built with `createDefault()` ignores the JVM proxy properties;
`aiohttp` ignores proxy variables without `trust_env=True`; and the Stripe
Python and Ruby SDKs ship their own CA bundle.

### Transparent, in a container

`--transparent` serves connections the kernel redirected via `iptables
REDIRECT`, taking the destination from TLS SNI or the `Host` header. Nothing
has to cooperate, so all four cases above are covered. With the CA in the
image's system trust store, the per-runtime matrix disappears entirely.

See `container/README.md`. Needs `--cap-add=NET_ADMIN`, not `--privileged`.

## Certificates

The CA is generated on first run into `~/.veris/ca`, reused thereafter, and the
key is written `0600`. Leaves are minted per host and cached in memory.

Two details that are easy to get wrong and are covered by tests: leaves are
served as **leaf + CA**, since Node and anything on OpenSSL reject a bare leaf
with `UNABLE_TO_VERIFY_LEAF_SIGNATURE`; and every leaf carries a SAN, since a
certificate with only a CN is rejected by every modern client.

## Development

```sh
make test    # unit and integration tests
make e2e     # against real curl, Python and Node clients
make dist    # static binaries for 5 platforms
```

`make e2e` matters because the Go tests exercise the proxy through Go's own TLS
stack, which is more forgiving than OpenSSL's.

## Licence

MIT. Built on [elazarl/goproxy](https://github.com/elazarl/goproxy) (BSD-3).
