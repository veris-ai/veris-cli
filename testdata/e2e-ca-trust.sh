#!/usr/bin/env bash
# Measures the ways a workload container can be made to trust the CA, since
# that is the one thing the namespace arrangement still asks of an image it
# does not control.
#
# Three candidates, each tested against the SAME unmodified image with the
# proxy in a separate container:
#
#   1. one env var   -- SSL_CERT_FILE=/ca.pem
#   2. over-mount    -- their own bundle plus our CA, mounted over the path
#                       the system trust store already lives at
#   3. nothing       -- the control: this must fail, or the test proves
#                       nothing about the other two
#
# Needs docker. Human-triggered, never CI.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
NET=veris-catrust-$$
ARCH=$(docker version --format '{{.Server.Arch}}')
IMG=python:3.12-slim     # a stock image: no iptables, no curl, no veris anything
trap 'docker rm -f ct-proxy ct-sandbox >/dev/null 2>&1 || true;
      docker network rm "$NET" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

say() { printf '\n==> %s\n' "$*"; }

say "build and start the proxy in its own container"
( cd "$HERE" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -o "$WORK/veris-proxy" ./cmd/veris-proxy )
cat > "$WORK/Dockerfile" <<'DOCKER'
FROM alpine:3.22
RUN apk add --no-cache iptables ca-certificates
COPY veris-proxy /usr/local/bin/veris-proxy
DOCKER
docker build -q -t veris-ct:local "$WORK" >/dev/null

docker network create "$NET" >/dev/null
docker run -d --name ct-sandbox --network "$NET" kennethreitz/httpbin >/dev/null
sleep 3

cat > "$WORK/config.json" <<'JSON'
{
  "version": 1, "listen": "0.0.0.0:8080", "sandbox_id": "sbx_catrust",
  "mode": "passthrough", "upstream": { "base_url": "http://ct-sandbox" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com"], "upstream": "http://ct-sandbox" }
  ]
}
JSON
docker run -d --name ct-proxy --network "$NET" --cap-add=NET_ADMIN \
  -v "$WORK/config.json":/veris/config.json:ro -v "$WORK":/out \
  veris-ct:local \
  veris-proxy serve --config /veris/config.json --ca-dir /veris/ca \
    --transparent --write-env /out/veris.env --env-format docker \
    --log-level info >/dev/null
for _ in $(seq 1 40); do
  docker logs ct-proxy 2>&1 | grep -q "dropped privileges" && break; sleep 0.5
done
docker cp ct-proxy:/veris/ca/veris-ca.pem "$WORK/veris-ca.pem" >/dev/null

# The workload probe: stdlib urllib, no requests, no certifi, no env vars set
# by us beyond whatever each case adds.
cat > "$WORK/probe.py" <<'PY'
import json, sys, urllib.request
try:
    with urllib.request.urlopen("https://api.stripe.com/anything/charges", timeout=15) as r:
        body = json.load(r)
    print("OK " + str(body.get("headers", {}).get("X-Veris-Original-Host")))
except Exception as exc:
    print("FAIL " + type(exc).__name__ + ": " + str(exc)[:90])
    sys.exit(1)
PY

run_case() { docker run --rm --network container:ct-proxy --cap-drop=ALL \
  -v "$WORK/probe.py":/probe.py:ro "$@" "$IMG" python3 /probe.py 2>&1 | tail -1; }

say "control: no CA trust at all -- must FAIL"
out=$(run_case || true); echo "    $out"
case "$out" in
  FAIL*CERTIFICATE_VERIFY_FAILED*|FAIL*SSLCertVerification*)
    echo "    correct: the handshake is refused, so interception IS happening" ;;
  OK*) echo "FAIL: it trusted an unknown CA; this test proves nothing"; exit 1 ;;
  *) echo "FAIL: unexpected -- $out"; exit 1 ;;
esac

say "1. one environment variable"
out=$(run_case -v "$WORK/veris-ca.pem":/ca.pem:ro -e SSL_CERT_FILE=/ca.pem || true)
echo "    $out"
case "$out" in OK*api.stripe.com*) echo "    works" ;; *) echo "FAIL"; exit 1 ;; esac

say "2. over-mount their own bundle, plus our CA -- no env vars"
# Read the image's OWN bundle so nothing it already trusts is lost, then append.
docker run --rm "$IMG" cat /etc/ssl/certs/ca-certificates.crt > "$WORK/bundle.crt"
before=$(grep -c BEGIN "$WORK/bundle.crt")
cat "$WORK/veris-ca.pem" >> "$WORK/bundle.crt"
after=$(grep -c BEGIN "$WORK/bundle.crt")
echo "    their bundle: $before certs -> $after with ours appended"
out=$(run_case -v "$WORK/bundle.crt":/etc/ssl/certs/ca-certificates.crt:ro || true)
echo "    $out"
case "$out" in
  OK*api.stripe.com*) echo "    works, with no environment variable at all" ;;
  *) echo "    does NOT work on this image/runtime"; ;;
esac

say "and their own trust survives it"
out=$(docker run --rm --network "$NET" --cap-drop=ALL \
  -v "$WORK/bundle.crt":/etc/ssl/certs/ca-certificates.crt:ro "$IMG" \
  python3 -c 'import ssl,socket
ctx = ssl.create_default_context()
print("loaded", len(ctx.get_ca_certs()), "roots")' 2>&1 | tail -1)
echo "    $out"

# The recipe worth recommending: one --env-file from the proxy plus the CA.
# It has to cover the runtimes that ship their OWN trust store and therefore
# ignore the system bundle entirely -- which is most of them.
say "3. the generated --env-file, across runtimes that each trust differently"
sed -i.bak 's#^SSL_CERT_FILE=.*#SSL_CERT_FILE=/ca.pem#; s#^REQUESTS_CA_BUNDLE=.*#REQUESTS_CA_BUNDLE=/ca.pem#; s#^CURL_CA_BUNDLE=.*#CURL_CA_BUNDLE=/ca.pem#; s#^NODE_EXTRA_CA_CERTS=.*#NODE_EXTRA_CA_CERTS=/ca.pem#' "$WORK/veris.env"
grep -E '^(SSL_CERT_FILE|NODE_EXTRA_CA_CERTS|REQUESTS_CA_BUNDLE)=' "$WORK/veris.env" | sed 's/^/    /'

envrun() { docker run --rm --network container:ct-proxy --cap-drop=ALL \
  --env-file "$WORK/veris.env" -v "$WORK/veris-ca.pem":/ca.pem:ro "$@" 2>&1 | tail -1; }

printf '    %-22s ' "python stdlib:"
envrun -v "$WORK/probe.py":/probe.py:ro "$IMG" python3 /probe.py

printf '    %-22s ' "python requests:"
envrun "$IMG" sh -c 'pip install -q requests 2>/dev/null; python3 -c "
import requests
r = requests.get(\"https://api.stripe.com/anything/x\", timeout=15)
print(\"OK \" + r.json()[\"headers\"][\"X-Veris-Original-Host\"])"'

printf '    %-22s ' "node fetch:"
envrun node:22-slim node -e '
fetch("https://api.stripe.com/anything/x").then(r=>r.json())
 .then(j=>console.log("OK "+j.headers["X-Veris-Original-Host"]))
 .catch(e=>{console.log("FAIL "+e.message);process.exit(1)})'

say "done"
