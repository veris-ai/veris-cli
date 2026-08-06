#!/usr/bin/env sh
# Veris container runner entrypoint.
#
# Sets up kernel-level interception, then execs the command it was given. The
# process under test needs no proxy configuration and no per-language CA
# variables: the CA is in the image's system trust store and the redirect is in
# the kernel, so Java, static Go binaries, Apache HttpClient and aiohttp are all
# covered without cooperating.
set -eu

CONFIG="${VERIS_CONFIG:-/veris/config.json}"
CA_DIR="${VERIS_CA_DIR:-/veris/ca}"
HTTP_PORT="${VERIS_TRANSPARENT_HTTP_PORT:-8081}"
HTTPS_PORT="${VERIS_TRANSPARENT_HTTPS_PORT:-8443}"
PROXY_PORT="${VERIS_PROXY_PORT:-8080}"

log() { printf 'veris: %s\n' "$*" >&2; }
die() { printf 'veris: fatal: %s\n' "$*" >&2; exit 1; }

[ -f "$CONFIG" ] || die "no config at $CONFIG (mount it, or set VERIS_CONFIG)"

# The proxy mints the CA on first start, so bring it up before touching trust
# stores.
veris-proxy serve --config "$CONFIG" \
  --listen "0.0.0.0:${PROXY_PORT}" \
  --transparent \
  --transparent-http "0.0.0.0:${HTTP_PORT}" \
  --transparent-https "0.0.0.0:${HTTPS_PORT}" \
  --log-level "${VERIS_LOG_LEVEL:-info}" \
  --log-format "${VERIS_LOG_FORMAT:-json}" &
PROXY_PID=$!

i=0
while [ "$i" -lt 100 ]; do
  if veris-proxy check --proxy "http://127.0.0.1:${PROXY_PORT}" --quiet 2>/dev/null; then break; fi
  kill -0 "$PROXY_PID" 2>/dev/null || die "the proxy exited during startup"
  i=$((i + 1)); sleep 0.1
done
veris-proxy check --proxy "http://127.0.0.1:${PROXY_PORT}" --quiet \
  || die "the proxy never became ready"

# --- system trust -------------------------------------------------------------
# One install here replaces the entire per-runtime CA environment matrix.
CA_PEM="${CA_DIR}/veris-ca.pem"
[ -f "$CA_PEM" ] || die "CA not found at $CA_PEM"

if command -v update-ca-certificates >/dev/null 2>&1; then
  # Debian, Ubuntu and Alpine.
  mkdir -p /usr/local/share/ca-certificates
  cp "$CA_PEM" /usr/local/share/ca-certificates/veris-ca.crt
  update-ca-certificates >/dev/null 2>&1 && log "CA installed into the system trust store"
elif command -v update-ca-trust >/dev/null 2>&1; then
  # RHEL, Fedora and friends.
  cp "$CA_PEM" /etc/pki/ca-trust/source/anchors/veris-ca.pem
  update-ca-trust extract >/dev/null 2>&1 && log "CA installed into the system trust store"
else
  log "warning: no system trust tool found; falling back to environment variables"
fi

# Java keeps its own truststore and never consults the system one. Adding to a
# copy rather than replacing cacerts matters: a truststore holding only our CA
# would break every other TLS connection in the JVM.
if command -v keytool >/dev/null 2>&1; then
  JAVA_HOME_DIR="${JAVA_HOME:-$(dirname "$(dirname "$(command -v java 2>/dev/null || echo /usr/bin/java)")")}"
  for candidate in "$JAVA_HOME_DIR/lib/security/cacerts" "$JAVA_HOME_DIR/jre/lib/security/cacerts"; do
    if [ -f "$candidate" ]; then
      cp "$candidate" /veris/veris-cacerts.jks
      keytool -importcert -noprompt -trustcacerts \
        -alias veris-local-ca -file "$CA_PEM" \
        -keystore /veris/veris-cacerts.jks -storepass changeit >/dev/null 2>&1 \
        && log "Java truststore built at /veris/veris-cacerts.jks"
      JAVA_TOOL_OPTIONS="${JAVA_TOOL_OPTIONS:+$JAVA_TOOL_OPTIONS }-Djavax.net.ssl.trustStore=/veris/veris-cacerts.jks -Djavax.net.ssl.trustStorePassword=changeit"
      export JAVA_TOOL_OPTIONS
      break
    fi
  done
fi

# --- kernel redirect ----------------------------------------------------------
# This is the part that needs CAP_NET_ADMIN. Without it we can still work
# through environment variables, so degrade loudly rather than failing.
REDIRECT_OK=0
if command -v iptables >/dev/null 2>&1; then
  # Exempt the proxy's own traffic, or its upstream calls would be redirected
  # straight back into itself.
  PROXY_UID="$(id -u)"
  if iptables -t nat -N VERIS 2>/dev/null || iptables -t nat -F VERIS 2>/dev/null; then
    iptables -t nat -A VERIS -m owner --uid-owner "$PROXY_UID" -j RETURN 2>/dev/null || true
    iptables -t nat -A VERIS -d 127.0.0.0/8 -j RETURN
    iptables -t nat -A VERIS -d 10.0.0.0/8 -j RETURN
    iptables -t nat -A VERIS -d 172.16.0.0/12 -j RETURN
    iptables -t nat -A VERIS -d 192.168.0.0/16 -j RETURN
    iptables -t nat -A VERIS -p tcp --dport 80  -j REDIRECT --to-port "$HTTP_PORT"
    iptables -t nat -A VERIS -p tcp --dport 443 -j REDIRECT --to-port "$HTTPS_PORT"
    if iptables -t nat -C OUTPUT -p tcp -j VERIS 2>/dev/null || iptables -t nat -A OUTPUT -p tcp -j VERIS; then
      REDIRECT_OK=1
      log "transparent interception active (iptables REDIRECT -> ${HTTP_PORT}/${HTTPS_PORT})"
    fi
  fi
fi

if [ "$REDIRECT_OK" = 0 ]; then
  log "warning: could not install iptables rules. Run with --cap-add=NET_ADMIN for"
  log "warning: language-agnostic interception. Falling back to proxy env vars,"
  log "warning: which do NOT cover Java, static Go binaries or Apache HttpClient."
fi

# Environment variables are still exported. They are redundant when the
# redirect is in place, and they are the whole mechanism when it is not.
eval "$(veris-proxy env --config "$CONFIG" --quiet --proxy-url "http://127.0.0.1:${PROXY_PORT}")"
export VERIS_PROXY_URL="http://127.0.0.1:${PROXY_PORT}"

# Refuse to run the command if interception is not live. A test suite that
# passes without interception proves nothing, which is worse than a failure.
veris-proxy check --proxy "$VERIS_PROXY_URL" --quiet \
  || die "interception is not live; refusing to run the command"

[ "$#" -gt 0 ] || die "no command given"
log "running: $*"
"$@"
STATUS=$?

veris-proxy check --proxy "$VERIS_PROXY_URL" --quiet >/dev/null 2>&1 || true
kill "$PROXY_PID" 2>/dev/null || true
exit "$STATUS"
