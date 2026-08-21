#!/usr/bin/env bash
# End-to-end verification against real HTTP clients.
#
# The Go tests exercise the proxy through Go's own TLS stack, which is more
# forgiving than most. This script proves the certificate chain is accepted by
# OpenSSL (curl), and by Python and Node when they are available, because those
# are the stacks that actually reject a bare leaf.
set -uo pipefail

BIN="${1:?usage: e2e.sh /path/to/veris-proxy}"
WORK="$(mktemp -d)"
PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

# Ephemeral ports so concurrent or repeated runs never collide.
freeport() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }
ORIGIN_PORT=$(freeport)
PROXY_PORT=$(freeport)
PROXY="http://127.0.0.1:${PROXY_PORT}"
CA="$WORK/ca/veris-ca.pem"

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
FAILED=0

# A stand-in for the Veris sandbox. Echoes back what it received so we can
# assert on the rewrite.
python3 - "$ORIGIN_PORT" <<'PY' &
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({
            "origin": "sandbox",
            "path": self.path,
            "original_host": self.headers.get("X-Veris-Original-Host"),
            "service": self.headers.get("X-Veris-Service"),
            "sandbox": self.headers.get("X-Veris-Sandbox"),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PY
PIDS+=($!)

for _ in $(seq 1 50); do
  curl -fsS "http://127.0.0.1:${ORIGIN_PORT}/ping" >/dev/null 2>&1 && break
  sleep 0.1
done
if ! curl -fsS "http://127.0.0.1:${ORIGIN_PORT}/ping" >/dev/null 2>&1; then
  echo "fatal: the fake sandbox origin never came up on ${ORIGIN_PORT}" >&2
  exit 1
fi

cat > "$WORK/config.json" <<EOF
{
  "version": 1,
  "listen": "127.0.0.1:${PROXY_PORT}",
  "sandbox_id": "sbx_e2e",
  "env_id": "env_e2e",
  "mode": "strict",
  "ca_dir": "$WORK/ca",
  "upstream": { "base_url": "http://127.0.0.1:${ORIGIN_PORT}" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com", "*.stripe.com"] }
  ],
  "canary_token": "e2e-token-1"
}
EOF

"$BIN" serve --config "$WORK/config.json" --log-level warn \
  --write-env "$WORK/veris.env" >"$WORK/proxy.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 50); do
  curl -fsS "${PROXY}/__veris/status" >/dev/null 2>&1 && break
  sleep 0.1
done

echo "veris-proxy e2e"

# --- curl / OpenSSL -----------------------------------------------------------
out=$(curl -sS --proxy "$PROXY" --cacert "$CA" https://api.stripe.com/v1/charges)
if echo "$out" | grep -q '"origin": "sandbox"' \
   && echo "$out" | grep -q '"path": "/s/sbx_e2e/stripe/v1/charges"' \
   && echo "$out" | grep -q '"original_host": "api.stripe.com"'; then
  pass "curl: https intercepted, rewritten to the sandbox, original host preserved"
else
  fail "curl: unexpected response: $out"
fi

# The chain check that matters. Without leaf + CA, OpenSSL rejects this.
if curl -sS --proxy "$PROXY" --cacert "$CA" https://api.stripe.com/ >/dev/null 2>&1; then
  pass "curl: certificate chain verifies under OpenSSL"
else
  fail "curl: certificate chain rejected"
fi

# Wildcard host.
if curl -sS --proxy "$PROXY" --cacert "$CA" https://files.stripe.com/v1/files \
   | grep -q '"path": "/s/sbx_e2e/stripe/v1/files"'; then
  pass "curl: wildcard host routed to the same service"
else
  fail "curl: wildcard host not routed"
fi

# The property the product depends on. 421 rather than a 5xx so the refusal is
# final: curl and every other client retry 5xx, and the route is not coming back.
code=$(curl -sS -o "$WORK/blocked.json" -w '%{http_code}' \
       --proxy "$PROXY" --cacert "$CA" https://api.openai.com/v1/models)
if [ "$code" = "421" ] && grep -q veris_unmapped_host "$WORK/blocked.json"; then
  pass "strict mode: unmapped host blocked with an actionable error"
else
  fail "strict mode: unmapped host got HTTP $code"
fi

# --- canary -------------------------------------------------------------------
if "$BIN" check --proxy "$PROXY" --expect-canary e2e-token-1 --quiet; then
  pass "check: canary matches, interception confirmed live"
else
  fail "check: canary probe failed"
fi

"$BIN" check --proxy "$PROXY" --expect-canary wrong-token --quiet 2>/dev/null
rc=$?
if [ "$rc" -eq 2 ]; then
  pass "check: a stale proxy with the wrong canary is rejected (exit 2)"
else
  fail "check: wrong canary produced exit $rc, want 2"
fi

if "$BIN" check --proxy http://127.0.0.1:1 --quiet 2>/dev/null; then
  fail "check: an unreachable proxy was reported healthy"
else
  pass "check: unreachable proxy fails, so a run without interception cannot pass"
fi

# --- Python requests ----------------------------------------------------------
# These probes source the environment the proxy writes (serve --write-env)
# rather than hand-setting HTTPS_PROXY.
# Hand-setting only the uppercase name is not equivalent: Python lowercases every
# *_proxy name it finds, so an ambient lowercase https_proxy (near-universal in
# CI runners and images that sit behind an egress proxy) silently wins and the
# request leaves through the wrong proxy. Sourcing the real file also means
# this test exercises what a supervisor actually reads.
if python3 -c 'import requests' 2>/dev/null; then
  if ( . "$WORK/veris.env"; python3 -c "
import json, requests
r = requests.get('https://api.stripe.com/v1/charges', timeout=10)
d = r.json()
assert d['origin'] == 'sandbox', d
assert d['service'] == 'stripe', d
assert d['sandbox'] == 'sbx_e2e', d
" ); then
    pass "python requests: intercepted by the emitted environment"
  else
    fail "python requests: failed"
  fi
else
  printf '  \033[33mSKIP\033[0m python requests not installed\n'
fi

# --- Node ---------------------------------------------------------------------
if command -v node >/dev/null 2>&1; then
  cat > "$WORK/probe.mjs" <<'JS'
const r = await fetch('https://api.stripe.com/v1/charges');
const d = await r.json();
if (d.origin !== 'sandbox') { console.error('unexpected', d); process.exit(1); }
if (d.service !== 'stripe') { console.error('bad service', d); process.exit(1); }
JS
  # Decide up front whether this Node can do --use-env-proxy, so that a genuine
  # interception failure fails the suite instead of hiding behind a SKIP.
  # Measured, not parsed from the version: the flag landed in 22.21 and 24.5,
  # so a numeric >= check wrongly includes every 23.x. This is also exactly how
  # the binary decides whether to emit the flag, so the two cannot disagree.
  nver=$(node -p 'process.versions.node')
  if NODE_OPTIONS=--use-env-proxy node -e '' >/dev/null 2>&1; then
    if ( . "$WORK/veris.env"; node "$WORK/probe.mjs" ); then
      pass "node fetch: intercepted by the emitted environment"
    else
      fail "node fetch: not intercepted on node $nver"
    fi
  else
    printf '  \033[33mSKIP\033[0m node fetch (node %s does not accept --use-env-proxy)\n' "$nver"
  fi
else
  printf '  \033[33mSKIP\033[0m node not installed\n'
fi

# --- env emission -------------------------------------------------------------
envout=$(cat "$WORK/veris.env")
for v in NODE_EXTRA_CA_CERTS REQUESTS_CA_BUNDLE HTTPS_PROXY NO_PROXY VERIS_CANARY; do
  echo "$envout" | grep -q "^export ${v}=" || fail "env: missing $v"
done
# Only where this machine's node accepts the flag: the binary measures the
# local node before emitting it, so on an older node its absence is correct.
if NODE_OPTIONS=--use-env-proxy node -e '' >/dev/null 2>&1; then
  echo "$envout" | grep -q 'NODE_OPTIONS=.*use-env-proxy' || fail "env: NODE_OPTIONS missing --use-env-proxy"
fi
# The variable must not be named so that Codex strips it on inheritance.
echo "$envout" | grep -qE '^export VERIS_[A-Z_]*(KEY|SECRET|TOKEN)=' \
  && fail "env: a variable name contains KEY/SECRET/TOKEN; Codex CLI would strip it" \
  || pass "env: emits the full matrix, and no name Codex would strip"

# The written environment must actually work when sourced.
if ( . "$WORK/veris.env"; curl -sS https://api.stripe.com/v1/charges | grep -q '"origin": "sandbox"' ); then
  pass "env: sourcing the file is sufficient to intercept curl"
else
  fail "env: sourced environment did not intercept"
fi

echo
[ "$FAILED" = 0 ] && { echo "all checks passed"; exit 0; } || { echo "FAILURES"; exit 1; }
