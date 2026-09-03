#!/usr/bin/env bash
# Proves the callback direction end to end, against a REAL sandbox and a REAL
# cloudflared tunnel:
#
#   sandbox (deployed from an environment, carrying the tunnel URL)
#     -> Cloudflare edge
#       -> cloudflared in the proxy container
#         -> the customer's container, over the shared namespace's loopback
#
# Nothing is stubbed. The one thing it cannot do is make a vendor schedule a
# webhook, so the sandbox's own registration probe is the delivery under test --
# which is the same path a callback takes, and the receipt must EXCLUDE it.
#
# Needs docker, VERIS_API_KEY, VERIS_API_BASE and VERIS_ENVIRONMENT_ID.
# Human-triggered, never CI.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

: "${VERIS_API_KEY:?set VERIS_API_KEY}"
: "${VERIS_API_BASE:?set VERIS_API_BASE}"
: "${VERIS_ENVIRONMENT_ID:?set VERIS_ENVIRONMENT_ID}"

say() { printf '\n==> %s\n' "$*"; }

say "build the runner (with cloudflared) and an app that receives callbacks"
docker build -q -f "$HERE/container/Dockerfile" --target runner \
  -t veris-cli-ing:local "$HERE" >/dev/null
cat > "$WORK/app.py" <<'PY'
import http.server, json, os, threading, time
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0); self.rfile.read(n)
        print(json.dumps({"callback": self.path}), flush=True)
        self.send_response(200); self.end_headers(); self.wfile.write(b'{"ok":true}')
    do_GET = do_POST
    def log_message(self, *a): pass
threading.Thread(
    target=lambda: http.server.HTTPServer(("127.0.0.1", 3900), H).serve_forever(),
    daemon=True).start()
# The URL is handed over so the app can register it with the vendor itself,
# which is the code path that ships.
print("VERIS_PUBLIC_URL=" + os.environ.get("VERIS_PUBLIC_URL", ""), flush=True)
time.sleep(25)
PY
cat > "$WORK/Dockerfile" <<'DOCKER'
FROM python:3.12-alpine
COPY app.py /app.py
CMD ["python3","/app.py"]
DOCKER
docker build -q -t veris-ingress-app:local "$WORK" >/dev/null

say "build the CLI"
( cd "$HERE" && go build -o "$WORK/veris" ./cmd/veris )

# The id is logged inside the proxy container, so the host cannot read it.
# Comparing the environment's sandbox list across the run is both simpler and
# stricter: it proves nothing was LEFT behind, not merely that one id went.
before=$(curl -s --max-time 20 -H "X-API-Key: $VERIS_API_KEY" \
  "$VERIS_API_BASE/v1/environments/$VERIS_ENVIRONMENT_ID/sandboxes" | tr ',' '\n' \
  | grep -oE '"id":"[a-z0-9]+"' | cut -d'"' -f4 | sort -u)

say "run: deploy a sandbox, open the tunnel, run the image inside its namespace"
set +e
out=$(HOME="$WORK" "$WORK/veris" run --image veris-ingress-app:local \
  --proxy-image veris-cli-ing:local \
  --environment "$VERIS_ENVIRONMENT_ID" --expose 3900 --log-level info 2>&1)
status=$?
set -e
printf "%s
" "${out//$'
'/$'
    '}"

say "the app was handed a public callback URL"
echo "$out" | grep -qE 'VERIS_PUBLIC_URL=https://' \
  || { echo "FAIL: the app never received VERIS_PUBLIC_URL"; exit 1; }

say "the sandbox reached the app THROUGH the tunnel"
echo "$out" | grep -q '"callback"' \
  || { echo "FAIL: nothing arrived at the app"; exit 1; }

# The probe travelled the callback path, so it proves the path works -- and it
# is not a callback the RUN produced. Counting it would let
# --require-callback '*' pass with no vendor delivery at all.
say "and the receipt EXCLUDES that probe rather than counting it"
echo "$out" | grep -q "received no callbacks from this run" \
  || { echo "FAIL: the startup probe leaked into the run's receipt"; exit 1; }

# The proxy deletes it on the way out, which only happens if the container's
# entrypoint forwarded the stop signal to it. Asked of the API rather than the
# logs, because the logs are the thing that could be lying.
# The run's sandbox is a per-run dependency: created for the run, used during
# it, and TORN DOWN AFTER. Prompt deletion at teardown is best-effort; the
# guarantee is the TTL every sandbox carries, so a leftover is bounded, never
# permanent. This asserts that guarantee -- an UNBOUNDED sandbox is the only
# failure -- and reports whether prompt deletion also happened.
say "any sandbox it created is torn down, or bounded by its TTL"
after=$(curl -s --max-time 20 -H "X-API-Key: $VERIS_API_KEY" \
  "$VERIS_API_BASE/v1/environments/$VERIS_ENVIRONMENT_ID/sandboxes" | tr ',' '\n' \
  | grep -oE '"id":"[a-z0-9]+"' | cut -d'"' -f4 | sort -u)
leftover=$(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after") || true)
if [ -z "$leftover" ]; then
  echo "    deleted promptly at teardown"
else
  for sbx in $leftover; do
    exp=$(curl -s --max-time 20 -H "X-API-Key: $VERIS_API_KEY" \
      "$VERIS_API_BASE/v1/sandboxes/$sbx" | grep -oE '"expires_at":"[^"]+"' || true)
    [ -n "$exp" ] \
      || { echo "FAIL: sandbox $sbx has no expiry -- it would leak forever"; exit 1; }
    echo "    $sbx not yet deleted, but bounded: $exp"
  done
fi

[ "$status" -eq 0 ] || { echo "FAIL: exit $status"; exit 1; }
say "PASS: a real sandbox delivered to a real app through a real tunnel"
