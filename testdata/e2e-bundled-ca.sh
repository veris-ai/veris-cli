#!/usr/bin/env bash
# Proves the two bundled-CA mechanisms end to end against a real
# bundle-pinning SDK. stripe-python hands its own ca-certificates.crt
# straight to the TLS layer and reads none of the trust environment, so the
# env-var handoff that satisfies every other client does nothing for it --
# which is exactly what --patch-bundled-cas and the trust-rejection
# diagnostics exist for.
#
# Five cases against the SAME workload image, all driven by `run --image`
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
#   4. MIXED traffic without the flag -- a harness half completes /veris/*
#      control-plane reads on the same host while the SDK half refuses.
#      This once passed: the harness reads satisfied --require-service and
#      suppressed the diagnostic. Now the control reads are counted apart,
#      the requirement is unmet (exit 3), and the rejection still prints.
#   5. the same mixed probe with the flag -- both halves complete and the
#      SDK's own traffic satisfies the requirement.
#   6. an UNKNOWN bundle-pinning client (httpx verifying against its own CA
#      file at a path the scan table does not know), WITH the flag -- the
#      run exits 3 and the diagnostic names that exact file as a candidate
#      to over-mount by hand. The self-correction handoff under test.
#   7. the manual over-mount the case-6 advice prescribes -- a copy of that
#      file with the Veris CA appended, bound over the same path -- and the
#      unknown client completes.
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
RUNNER=veris-cli-bca:local
trap 'docker rm -f "$STUB" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

say() { printf '\n==> %s\n' "$*"; }

say "build the CLI, the runner image, and a workload image with stripe baked in"
( cd "$HERE" && go build -o "$WORK/veris" ./cmd/veris )
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
# The mixed probe is the field failure that once passed silently: a HARNESS
# half whose httpx honours SSL_CERT_FILE and reads /veris/* control plane
# (completing fine), beside the SDK half refusing the minted leaf. The
# completing harness traffic used to suppress the trust diagnostic AND satisfy
# --require-service on its own.
cat > "$WORK/mixed.py" <<'PY'
import os, httpx, stripe

verify = os.environ.get("SSL_CERT_FILE") or True
for _ in range(4):
    httpx.get("https://api.stripe.com/veris/health", verify=verify, timeout=10)
print("HARNESS OK 4 control-plane reads completed", flush=True)

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
# The unknown probe mimics an SDK the scan table does not know: httpx handed
# its own CA file at a private path. The scan cannot patch it -- but after a
# refusal it must NAME it, which is the whole self-correction handoff.
cat > "$WORK/unknown.py" <<'PY'
import httpx

try:
    r = httpx.get("https://api.stripe.com/v1/charges",
                  verify="/opt/trust/cacert.pem", timeout=10)
    print("PROBE OK (status %d)" % r.status_code, flush=True)
except Exception as exc:
    print("PROBE FAIL %s: %s" % (type(exc).__name__, str(exc)[:120]), flush=True)
PY
# Installed at BUILD time so the bundled CA sits in the image layers, which is
# where the scan has to find it. No version pin: the pinning under test is the
# SDK's CA bundle, not its release. /opt/trust/cacert.pem is the unknown
# client's private trust file: real public roots, no Veris CA.
cat > "$WORK/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
RUN pip install --no-cache-dir stripe httpx
RUN mkdir -p /opt/trust && \
    cp /usr/local/lib/python3.12/site-packages/certifi/cacert.pem /opt/trust/cacert.pem
COPY probe.py /probe.py
COPY mixed.py /mixed.py
COPY unknown.py /unknown.py
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
# this run. run_with takes flags AND the command; run_case keeps the fixed
# probe for the cases that share it.
run_with() { HOME="$WORK" "$WORK/veris" run --image "$APP" \
  --proxy-image "$RUNNER" --config "$WORK/config.json" "$@" 2>&1; }
run_case() { run_with "$@" -- python3 /probe.py; }

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

say "4. mixed traffic: harness httpx completes on the host while the SDK refuses"
# The field failure: control-plane reads complete on api.stripe.com, the SDK's
# handshakes are rejected, and the old accounting let the harness traffic
# satisfy --require-service and silence the diagnostic. Now the control reads
# are counted apart, the requirement is unmet, and the rejection still prints.
set +e
out=$(run_with --require-service stripe -- python3 /mixed.py); status=$?
set -e
echo "$out" | sed 's/^/    /' | tail -8
[ "$status" -eq 3 ] \
  || { echo "FAIL: expected exit 3 (harness traffic must not vouch), got $status"; exit 1; }
echo "$out" | grep -q 'HARNESS OK 4 control-plane reads completed' \
  || { echo "FAIL: the harness half did not complete"; exit 1; }
echo "$out" | grep -q 'PROBE FAIL' \
  || { echo "FAIL: the SDK half did not report a transport failure"; exit 1; }
echo "$out" | grep -Eq 'api\.stripe\.com: [0-9]+ TLS handshake\(s\) rejected' \
  || { echo "FAIL: mixed traffic silenced the trust diagnostic"; exit 1; }
echo "$out" | grep -q 'control-plane request(s)' \
  || { echo "FAIL: the receipt does not name the excluded control-plane reads"; exit 1; }
echo "    exit 3: the harness's own reads no longer count, and the refusal prints"

say "5. mixed traffic with --patch-bundled-cas: both halves complete"
set +e
out=$(run_with --require-service stripe --patch-bundled-cas -- python3 /mixed.py); status=$?
set -e
echo "$out" | sed 's/^/    /' | tail -6
[ "$status" -eq 0 ] || { echo "FAIL: exit $status"; exit 1; }
echo "$out" | grep -q 'PROBE OK' \
  || { echo "FAIL: the SDK half did not complete after the overlay"; exit 1; }
echo "    over-mounted, and the SDK's own traffic satisfies the requirement"

say "6. an unknown bundle-pinning client, flag ON: the diagnostic names the file"
set +e
out=$(run_with --patch-bundled-cas --require-service stripe -- python3 /unknown.py); status=$?
set -e
echo "$out" | sed 's/^/    /' | tail -6
[ "$status" -eq 3 ] || { echo "FAIL: expected exit 3, got $status"; exit 1; }
echo "$out" | grep -q 'PROBE FAIL' \
  || { echo "FAIL: the unknown client did not report a transport failure"; exit 1; }
echo "$out" | grep -q '/opt/trust/cacert.pem' \
  || { echo "FAIL: the diagnostic did not name the unknown bundle to over-mount"; exit 1; }
echo "$out" | grep -q 'bind it over that exact path' \
  || { echo "FAIL: the diagnostic did not prescribe the manual over-mount"; exit 1; }
echo "    exit 3, and the advice names /opt/trust/cacert.pem"

say "7. the prescribed manual over-mount fixes the unknown client"
# Exactly what the case-6 advice says: the named file with the published
# Veris CA appended, at the same path. The proxy container mints a fresh CA
# per run, so the append uses the LIVE run's /veris-share/veris-ca.pem: the
# file is bound writable and patched in the run's own first step -- trust
# data changed, code under test untouched.
docker run --rm "$APP" cat /opt/trust/cacert.pem > "$WORK/patched-unknown.pem"
set +e
out=$(run_with --require-service stripe \
  -v "$WORK/patched-unknown.pem:/opt/trust/cacert.pem" \
  -- sh -c 'cat /veris-share/veris-ca.pem >> /opt/trust/cacert.pem && python3 /unknown.py'); status=$?
set -e
echo "$out" | sed 's/^/    /' | tail -5
[ "$status" -eq 0 ] || { echo "FAIL: exit $status"; exit 1; }
echo "$out" | grep -q 'PROBE OK' \
  || { echo "FAIL: the over-mounted trust file did not fix the unknown client"; exit 1; }
echo "    the file the diagnostic named, patched by hand, closes the loop"

say "PASS: the diagnostics catch a bundle-pinning SDK, and the overlay fixes it"
