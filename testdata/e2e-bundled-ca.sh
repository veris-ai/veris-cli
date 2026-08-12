#!/usr/bin/env bash
# Proves the two bundled-CA mechanisms end to end against a real
# bundle-pinning SDK. stripe-python hands its own ca-certificates.crt
# straight to the TLS layer and reads none of the trust environment, so the
# env-var handoff that satisfies every other client does nothing for it --
# which is exactly what --patch-bundled-cas and the trust-rejection
# diagnostics exist for.
#
# Three cases against the SAME workload image, all driven by `run --image`
# because both mechanisms live in the containerised tier:
#
#   1. without --patch-bundled-cas -- the SDK refuses the minted leaf, the
#      run exits 3, and the diagnostics name api.stripe.com and the rejected
#      handshakes. The control AND the diagnostic under test at once.
#   2. with --patch-bundled-cas -- the bundle in the IMAGE layers is found,
#      a copy gains the Veris CA, the copy is over-mounted, and the same
#      probe completes against the stub.
#   3. with the SDK's data dir arriving through a -v mount -- the mount
#      shadows the image, so only the VOLUME scan can find the effective
#      copy: were it broken, no stripe overlay would exist and TLS would
#      fail exactly as in case 1.
#
# `run --image` puts the proxy on a network of its own, so the stand-in
# sandbox is published on the host and addressed as host.docker.internal,
# which Docker Desktop resolves in every container. A daemon that cannot
# resolve it (bare Linux without --add-host) makes this skip, not fail.
#
# Needs docker. Human-triggered, never CI.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
STUB=bca-stub-$$
APP=veris-bca-app:local
RUNNER=veris-proxy-bca:local
trap 'docker rm -f "$STUB" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

say() { printf '\n==> %s\n' "$*"; }

say "build the CLI, the runner image, and a workload image with stripe baked in"
( cd "$HERE" && go build -o "$WORK/veris-proxy" ./cmd/veris-proxy )
docker build -q -f "$HERE/container/Dockerfile" --target runner -t "$RUNNER" "$HERE" >/dev/null

# The probe is the shipping code path: the SDK pointed at its production
# hostname, no base-URL override, no CA flag. ANY response from the stub is
# transport success -- an HTTP-level error body still travelled the whole
# intercepted path -- and only a connection/TLS failure is PROBE FAIL. It
# always exits 0, so the run's own exit code carries the proxy's verdict.
cat > "$WORK/probe.py" <<'PY'
import stripe

stripe.api_key = "sk_test_x"
stripe.max_network_retries = 0
try:
    stripe.Customer.list(limit=1)
    print("PROBE OK (the stub even answered 2xx)", flush=True)
except stripe.APIConnectionError as exc:
    print("PROBE FAIL " + str(exc)[:160], flush=True)
except Exception as exc:
    print("PROBE OK (stub reached; SDK raised %s)" % type(exc).__name__, flush=True)
PY
# Installed at BUILD time so the bundled CA sits in the image layers, which is
# where the scan has to find it. No version pin: the pinning under test is the
# SDK's CA bundle, not its release.
cat > "$WORK/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
RUN pip install --no-cache-dir stripe
COPY probe.py /probe.py
DOCKER
docker build -q -t "$APP" "$WORK" >/dev/null

say "stand up a stand-in sandbox, published on the host"
docker run -d --name "$STUB" -p 127.0.0.1:0:80 kennethreitz/httpbin >/dev/null
PORT=$(docker port "$STUB" 80/tcp | head -1); PORT=${PORT##*:}
for _ in $(seq 1 40); do
  curl -fsS -o /dev/null "http://127.0.0.1:$PORT/anything" 2>/dev/null && break
  sleep 0.5
done
curl -fsS -o /dev/null "http://127.0.0.1:$PORT/anything" \
  || { echo "FAIL: the stub never came up on 127.0.0.1:$PORT"; exit 1; }

if ! docker run --rm "$APP" python3 -c \
    "import socket; socket.create_connection(('host.docker.internal', $PORT), timeout=5)" \
    >/dev/null 2>&1; then
  echo "SKIP: this docker daemon does not resolve host.docker.internal, so the"
  echo "      proxy on the run's own network cannot reach the stand-in sandbox."
  echo "      Docker Desktop resolves it everywhere; on bare Linux there is no"
  echo "      way to hand --add-host to a container the run starts itself."
  exit 0
fi

cat > "$WORK/config.json" <<JSON
{
  "version": 1,
  "sandbox_id": "sbx_bundled_ca",
  "mode": "strict",
  "upstream": { "base_url": "http://host.docker.internal:$PORT" },
  "services": [
    {
      "name": "stripe",
      "hosts": ["api.stripe.com"],
      "upstream": "http://host.docker.internal:$PORT"
    }
  ]
}
JSON

# HOME under $WORK, so the scan cache and anything else the CLI writes stay in
# this run. Extra arguments land between the fixed flags and the command.
run_case() { HOME="$WORK" "$WORK/veris-proxy" run --image "$APP" \
  --proxy-image "$RUNNER" --config "$WORK/config.json" "$@" \
  -- python3 /probe.py 2>&1; }

say "1. without --patch-bundled-cas: the SDK must refuse, and the run must say so"
set +e
out=$(run_case); status=$?
set -e
echo "$out" | sed 's/^/    /' | tail -6
[ "$status" -eq 3 ] \
  || { echo "FAIL: expected exit 3 (trust verdict), got $status"; exit 1; }
echo "$out" | grep -q 'PROBE FAIL' \
  || { echo "FAIL: the probe did not report a transport failure"; exit 1; }
echo "$out" | grep -Eq 'api\.stripe\.com: [0-9]+ TLS handshake\(s\) rejected' \
  || { echo "FAIL: no trust diagnostic naming api.stripe.com"; exit 1; }
echo "    exit 3, and the diagnostic names the host and the rejected handshakes"

say "2. with --patch-bundled-cas: the image's bundle is patched and the probe completes"
set +e
out=$(run_case --patch-bundled-cas --require-service stripe); status=$?
set -e
echo "$out" | sed 's/^/    /' | tail -8
[ "$status" -eq 0 ] || { echo "FAIL: exit $status"; exit 1; }
echo "$out" | grep -Eq 'stripe: bundled CA at .*stripe/data/ca-certificates\.crt -- over-mounted' \
  || { echo "FAIL: no overlay line naming stripe's bundled CA"; exit 1; }
echo "$out" | grep -q 'PROBE OK' \
  || { echo "FAIL: the probe did not complete"; exit 1; }
echo "    over-mounted, intercepted, and the sandbox saw service stripe"

say "3. the same bundle arriving through a -v mount over the SDK's data dir"
SP=$(docker run --rm "$APP" python3 -c \
  'import os, stripe; print(os.path.join(os.path.dirname(stripe.__file__), "data"))')
mkdir -p "$WORK/stripedata"
docker run --rm "$APP" sh -c "cd '$SP' && tar cf - ." | tar xf - -C "$WORK/stripedata"
# The mount shadows the image's copy, so the image-scan candidate is dropped
# and only the volume walk can supply the overlay: this passing IS the proof
# that the -v path works, not merely that case 2 worked twice.
set +e
out=$(run_case --patch-bundled-cas --require-service stripe \
  -v "$WORK/stripedata:$SP"); status=$?
set -e
echo "$out" | sed 's/^/    /' | tail -8
[ "$status" -eq 0 ] || { echo "FAIL: exit $status"; exit 1; }
echo "$out" | grep -Eq 'stripe: bundled CA at .*stripe/data/ca-certificates\.crt -- over-mounted' \
  || { echo "FAIL: no overlay for the mounted bundle"; exit 1; }
echo "$out" | grep -q 'PROBE OK' \
  || { echo "FAIL: the probe did not complete through the mounted bundle"; exit 1; }
echo "    the volume scan found the effective copy and the overlay won"

say "PASS: the diagnostics catch a bundle-pinning SDK, and the overlay fixes it"
