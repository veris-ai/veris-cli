# Container runner

Kernel-level interception, so nothing in the process under test has to
cooperate.

## Why this tier exists

Environment variables cannot cover every runtime. Java reads no proxy
environment variable of any kind. Static Go binaries on macOS ignore
`SSL_CERT_FILE`. Apache HttpClient built with `createDefault()` ignores the JVM
proxy properties. `aiohttp` ignores proxy variables unless the session was
constructed with `trust_env=True`.

Inside a container all of that goes away: `iptables REDIRECT` moves the traffic
in the kernel, below every library.

Two arrangements do that, and the first one is almost always the right answer,
because it puts every requirement on a container we control instead of on
yours. Either way, name a sandbox with `VERIS_SANDBOX_ID` (plus
`VERIS_API_KEY`) or mount a config at `/veris/config.json`; with neither, the
entrypoint refuses and says so. `--cap-add=NET_ADMIN` is required and
sufficient — `--privileged` is not needed.

## Use the network namespace. Your image needs nothing.

Run the proxy as **our** container and have yours join its network namespace.
Two containers sharing a namespace share one network stack — one routing table,
one set of iptables rules — so the redirect the proxy installs applies to your
sockets too.

```sh
# 1. our container. Give it a sandbox and a directory; it does the rest.
docker run -d --name veris-proxy --network testnet --cap-add=NET_ADMIN \
  -e VERIS_SANDBOX_ID=sbx_abc123 -e VERIS_API_KEY=... \
  -v "$PWD/.veris":/veris-share \
  ghcr.io/veris-ai/veris-proxy:runner

# 2. your image, unchanged. This command is printed by step 1, filled in.
docker run --rm --network container:veris-proxy --cap-drop=ALL \
  --env-file .veris/veris.env -v "$PWD/.veris":/veris-share \
  your-image pytest -q
```

`--cap-drop=ALL` in step 2 is the same hardened default `veris run
--image` uses. An entrypoint that switches users (`su`, `gosu`, `service`)
needs `--cap-add=SETUID --cap-add=SETGID` after it — `run --image` takes the
same as `--cap-add SETUID --cap-add SETGID` — or build the image to run as
that `USER` and drop the switch.

No command after the image in step 1: that is what selects this mode. The
entrypoint resolves the sandbox, installs the redirect, drops to an
unprivileged uid, and leaves two files in the mounted directory —
`veris.env` and `veris-ca.pem` — then prints step 2 with the container name
already filled in. **The CA private key is not among them**; it stays in our
container, so your workload cannot sign with it.

`veris.env` carries **CA trust only** — no `HTTP_PROXY`, no `NO_PROXY`,
nothing that routes. The kernel already does that here, and telling the client
about a proxy as well would undo the point: a client that honours those
variables starts cooperating and takes the explicit-proxy path instead, which
is a different transport from the one this tier provides. It would also mask a
redirect that silently stopped working, since the traffic would keep flowing
by the other route. With trust only, the mode the proxy logs is `transparent`,
and a broken redirect fails loudly.

This is the arrangement to reach for, because **every requirement moves off
your image and onto ours**:

| | your image | ours |
|---|---|---|
| `iptables` | not needed | we install it |
| a privilege dropper | not needed | the binary drops itself |
| `ca-certificates` | not needed | not needed either |
| runs as root | no — `CapEff: 0` | yes, then drops |
| `CAP_NET_ADMIN` | no | yes |
| entrypoint changed | no | ours, fixed |

It does not matter how you built your image, what it runs as, whether it has a
shell, or what its entrypoint does. Distroless and scratch work. The two things
you give it are the namespace and the CA.

The costs, and they are the whole list:

- **A shared namespace means shared ports.** Your container cannot publish its
  own, and shares `localhost` with the proxy.
- **Ordering.** Wait for `interception is live` in the proxy's logs before
  starting the workload, or its first requests race the rules.
- **The uid rule weakens.** The proxy exempts uid 14741 and cannot see what
  uid your container runs as. If yours runs as 14741, set `VERIS_PROXY_UID`.
- **CA trust is still yours to arrange** — see below. Nothing avoids it; it is
  what TLS interception means.

### Arranging CA trust

The client has to trust the CA or the handshake fails. Two ways, measured
against a stock `python:3.12-slim` with the proxy in its own container
(`proxy/testdata/e2e-ca-trust.sh`):

**An `--env-file` the proxy writes. Recommended.** `serve --write-env FILE
--env-format docker` emits every variable the runtimes read, and
`docker run --env-file` takes it verbatim:

```sh
docker run -d --name veris-proxy ... \
  --write-env /shared/veris.env --env-format docker
docker run --rm --network container:veris-proxy \
  --env-file /shared/veris.env -v ./veris-ca.pem:/ca.pem:ro \
  your-image pytest -q
```

This is the one that covers everything, because **most runtimes ship their own
trust store and ignore the system one**: Node carries 145 roots of its own,
Python's `requests` uses certifi rather than `/etc/ssl/certs`, and the JVM reads
a JKS. Measured working for Python stdlib, Python requests and Node fetch in
one recipe.

One tier remains beyond any environment variable: an SDK that ships its own
CA file and passes it straight to the TLS layer (stripe-python and
stripe-ruby, older botocore, httplib2). For those, `run --image` takes
`--patch-bundled-cas` — see "SDKs that bundle their own CA" in the main
README. The proxy also reports the failure it closes: a client refusing the
minted certificate for a mapped host shows up in the run's diagnostics
rather than as a silent TLS mystery.

**Over-mounting the system bundle.** Read the image's own bundle, append the
CA, mount the result back over it. No environment variables at all:

```sh
docker run --rm your-image cat /etc/ssl/certs/ca-certificates.crt > bundle.crt
cat veris-ca.pem >> bundle.crt
docker run --rm --network container:veris-proxy \
  -v ./bundle.crt:/etc/ssl/certs/ca-certificates.crt:ro \
  your-image pytest -q
```

Reading their bundle first is the point: appending keeps every root the image
already trusted (measured, 150 → 151), where mounting a file containing only
our CA would break its other TLS. Use this when you cannot set environment
variables — but note it covers only what reads the system store, so on its own
it misses Node, requests and the JVM.

## One container, if you own the image

Simpler to operate — one `docker run`, no ordering — and the trade is that
every requirement above lands on your image instead of ours:

```sh
docker run --rm --cap-add=NET_ADMIN \
  -e VERIS_SANDBOX_ID=sbx_abc123 -e VERIS_API_KEY=... \
  -v "$PWD:/work" -w /work \
  ghcr.io/veris-ai/veris-proxy:runner \
  pytest -q
```

Your image must carry `iptables` and start as root. Nothing else: the binary
installs its own trust store entry and drops its own privileges, so there is no
`su-exec`/`gosu`/`setpriv` to add. Missing iptables or started non-root, `serve`
refuses and names what it needs rather than running your command
uninstrumented. One layer covers it:

```dockerfile
# Debian/Ubuntu
RUN apt-get install -y iptables ca-certificates
# Alpine
RUN apk add iptables ca-certificates
```

### With an entrypoint of your own

The veris entrypoint **wraps** rather than replaces: it sets up interception and
then `exec`s whatever it was given, the way `tini` is used.

```dockerfile
ENTRYPOINT ["/usr/local/bin/veris-entrypoint", "/app/your-entrypoint.sh"]
```

Or with no image rebuild at all, since the binary is static:

```sh
docker run --cap-add=NET_ADMIN \
  -v ./veris:/veris-bin/veris:ro \
  -v ./veris-entrypoint:/veris-bin/veris-entrypoint:ro \
  --entrypoint /veris-bin/veris-entrypoint \
  your-image  /app/your-entrypoint.sh  pytest -q
```

`docker inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}'`
tells you what to restate.

### Without our entrypoint at all

`serve --transparent` stands itself up: as root on Linux it installs the
redirect, puts the CA in the system trust store, drops to an unprivileged uid,
and only then serves. So an image can run the binary directly.

```dockerfile
FROM alpine:3.22
RUN apk add --no-cache iptables ca-certificates
COPY veris /usr/local/bin/veris
```

```sh
docker run -d --cap-add=NET_ADMIN your-image veris serve --transparent
```

It refuses if it cannot install the redirect — including when started
non-root, where `--cap-add=NET_ADMIN` does not help, because Linux clears
capabilities on a uid change. Start as root and let it drop itself.
`--redirect-external` says the rules are somebody else's job, which is what our
entrypoint passes.

## The proxy runs as its own user, and that is load-bearing

The redirect has to exempt the proxy's own upstream calls, or they are
redirected straight back into it. The exemption is by uid, so the proxy and the
command under test must not share one -- a single rule cannot tell them apart,
and the failure is silent: every rule installs, the entrypoint reports
interception active, and every request goes to the real internet.

So `serve` drops itself to uid 14741 after installing the rules, and refuses to
start if handed uid 0 -- exempting root exempts everything. `run --image`
likewise refuses an image whose `USER` is that uid, pulling the image to read it
rather than skipping the check.

`proxy/testdata/e2e-container.sh` measures this rather than reading the rules,
which is the only way to catch it.


## Known constraints

- The host kernel must have `iptable_nat` available. A container cannot
  `modprobe`, and the failure mode is a misleading "table does not exist".
- `--network=host` would apply the rules to the host firewall. Do not use it.
- Hosts in `allow_passthrough` are excluded from the redirect by CIDR, not by
  name, since the redirect happens before DNS.
- **A vendor whose hostname resolves to a private address is not intercepted.**
  The redirect exempts the RFC1918 ranges so the app can still reach its own
  sidecars, its database and the in-pod network — but that exemption is by
  destination address, and the kernel has no idea the `10.x` it is looking at
  came from a split-horizon DNS answer for `api.vendor.example` or from a
  PrivateLink endpoint. Such a dependency needs the explicit-proxy tier, which
  routes by name.
- **The namespace tier cannot give a JVM its truststore.** Java reads a JKS,
  not a PEM, and building one needs a JDK — which is in *your* image, not the
  proxy's. The one-container tier handles it because it runs inside an image
  that has the JDK. For a Java workload in the namespace tier, build the JKS
  yourself from the published `veris-ca.pem` and pass
  `JAVA_TOOL_OPTIONS=-Djavax.net.ssl.trustStore=...`:

  ```sh
  keytool -importcert -noprompt -trustcacerts -alias veris-local-ca \
    -file .veris/veris-ca.pem -keystore truststore.jks -storepass changeit
  ```
- **`/__veris/status` is unauthenticated on whatever the proxy binds.** It
  reports the sandbox id, the service list, the canary and which hosts were
  called. That is fine on loopback and inside a shared namespace; do not bind
  `serve --listen` to a routable address on a shared network.
- **A command that itself drops to the proxy's uid escapes interception.** The
  exemption covers every socket owned by that uid, so a launcher doing
  `su-exec veris ./app` puts the app on the exempt side. The entrypoint checks
  its own uid, which cannot see what the command does later. This is inherent
  to uid-based exemption — Istio's sidecar has the same property with uid
  1337 — and the mitigation is the same: 14741 is chosen to be one nothing
  else claims. Do not run the code under test as it.
