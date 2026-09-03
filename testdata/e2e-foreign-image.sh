#!/usr/bin/env bash
# Proves the container tier works on SOMEONE ELSE'S image, without taking over
# their entrypoint.
#
# The runner image is fine when your code can live in it. A real application
# image cannot: it has its own base, its own dependencies, and usually its own
# ENTRYPOINT doing migrations or process supervision. Replacing that entrypoint
# is not an option, so the veris one has to COMPOSE with it -- run first, set up
# interception, then exec whatever was already there. Same shape as tini or
# dumb-init.
#
# It also pins the requirements that image must satisfy, because they are the
# real cost of this tier and they are easy to discover too late.
#
# Needs docker. Human-triggered, never CI.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
NET=veris-foreign-$$
ARCH=$(docker version --format '{{.Server.Arch}}' 2>/dev/null || echo arm64)
trap 'docker rm -f fi-sandbox >/dev/null 2>&1 || true;
      docker network rm "$NET" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

say() { printf '\n==> %s\n' "$*"; }

say "build the veris binary for the container"
( cd "$HERE" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -o "$WORK/veris" ./cmd/veris )
cp "$HERE/container/entrypoint.sh" "$WORK/veris-entrypoint"
chmod +x "$WORK/veris-entrypoint"

# A stand-in for a customer's application image: not ours, its own base, and
# critically its OWN entrypoint that must keep running.
say "build a stand-in customer image with its own ENTRYPOINT"
cat > "$WORK/app-entrypoint.sh" <<'SH'
#!/bin/sh
# The customer's own entrypoint. Its side effect has to survive.
echo "CUSTOMER-INIT-RAN"
exec "$@"
SH
cat > "$WORK/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
# The four things the kernel-redirect tier needs from an image. This is the
# whole cost of the tier, and it is one layer.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends iptables util-linux ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
COPY app-entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh
ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["python", "-c", "print('default cmd')"]
DOCKER
docker build -q -t veris-foreign-app:local "$WORK" >/dev/null

say "stand up a stand-in sandbox"
docker network create "$NET" >/dev/null
docker run -d --name fi-sandbox --network "$NET" kennethreitz/httpbin >/dev/null
sleep 3

cat > "$WORK/config.json" <<'JSON'
{
  "version": 1,
  "listen": "0.0.0.0:8080",
  "sandbox_id": "sbx_foreign",
  "mode": "strict",
  "upstream": { "base_url": "http://fi-sandbox" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com"], "upstream": "http://fi-sandbox" }
  ]
}
JSON

# The composition. --entrypoint puts the veris wrapper in front; the customer's
# own entrypoint becomes its first argument, and their command follows. Nothing
# in their image is rebuilt to accommodate us beyond the packages above, and
# their entrypoint still runs.
say "run their image, their entrypoint, with the wrapper in front"
set +e
out=$(docker run --rm --network "$NET" --cap-add=NET_ADMIN \
  -v "$WORK/veris":/veris-bin/veris:ro \
  -v "$WORK/veris-entrypoint":/veris-bin/veris-entrypoint:ro \
  -v "$WORK/config.json":/veris/config.json:ro \
  -e PATH=/veris-bin:/usr/local/bin:/usr/bin:/bin \
  -e VERIS_CA_DIR=/tmp/veris-ca \
  --entrypoint /veris-bin/veris-entrypoint \
  veris-foreign-app:local \
    /app/entrypoint.sh \
    sh -c 'curl -sS --noproxy "*" https://api.stripe.com/anything/charges' 2>&1)
set -e
echo "$out" | grep -v '^{"time"' | tail -22

say "checks"
echo "$out" | grep -q 'CUSTOMER-INIT-RAN' \
  || { echo "FAIL: the customer's own entrypoint did not run"; exit 1; }
echo "    their entrypoint ran"

echo "$out" | grep -q 'kernel redirect installed' \
  || { echo "FAIL: the kernel redirect was not installed"; exit 1; }
echo "    the kernel redirect installed itself in their image"

echo "$out" | grep -qi 'X-Veris-Original-Host' \
  || { echo "FAIL: the request was not intercepted"; exit 1; }
echo "    an unmodified curl, --noproxy '*', was intercepted anyway"

# The failure that used to be a warning: an image missing iptables must refuse,
# not run the command with no interception and call it a pass.
say "an image WITHOUT iptables must refuse rather than degrade"
set +e
bare=$(docker run --rm --network "$NET" --cap-add=NET_ADMIN \
  -v "$WORK/veris":/veris-bin/veris:ro \
  -v "$WORK/veris-entrypoint":/veris-bin/veris-entrypoint:ro \
  -v "$WORK/config.json":/veris/config.json:ro \
  -e PATH=/veris-bin:/usr/local/bin:/usr/bin:/bin \
  -e VERIS_CA_DIR=/tmp/veris-ca \
  --entrypoint /veris-bin/veris-entrypoint \
  python:3.12-slim  python -c 'print("SHOULD-NOT-RUN")' 2>&1)
status=$?
set -e
[ "$status" != 0 ] || { echo "FAIL: it ran the command with no interception"; exit 1; }
echo "$bare" | grep -q 'SHOULD-NOT-RUN' && { echo "FAIL: the command ran anyway"; exit 1; }
echo "$bare" | grep -q 'iptables' || { echo "FAIL: it did not name what was missing"; exit 1; }
echo "    refused, and named iptables as the missing piece"

say "PASS: a foreign image intercepts, and its own entrypoint survives"
