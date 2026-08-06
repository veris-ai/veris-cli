# Container runner

Kernel-level interception, so nothing in the process under test has to
cooperate.

## Why this tier exists

Environment variables cannot cover every runtime. Java reads no proxy
environment variable of any kind. Static Go binaries on macOS ignore
`SSL_CERT_FILE`. Apache HttpClient built with `createDefault()` ignores the JVM
proxy properties. `aiohttp` ignores proxy variables unless the session was
constructed with `trust_env=True`.

Inside a container all of that goes away. The CA lives in the image's system
trust store, and `iptables REDIRECT` moves the traffic in the kernel, below
every library.

## Use

```sh
docker run --rm \
  --cap-add=NET_ADMIN \
  -v "$PWD:/work" \
  -v "$PWD/.veris/proxy.json:/veris/config.json:ro" \
  ghcr.io/veris-ai/veris-proxy:runner \
  pytest -q
```

`--cap-add=NET_ADMIN` is required and sufficient. `--privileged` is not needed.
Without the capability the entrypoint degrades to environment variables and
says so loudly.

## Adding it to an image you already have

```dockerfile
COPY --from=ghcr.io/veris-ai/veris-proxy:binary /veris-proxy /usr/local/bin/veris-proxy
COPY --from=ghcr.io/veris-ai/veris-proxy:runner /usr/local/bin/veris-entrypoint /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/veris-entrypoint"]
```

## Known constraints

- The host kernel must have `iptable_nat` available. A container cannot
  `modprobe`, and the failure mode is a misleading "table does not exist".
- `--network=host` would apply the rules to the host firewall. Do not use it.
- Hosts in `allow_passthrough` are excluded from the redirect by CIDR, not by
  name, since the redirect happens before DNS.
