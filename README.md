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

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/veris-ai/veris-proxy/main/scripts/install.sh | sh
```

Fetches the latest released static binary for this OS/arch into
`~/.local/bin` (override with `VERIS_INSTALL_DIR`; pin with
`VERIS_PROXY_VERSION`). No root and no package manager, so the same line
works on a laptop, a CI runner, and inside a container build. Windows:
download the `.exe` from the releases page.

## Quick start

```sh
export VERIS_API_KEY=...
veris-proxy run --environment env_abc123 --image your-image -- pytest -q
```

Or with no command at all, letting the image's own `ENTRYPOINT` and `CMD` run —
which is what an application image is built to do:

```sh
veris-proxy run --environment env_abc123 --image your-image
```

One command, no docker of your own, nothing provisioned in advance.
`--environment` deploys a fresh sandbox of that environment for this run and
deletes it afterwards; the proxy starts in its own container, your image runs
in a second one sharing that network namespace, the output streams, the run
reports what the sandbox received, your command's exit code propagates, and
everything is torn down.

`--sandbox sbx_abc123` instead attaches to a sandbox that already exists —
the right shape when something seeded state into it first, or when several
suites share one world. Everything below works the same in either mode.

**Your image needs nothing** — no capability, no `iptables`, no entrypoint
change, no particular base. Distroless and scratch work. Every requirement sits
on the proxy container, which we build.

```
veris-proxy: interception live in veris-proxy-35681
   7 customers, first: gus.thornton@example.com
veris-proxy: the sandbox received 1 request(s):
  stripe                       1
```

Add `--require-service stripe` and the run exits 3 if your suite never called
it. Without it nothing is asserted — the receipt is still printed, so a run that
sent nothing says so, but the exit code stays your command's. `-v`, `-e` and
`-w` pass through to your container.

The proxy's own image comes from a repository holding that image and nothing
else, so pulling it grants nothing else; `--proxy-image` overrides it.
Pulling needs a logged-in gcloud (`gcloud auth login`; if docker then still
answers 401, `gcloud auth configure-docker us-central1-docker.pkg.dev` wires
docker to that login). The auth requirement goes away once the image is
published publicly.

See `container/README.md` for the two docker commands this is doing for you,
for running the proxy against an image you cannot restart, and for adding it to
an image you already have.

### Receiving webhooks

The proxy routes your code OUT to the sandbox. A webhook comes back IN, and a
sandbox in the cluster cannot reach an app on your laptop. `--expose` opens a
tunnel for that direction and registers it with the sandbox:

```sh
veris-proxy serve --sandbox sbx_abc123 --expose 3000
```

Ingress belongs to the session rather than to one command, so `serve` owns it.
`run --image` takes the same flags and forwards them to the proxy container,
where cloudflared reaches your image over the shared namespace's loopback.

The port is the one your app listens on. Your image starts only after the proxy
reports ready, so the registration probe necessarily runs before anything is
listening; the proxy waits for your port to open and re-probes, and the verdict
you read is the one taken then. Your app is
handed `VERIS_PUBLIC_URL` and registers that with the vendor itself — through
the vendor's own API, because that registration call is the code path that
ships.

```
veris-proxy: callbacks arrive at https://odd-forest-1a2b.trycloudflare.com
veris-proxy: callbacks registered  via=stripe
...
veris-proxy: your app received 2 callback(s):
  POST   /hooks/stripe                2 -> 200
```

`--require-callback /hooks/stripe` exits 3 if nothing arrived. Without it a
webhook suite that received nothing still passes, which is the same failure the
egress receipt exists to catch.

A quick tunnel needs no Cloudflare account and mints a new hostname each run.
`--expose-token` (plus `--expose-hostname`) uses a named tunnel instead, for a
stable URL. If your app runs on the HOST while the proxy is in a container, add
`--expose-host host.docker.internal` — loopback there is the container's own.

`testdata/e2e-ingress.sh` drives the whole path against a real sandbox and a
real tunnel, and asserts that the sandbox's own startup probe is EXCLUDED from
the receipt — otherwise `--require-callback '*'` would pass with no vendor
delivery at all.

### A sandbox of your own

`--environment` works for a long-lived `serve` session the same way it does
for `run`:

```sh
veris-proxy serve --environment env_abc123 --expose 3000
```

When you are receiving callbacks it is not just simpler but safer, for a
reason worth stating plainly: `client.default_base_url` is a sandbox-wide singleton. Two
concurrent runs sharing one sandbox overwrite each other's callback URL, and the
first run's webhooks are then delivered to the second run's app — silently, with
neither able to notice. A sandbox per run removes that entirely.

It also removes the registration window. The tunnel needs only a local port, so
it opens first and its URL is handed over at creation; the sandbox is never
alive without knowing where to deliver. `--ttl-minutes` bounds the sandbox's
life if teardown never runs, so a crashed run cannot leak one.

Attaching with `--sandbox` still works, and still registers by PATCH. It warns
when it replaces someone else's URL.

The URL is a capability, in either mode: anyone holding it can POST to your app.

### Without a container

`veris-proxy run` without `--image` is the fallback for work that is not containerised. It runs
your command as a local child process with proxy and CA environment variables
set, which is a *request* rather than an enforcement — a library that ignores
those variables reaches the real vendor. It does not cover Java, static Go
binaries, Apache HttpClient or aiohttp; the container path does.

```sh
veris-proxy run --sandbox sbx_abc123 -- pytest -q
```


## Commands

| Command | Purpose |
|---|---|
| `serve` | Run the proxy. This is what the container image runs. |
| `run` | Fallback: run a LOCAL command with proxy env vars, and report what it sent. |
| `check` | Assert a live proxy belongs to THIS run. Exit 2 if not. |

`serve --write-env FILE` and `serve --ready-file FILE` are how a supervisor
picks the proxy up: the environment as sourceable POSIX exports, and an
edge-triggered marker written last, so its existence means every listener is
bound and the environment is complete. The container entrypoint uses both, and
needs no other command — it cannot probe a port, because debian-slim and
python-slim carry no curl, wget or nc and `sh` has no `/dev/tcp`.

`--write-env` records the **bound** address, so `--listen :0` works; a separate
command computing it from config could not.

`run` exits with the command's own status, or 3 if a `--require-service` /
`--require-host` assertion went unmet, or 4 if the outcome is indeterminate.

`run`, `serve` and `env` all take the same routing flags, most explicit first:
`--config <file>` · `--sandbox <id>` · `$VERIS_PROXY_CONFIG` ·
`$VERIS_SANDBOX_ID`. The layers never merge. `serve --print-routes` shows what
a sandbox id resolves to without starting anything.

**`serve` is the primary command** — it is what the container image runs, and
what a long-lived local session runs. **`run` is the fallback**: it `fork`/
`exec`s a local command with the interception environment, which only covers
libraries that honour those variables.

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
    { "name": "stripe", "hosts": ["api.stripe.com", "*.stripe.com"] },
    { "name": "google-calendar", "hosts": ["www.googleapis.com"],
      "paths": ["/calendar/v3"] },
    { "name": "google-drive", "hosts": ["www.googleapis.com"],
      "paths": ["/drive/v3", "/upload/drive/v3"] }
  ]
}
```

Host matching is exact or a single leading `*.` wildcard. Exact always beats
wildcard, so `api.stripe.com` can route differently from `*.stripe.com`.

`paths` narrows an entry to request paths under a prefix, which some vendors
require: Google fronts Calendar, Drive and its identity endpoints on
`www.googleapis.com`, told apart only by prefix. Prefixes match on a segment
boundary, so `/userinfo` claims `/userinfo/v2/me` and not `/userinfoXYZ`. Host
specificity outranks prefix length; within one host the longer prefix wins; an
entry with no `paths` claims the whole host and loses to any explicit prefix on
it. Two services claiming the same host *and* prefix is rejected at load rather
than resolved by declaration order.

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

### Where the hostnames come from

You do not have to write the `hosts` and `paths` above. They are **generated**
from each service's measured parity backend — the host the oracle was actually
driven against — by `parity vendor-routes --write`, embedded in the binary, and
drift-tested against the backends on every run of the services suite.

That matters because the facts are not guessable. A hand-written table put
Google's `/tokeninfo` on `www.googleapis.com`; the measured record puts it on
`oauth2.googleapis.com`, where Google actually serves it, so a client's token
introspection would have been routed at a service that does not answer there.
Google's identity surface alone spans four hostnames, and three Google services
share a fourth. A second copy of a measured fact is a second chance to be wrong
about it.

`--sandbox` combines that table with the control plane's answer for a given
sandbox, so the config nobody wrote is still the measured one.

## Two design decisions worth knowing

### Only provisioned services are rerouted

A host with no matching service reaches its real destination. Telemetry,
package registries, an internal API and anything else the code under test talks
to behave exactly as they always did, so pointing a project at a sandbox is one
command rather than a configuration project.

`--strict` (or `"mode": "strict"`) blocks unmapped hosts with a 502 and an
actionable error, for a run that has to prove the code under test reached
nothing but the sandbox. That guarantee is real, but it is not the only way to
get it: the receipt below reports what the sandbox actually received, so a
suite quietly talking to the real vendor is visible without having to forbid
every host nobody thought to list.

### The receipt makes a silent no-op impossible

Two mechanisms, answering two different questions.

`veris-proxy check` asserts on a per-run canary token *before* your tests run.
It fails if the proxy is unreachable, if it is not a Veris proxy, or if it
belongs to a different run — a proxy left running from an earlier run, pointing
at a different sandbox, would otherwise let tests pass against the wrong
simulated data.

A canary always exists: the proxy mints one per process when the config names
none, so the assertion never quietly weakens into a liveness probe. `check`
refuses to run without one unless you pass `--any-run` and accept that it
cannot detect a stale proxy.

The receipt answers the question the canary cannot: interception was live, but
did this run actually *use* it? `run` records every request the sandbox
received, keyed by host, service and matched path prefix, and prints the
summary when the command finishes. `--require-service stripe:2` turns that into
an exit code. A suite that quietly stopped calling its dependency passes the
canary check and produces an empty receipt.

Without both, "interception silently did not happen" and "everything worked"
look identical.

## HTTP/2

All three tiers negotiate h2 by ALPN, and fall back to HTTP/1.1 for a client
that does not offer it. The leg from the proxy to the sandbox asks for h2 too.

This is not a detail. Google, Stripe and most large vendors serve HTTP/2, so a
client that negotiates h2 in production and HTTP/1.1 here is exercising a
different transport than the one that ships — different multiplexing, different
header handling, and a different set of code paths inside its own HTTP library.
Both the CONNECT-tunnel tier and the transparent listeners are covered by tests
that fail if the negotiation silently drops back.

## Two tiers of interception

### Kernel redirect, in a container — the default

`iptables REDIRECT` moves the traffic in the kernel, below every library, so
nothing in the process under test has to honour anything. Needs
`--cap-add=NET_ADMIN`. Two arrangements, and which one you want depends on
whether you control the image:

- **Your image joins the proxy's network namespace** (`--network
  container:veris-proxy`) — the one to reach for. Every requirement lands on
  our container rather than yours: it is root, it has iptables, it drops
  itself. Yours needs no capability, no iptables, no entrypoint change and no
  particular base image, so distroless and scratch work. It does not matter how
  you built it.
- **One container, proxy inside.** Simpler to operate, and the trade is that
  those requirements move onto your image, which must also start as root.
  Missing any of them the entrypoint refuses rather than silently degrading.

`serve --transparent` stands itself up when it starts as root on Linux: it
installs the redirect, puts the CA in the system trust store, and drops to an
unprivileged uid before serving. So an image can run the binary directly and
needs no entrypoint script from us. It no-ops when already unprivileged, which
is how it composes with the entrypoint that drops first.

See `container/README.md` for both arrangements, and for composing with an
entrypoint you already have.

The proxy runs as a dedicated uid (14741) inside that container, because the
redirect exempts its own upstream calls by uid and one rule cannot tell two
processes with one uid apart. The entrypoint refuses to start if your command
would share it.

### Explicit proxy, on the host — the fallback

`run` sets the full matrix of proxy and CA variables on the command it starts,
and `serve --write-env` writes the same set to a file. There is no standard for
any of them, so each runtime needs its own:

| Runtime | Proxy | CA |
|---|---|---|
| Python requests / httpx | env | `REQUESTS_CA_BUNDLE` / `SSL_CERT_FILE` |
| Go | env | `SSL_CERT_FILE` (Linux only) |
| Node | needs `--use-env-proxy` | `NODE_EXTRA_CA_CERTS` |
| .NET | env | `SSL_CERT_FILE` (Linux only) |
| Java | `JAVA_TOOL_OPTIONS`, after `trust --java` | JKS truststore, not a PEM |

Java deserves its own paragraph because it reads none of the usual variables
and wants a JKS rather than a PEM. `run` builds one when it finds a JDK —
copying the JDK's own cacerts and adding the Veris CA, never replacing it,
since a store holding only our CA would break every other TLS connection in the
JVM — and emits `JAVA_TOOL_OPTIONS` with the `-D` proxy, `nonProxyHosts` and
truststore flags, which every JVM including Gradle and Maven test forks picks
up.

An app that loads its own keystore from disk never consults the JVM default
truststore. There is no built-in command for that any more; add the CA to its
keystore yourself:

```sh
keytool -importcert -noprompt -trustcacerts -alias veris-local-ca \
  -file ~/.veris/ca/veris-ca.pem -keystore your-keystore.p12 -storepass ...
```

`run` prints what it cannot cover to stderr rather than letting you discover it
as a mystery TLS failure. Four cases are genuinely out of reach: Go on macOS
ignores `SSL_CERT_FILE` and verifies through Security.framework; Apache
HttpClient built with `createDefault()` ignores the JVM proxy properties;
`aiohttp` ignores proxy variables without `trust_env=True`; and the Stripe
Python and Ruby SDKs ship their own CA bundle. All four are covered by the
container tier.


## Certificates

The CA is generated on first run into `~/.veris/ca`, reused thereafter, and the
key is written `0600`. Leaves are minted per host and cached in memory.

Two details that are easy to get wrong and are covered by tests: leaves are
served as **leaf + CA**, since Node and anything on OpenSSL reject a bare leaf
with `UNABLE_TO_VERIFY_LEAF_SIGNATURE`; and every leaf carries a SAN, since a
certificate with only a CN is rejected by every modern client.

## Development

This directory is self-contained — it shares no runtime with the rest of the
repo — and its tree is published verbatim as the public
[veris-ai/veris-proxy](https://github.com/veris-ai/veris-proxy) repository
(snapshot commits, not the parent repo's history), so its own Makefile
carries the targets:

```sh
make test    # go test -race
make lint    # gofmt + go vet
make build   # a binary for this machine (bin/veris-proxy)
make dist    # static binaries for 5 platforms
make e2e     # against real curl, Python and Node clients
```

The runner image (`make proxy-image`) is built and pushed by the parent
repository's CI, which also mirrors `proxy-test`/`proxy-build`/`proxy-dist`
at its root. Releasing: push the split to the standalone repo and tag it
`vX.Y.Z`; its release workflow attaches the `make dist` binaries, which is
where `scripts/install.sh` downloads from.

The e2e script matters because the Go tests exercise the proxy through Go's own
TLS stack, which is more forgiving than OpenSSL's. CI runs the unit tests, the
race detector and every cross-build on any PR touching `proxy/**`; it never
runs parity or anything that drives a real vendor.

## Licence

MIT. Built on [elazarl/goproxy](https://github.com/elazarl/goproxy) (BSD-3).
