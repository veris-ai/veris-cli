#!/usr/bin/env bash
# Proves the container tier actually intercepts, by measuring rather than
# reading the rules.
#
# The bug this exists to catch is invisible from the outside: the kernel
# redirect exempts the proxy by uid, and if the proxy shares a uid with the
# command under test the exemption covers both. Every rule installs, the
# entrypoint reports "transparent interception active", and every request goes
# straight to the real internet. The only way to know is to send a request and
# see where it lands.
#
# Needs docker. Human-triggered, never CI.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
IMAGE=veris-cli-e2e:local
NET=veris-e2e-$$
trap 'docker rm -f sandbox-e2e >/dev/null 2>&1 || true;
      docker network rm "$NET" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

say() { printf '\n==> %s\n' "$*"; }

say "build the runner image"
docker build -q -f "$HERE/container/Dockerfile" --target runner -t "$IMAGE" "$HERE"

# The reference runner deliberately ships no HTTP client; the command under
# test brings its own. Add one here at BUILD time, while the network is still
# ordinary -- installing it at run time would have to pass through the very
# interception being measured.
printf 'FROM %s\nRUN apk add --no-cache curl\n' "$IMAGE" > "$WORK/Dockerfile.probe"
docker build -q -t "$IMAGE-probe" -f "$WORK/Dockerfile.probe" "$WORK" >/dev/null
IMAGE="$IMAGE-probe"

say "stand up a stand-in sandbox"
docker network create "$NET" >/dev/null
# Any HTTP server will do: the question is which destination the request
# reaches, not what it answers.
docker run -d --name sandbox-e2e --network "$NET" -p 0:80 \
  kennethreitz/httpbin >/dev/null
sleep 3

# The service upstream is overridden to the stand-in's root rather than the
# derived /s/{sandbox}/{service} route, so the rewritten path lands on an
# endpoint that echoes the request back. The question here is which destination
# the request reached, not what a real sandbox would answer.
cat > "$WORK/config.json" <<'JSON'
{
  "version": 1,
  "sandbox_id": "sbx_container_e2e",
  "mode": "strict",
  "upstream": { "base_url": "http://sandbox-e2e" },
  "services": [
    {
      "name": "stripe",
      "hosts": ["api.stripe.com"],
      "upstream": "http://sandbox-e2e"
    }
  ]
}
JSON

# The command under test is told NOTHING: --noproxy "*" makes curl ignore every
# proxy variable in its environment, so anything that still gets intercepted was
# intercepted by the kernel, not by cooperation.
probe='curl -sS --noproxy "*" -o /tmp/body -w "%{http_code}" https://api.stripe.com/anything/charges; echo; head -c 900 /tmp/body'

# The primary path, with no config file anywhere: a sandbox id and a key.
say "a sandbox id alone is enough -- no config file mounted"
grep -q VERIS_SANDBOX_ID "$HERE/container/entrypoint.sh" \
  || { echo "FAIL: the entrypoint cannot take a sandbox id"; exit 1; }
set +e
noconf=$(docker run --rm --network "$NET" --cap-add=NET_ADMIN "$IMAGE" true 2>&1)
set -e
echo "$noconf" | grep -q "nothing to route" \
  || { echo "FAIL: with neither a sandbox nor a config it should say so: $noconf"; exit 1; }
echo "    with neither, it refuses and names both ways in"

say "with NET_ADMIN: the request must NOT reach the real internet"
set +e
out=$(docker run --rm --network "$NET" --cap-add=NET_ADMIN \
  -v "$WORK/config.json":/veris/config.json:ro "$IMAGE" \
  sh -c "$probe" 2>&1)
set -e
echo "$out" | tail -5

# X-Veris-Original-Host is stamped by the proxy on rewrite and echoed back by
# the stand-in, so seeing it proves the request went THROUGH the proxy rather
# than merely arriving somewhere. Real Stripe answers an error envelope naming
# invalid_request_error, which cannot be mistaken for it; anything else means
# the probe never ran.
if echo "$out" | grep -qi 'X-Veris-Original-Host'; then
  echo "    intercepted: the request reached the stand-in carrying the proxy's own header"
elif echo "$out" | grep -q 'invalid_request_error'; then
  echo "FAIL: the request reached REAL Stripe. Interception did not happen."
  exit 1
else
  echo "FAIL: the probe did not complete; cannot tell where the request landed"
  exit 1
fi

say "the proxy must not share a uid with the command under test"
# Read the proxy's uid off its own /proc entry rather than parsing ps output.
# The tagged pattern requires digits on both sides: the entrypoint echoes the
# command it is about to run, so the tag alone also matches the unexpanded
# source text.
uids=$(docker run --rm --network "$NET" --cap-add=NET_ADMIN \
  -v "$WORK/config.json":/veris/config.json:ro "$IMAGE" \
  sh -c 'echo "VERISUID workload=$(id -u) proxy=$(stat -c %u /proc/$(pgrep -nx veris))"' 2>&1 \
  | grep -o 'VERISUID workload=[0-9][0-9]* proxy=[0-9][0-9]*' | tail -1)
echo "    $uids"
workload=${uids#*workload=}; workload=${workload%% *}
proxyuid=${uids##*proxy=}
[ -n "$proxyuid" ] && [ "$workload" != "$proxyuid" ] \
  || { echo "FAIL: proxy and workload share uid $workload, so one exemption rule covers both"; exit 1; }
[ "$proxyuid" = 14741 ] || { echo "FAIL: proxy uid is $proxyuid, want 14741"; exit 1; }
echo "    workload uid $workload, proxy uid $proxyuid -- the exemption names only the proxy"

say "a uid collision must be refused, not silently ignored"
set +e
docker run --rm --network "$NET" --cap-add=NET_ADMIN \
  -e VERIS_PROXY_UID=0 \
  -v "$WORK/config.json":/veris/config.json:ro "$IMAGE" true >"$WORK/collide" 2>&1
collide=$?
set -e
[ "$collide" = 0 ] && { echo "FAIL: a uid collision was accepted"; cat "$WORK/collide"; exit 1; }
grep -q "proxy's own uid" "$WORK/collide" || { echo "FAIL: wrong error"; cat "$WORK/collide"; exit 1; }
echo "    refused, naming the collision"

say "PASS: the container tier intercepts, with the proxy on its own uid"
