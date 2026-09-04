#!/usr/bin/env bash
# Proves `--sandbox <id>` end to end: no config file is ever written by hand,
# and the vendor hostnames come from the table the control plane generates and
# serves with the sandbox -- the only source this binary has -- rather than
# from anything a developer typed.
#
# Uses a Google sandbox on purpose. Google is the case that makes the table
# worth generating: three services share www.googleapis.com and are told
# apart only by path prefix, and /tokeninfo lives on a different host again.
#
# Human-triggered, never CI: it costs a real sandbox.
set -euo pipefail

# The control plane to provision against, and a key for it. Override BASE to
# aim the run at a different environment.
BASE="${VERIS_API_BASE:-https://svc.api.veris.ai}"
API_KEY="${VERIS_API_KEY:?set VERIS_API_KEY to a control-plane API key}"
WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null || true; rm -rf "$WORK"' EXIT

export VERIS_API_KEY="$API_KEY" VERIS_API_BASE="$BASE"
auth=(-H "X-API-Key: $API_KEY")
json=(-H 'Content-Type: application/json')

say() { printf '\n==> %s\n' "$*"; }
jqp() { python3 -c "import json,sys; print(json.load(sys.stdin)$1)"; }

say "build"
( cd "$(dirname "$0")/.." && go build -o "$WORK/veris" ./cmd/veris )
PROXY="$WORK/veris"

# Only now, and only after every tool that reads a real home directory has run:
# `go build` would rebuild its entire module cache here as read-only files.
# ~/.veris is where the sandbox cache lives, and this test must not disturb
# the developer's own.
export HOME="$WORK/home"
mkdir -p "$HOME"

say "provision a google-calendar sandbox"
env_id=$(curl -fsS "${auth[@]}" "${json[@]}" \
  -d '{"name": "proxy-use-e2e", "services": ["google-calendar"]}' \
  "$BASE/v1/environments" | jqp '["id"]')
sandboxes="$BASE/v1/environments/$env_id/sandboxes"
sbx=$(curl -fsS "${auth[@]}" "${json[@]}" -d '{"ttl_minutes": 20}' "$sandboxes" | jqp '["id"]')
echo "    sandbox $sbx"
trap 'curl -fsS -X DELETE "${auth[@]}" "$sandboxes/$sbx" >/dev/null 2>&1 || true;
      chmod -R u+w "$WORK" 2>/dev/null || true; rm -rf "$WORK"' EXIT

for _ in $(seq 1 60); do
  status=$(curl -fsS "${auth[@]}" "$sandboxes/$sbx" | jqp '["status"]')
  [ "$status" = ready ] && break
  sleep 2
done
[ "$status" = ready ] || { echo "sandbox never became ready ($status)"; exit 1; }
for _ in $(seq 1 60); do
  curl -fsS "$BASE/s/$sbx/google-calendar/veris/health" >/dev/null 2>&1 && break
  sleep 2
done
echo "    ready"

# The whole point: nothing is authored, and nothing is remembered either.
say "the routing derived from the sandbox id alone"
"$PROXY" serve --sandbox "$sbx" --print-routes | tee "$WORK/routes.txt"
"$PROXY" serve --sandbox "$sbx" --print-routes --log-level error >/dev/null
grep -q "google-identity" "$WORK/routes.txt" \
  || { echo "FAIL: google-identity was not auto-added"; exit 1; }

# The same sandbox id also drives the environment the proxy hands a command,
# written by serve rather than computed by a second command.
"$PROXY" serve --sandbox "$sbx" --listen 127.0.0.1:0 \
  --write-env "$WORK/veris.env" --ready-file "$WORK/ready" --log-level error &
SERVE_PID=$!
for _ in $(seq 1 60); do [ -s "$WORK/ready" ] && break; sleep 0.2; done
kill "$SERVE_PID" 2>/dev/null || true
python3 - "$WORK/veris.env" <<'PYCHK'
import re, sys
names = set(re.findall(r"^export (\w+)=", open(sys.argv[1]).read(), re.M))
assert {"HTTPS_PROXY", "SSL_CERT_FILE"} <= names, sorted(names)
print(f"    the environment exposes {len(names)} variables, from the same sandbox id")
PYCHK

"$PROXY" serve --sandbox "$sbx" --print-routes >/dev/null
python3 - "$WORK/routes.txt" <<'PYCHK'
import sys
text = open(sys.argv[1]).read()
assert "oauth2.googleapis.com/tokeninfo" in text, text
assert "www.googleapis.com/tokeninfo" not in text, text
assert "www.googleapis.com/calendar/v3" in text, text
print("    /tokeninfo on oauth2.googleapis.com, calendar on www.googleapis.com")
PYCHK

# The known-good access token google-identity publishes in its world, read the
# way a client is meant to read it rather than hardcoded here. Calendar is a
# family MEMBER: it verifies the bearer by introspection against its sibling
# issuer, so an invented token earns a genuine Google 401 -- which is the mock
# behaving correctly, not a bug.
TOKEN=$(curl -fsS "$BASE/s/$sbx/google-identity/veris/data?entity_type=oauth_tokens" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["rows"][0]["access_token"])')
[ -n "$TOKEN" ] || { echo "FAIL: google-identity published no token to read"; exit 1; }
echo "    read a published token from google-identity's world"

# No --config anywhere below.
say "run an unmodified client with no config file at all"
set +e
VERIS_TOKEN="$TOKEN" "$PROXY" run --sandbox "$sbx" --require-service google-calendar -- \
  sh -c 'curl -sS -o "$0/body.json" -w "%{http_code}" \
           -H "Authorization: Bearer $VERIS_TOKEN" \
           https://www.googleapis.com/calendar/v3/users/me/calendarList' "$WORK" > "$WORK/code"
status=$?
set -e
code=$(cat "$WORK/code" 2>/dev/null || echo none)
echo "    HTTP $code, veris exit $status"
head -c 400 "$WORK/body.json" 2>/dev/null; echo

[ "$status" = 0 ] || { echo "FAIL: veris run exited $status"; exit 1; }
[ "$code" = 200 ] || { echo "FAIL: expected 200 from the simulated Calendar"; exit 1; }
grep -q 'calendar#calendarList' "$WORK/body.json" \
  || { echo "FAIL: response is not a Calendar payload"; exit 1; }

# --sandbox carries no state between commands, so two suites can run at once.
say "--sandbox carries nothing between commands"
set +e
VERIS_TOKEN="$TOKEN" "$PROXY" run --sandbox "$sbx" \
  --require-service google-calendar --quiet -- \
  sh -c 'curl -sS -o /dev/null -w "%{http_code}" \
           -H "Authorization: Bearer $VERIS_TOKEN" \
           https://www.googleapis.com/calendar/v3/users/me/calendarList' > "$WORK/code2"
persandbox=$?
set -e
echo "    HTTP $(cat "$WORK/code2"), exit $persandbox, with nothing selected"
[ "$persandbox" = 0 ] || { echo "FAIL: --sandbox run exited $persandbox"; exit 1; }
[ "$(cat "$WORK/code2")" = 200 ] || { echo "FAIL: expected 200"; exit 1; }

# Nothing persists, so the next command has nothing to fall back on. That is
# the property that lets two suites run against two sandboxes at once.
say "with no sandbox named at all, it refuses and says how"
set +e
out=$("$PROXY" run -- true 2>&1); bare=$?
set -e
[ "$bare" != 0 ] || { echo "FAIL: run succeeded with no sandbox named"; exit 1; }
echo "$out" | grep -q -- "--sandbox" || { echo "FAIL: unhelpful error: $out"; exit 1; }
echo "    refused, naming the fix"

# The environment variable is the other way to say it, which is what a CI job
# sets once for a whole pipeline.
say "\$VERIS_SANDBOX_ID is equivalent to the flag"
set +e
VERIS_SANDBOX_ID="$sbx" VERIS_TOKEN="$TOKEN" "$PROXY" run \
  --require-service google-calendar --quiet -- \
  sh -c 'curl -sS -o /dev/null -w "%{http_code}" \
           -H "Authorization: Bearer $VERIS_TOKEN" \
           https://www.googleapis.com/calendar/v3/users/me/calendarList' > "$WORK/code3"
viaenv=$?
set -e
echo "    HTTP $(cat "$WORK/code3"), exit $viaenv, via the environment"
[ "$viaenv" = 0 ] && [ "$(cat "$WORK/code3")" = 200 ] \
  || { echo "FAIL: \$VERIS_SANDBOX_ID did not route"; exit 1; }

say "PASS: --sandbox run, with no config file and no stored state"
