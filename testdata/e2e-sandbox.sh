#!/usr/bin/env bash
# End-to-end proof against a REAL sandbox: provision one through the control
# plane, point the proxy at it, and drive an unmodified vendor SDK call at the
# vendor's own hostname.
#
# This is the only test that proves the thing the product claims. The Go tests
# exercise the proxy through Go's own TLS stack against a local origin; this
# one goes through curl's OpenSSL, over the public ingress, into a sandbox that
# was provisioned seconds earlier.
#
# Human-triggered, never CI: it costs a real sandbox.
set -euo pipefail

# The control plane to provision against, and a key for it. Override BASE to
# aim the run at a different environment.
BASE="${VERIS_API_BASE:-https://svc.api.veris.ai}"
API_KEY="${VERIS_API_KEY:?set VERIS_API_KEY to a control-plane API key}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

auth=(-H "X-API-Key: $API_KEY")
json=(-H 'Content-Type: application/json')

say() { printf '\n==> %s\n' "$*"; }
jqp() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }

say "build the proxy"
( cd "$(dirname "$0")/.." && go build -o "$WORK/veris" ./cmd/veris )
PROXY="$WORK/veris"

say "provision a sandbox"
env_id=$(curl -fsS "${auth[@]}" "${json[@]}" \
  -d '{"name": "proxy-e2e", "services": ["stripe"]}' \
  "$BASE/v1/environments" | jqp '["id"]')
sandboxes="$BASE/v1/environments/$env_id/sandboxes"
sbx=$(curl -fsS "${auth[@]}" "${json[@]}" -d '{"ttl_minutes": 20}' "$sandboxes" | jqp '["id"]')
echo "    environment $env_id, sandbox $sbx"
trap 'curl -fsS -X DELETE "${auth[@]}" "$sandboxes/$sbx" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

for _ in $(seq 1 60); do
  status=$(curl -fsS "${auth[@]}" "$sandboxes/$sbx" | jqp '["status"]')
  [ "$status" = ready ] && break
  sleep 2
done
[ "$status" = ready ] || { echo "sandbox never became ready ($status)"; exit 1; }

# "ready" means the sandbox is scheduled, not that the service is answering.
# scripts/smoke.sh polls the service's own health for the same reason; without
# it the first request races the pod and comes back 502 from the gateway.
for _ in $(seq 1 60); do
  curl -fsS "$BASE/s/$sbx/stripe/veris/health" >/dev/null 2>&1 && break
  sleep 2
done
curl -fsS "$BASE/s/$sbx/stripe/veris/health" >/dev/null \
  || { echo "the stripe service never answered"; exit 1; }
echo "    ready, and stripe is answering"

say "write the proxy config"
cat > "$WORK/proxy.json" <<JSON
{
  "version": 1,
  "listen": "127.0.0.1:0",
  "sandbox_id": "$sbx",
  "ca_dir": "$WORK/ca",
  "upstream": { "base_url": "$BASE" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com"] }
  ]
}
JSON

# A well-formed test key. Stripe keys are self-describing, so the simulated
# service shape-checks them exactly as the real one does -- a placeholder like
# "sk_test_veris" earns a genuine Stripe 401, which is the point of the mock.
# Synthesized, never a real account's.
export STRIPE_KEY=sk_test_51QxAbCdEfGhIjKlMnOpQrStUvWxYz

# The whole product, in one command. curl is completely unmodified: it is
# pointed at api.stripe.com and told nothing about Veris.
say "run an unmodified client at api.stripe.com"
set +e
"$PROXY" run --config "$WORK/proxy.json" --require-service stripe -- \
  sh -c 'curl -sS -o "$0/body.json" -w "%{http_code}" \
           -H "Authorization: Bearer $STRIPE_KEY" \
           https://api.stripe.com/v1/customers' "$WORK" > "$WORK/code"
status=$?
set -e

code=$(cat "$WORK/code" 2>/dev/null || echo "none")
echo "    HTTP $code, veris exit $status"
head -c 400 "$WORK/body.json" 2>/dev/null; echo

[ "$status" = 0 ] || { echo "FAIL: veris run exited $status"; exit 1; }
[ "$code" = 200 ] || { echo "FAIL: expected HTTP 200 from the simulated Stripe"; exit 1; }
grep -q '"object"' "$WORK/body.json" || { echo "FAIL: response is not a Stripe payload"; exit 1; }

# The negative control: the same run with a requirement nothing satisfies must
# fail, or the requirement mechanism proves nothing.
say "negative control: a service the run never called"
set +e
"$PROXY" run --config "$WORK/proxy.json" --require-service stripe:99 --quiet -- true
neg=$?
set -e
[ "$neg" = 3 ] || { echo "FAIL: unmet requirement exited $neg, want 3"; exit 1; }
echo "    unmet requirement correctly exited 3"

say "PASS: an unmodified client reached a real sandbox through the proxy"
