#!/usr/bin/env bash
# Proves `serve --transparent` stands ITSELF up in a Linux container: no
# entrypoint script, no shell wrapper, no privileged helper. Just the binary as
# the container's command.
#
# The binary installs the iptables redirect, puts the CA in the system trust
# store, drops to an unprivileged uid, and only then serves. Everything the
# entrypoint script does in shell, done by the thing that has to be there
# anyway -- so an image can run the proxy without adopting our entrypoint.
#
# Needs docker. Human-triggered, never CI.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
NET=veris-noent-$$
ARCH=$(docker version --format '{{.Server.Arch}}')
trap 'docker rm -f noent-proxy noent-sandbox >/dev/null 2>&1 || true;
      docker network rm "$NET" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

say() { printf '\n==> %s\n' "$*"; }

say "build a linux binary and an image that ONLY contains it"
( cd "$HERE" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -o "$WORK/veris-proxy" ./cmd/veris-proxy )
cat > "$WORK/Dockerfile" <<'DOCKER'
FROM alpine:3.22
RUN apk add --no-cache iptables ca-certificates
COPY veris-proxy /usr/local/bin/veris-proxy
# No ENTRYPOINT of ours, no entrypoint script, no su-exec. The binary is it.
DOCKER
docker build -q -t veris-noent:local "$WORK" >/dev/null

say "stand up a stand-in sandbox"
docker network create "$NET" >/dev/null
docker run -d --name noent-sandbox --network "$NET" kennethreitz/httpbin >/dev/null
sleep 3

cat > "$WORK/config.json" <<'JSON'
{
  "version": 1,
  "listen": "0.0.0.0:8080",
  "sandbox_id": "sbx_noent",
  "mode": "strict",
  "upstream": { "base_url": "http://noent-sandbox" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com"], "upstream": "http://noent-sandbox" }
  ]
}
JSON

# The whole thing: the container's command IS `veris-proxy serve`.
say "run the binary directly as the container command"
docker run -d --name noent-proxy --network "$NET" --cap-add=NET_ADMIN \
  -v "$WORK/config.json":/veris/config.json:ro \
  veris-noent:local \
  veris-proxy serve --config /veris/config.json --ca-dir /veris/ca \
    --transparent --log-level info >/dev/null

for _ in $(seq 1 40); do
  docker logs noent-proxy 2>&1 | grep -q "dropped privileges" && break
  sleep 0.5
done
docker logs noent-proxy 2>&1 | grep -E "kernel redirect installed|system trust|dropped privileges|listening" \
  | sed 's/^/    /' | head -8

say "it installed the rules itself"
docker exec noent-proxy iptables -t nat -L VERIS -n 2>/dev/null | sed 's/^/    /' | head -10
docker exec noent-proxy iptables -t nat -L VERIS -n 2>/dev/null | grep -q "owner UID match 14741" \
  || { echo "FAIL: no uid exemption rule"; exit 1; }

say "it is no longer root"
uid=$(docker exec noent-proxy sh -c 'ps -o user,args | grep -m1 "[v]eris-proxy serve" | awk "{print \$1}"')
echo "    proxy runs as: $uid"
[ "$uid" != "root" ] && [ "$uid" != "0" ] || { echo "FAIL: still root"; exit 1; }

# A workload container joining the namespace. It gets NO capabilities and knows
# nothing about the proxy -- --noproxy "*" so any variable that leaked is
# ignored. The only thing it is given is the CA to trust.
docker cp noent-proxy:/veris/ca/veris-ca.pem "$WORK/veris-ca.pem" >/dev/null
say "an unmodified image, sharing the namespace, is intercepted"
set +e
out=$(docker run --rm --network container:noent-proxy --cap-drop=ALL \
  -v "$WORK/veris-ca.pem":/ca.pem:ro \
  curlimages/curl:latest \
  -sS --noproxy '*' --cacert /ca.pem https://api.stripe.com/anything/charges 2>&1)
set -e
echo "$out" | head -c 500; echo

echo "$out" | grep -qi 'X-Veris-Original-Host' \
  || { echo "FAIL: not intercepted"; exit 1; }
echo "    intercepted, with no entrypoint script anywhere in this test"

say "PASS: serve --transparent stands itself up"
