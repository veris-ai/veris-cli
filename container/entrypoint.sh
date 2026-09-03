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
PROXY_UID="${VERIS_PROXY_UID:-14741}"

# iptables lives in sbin, which a non-root PATH and many slim images leave out.
# `serve` looks there itself, but it inherits this PATH, so widen it here.
case ":${PATH}:" in
  *:/usr/sbin:*) ;;
  *) PATH="${PATH}:/usr/sbin:/sbin" ;;
esac
export PATH

log() { printf 'veris: %s\n' "$*" >&2; }
die() { printf 'veris: fatal: %s\n' "$*" >&2; exit 1; }

# Either name a sandbox (the usual way) or mount a config file. The sandbox id
# needs an API key for the first fetch and nothing after that.
if [ -n "${VERIS_SANDBOX_ID:-}" ]; then
  TARGET="--sandbox ${VERIS_SANDBOX_ID}"
elif [ -n "${VERIS_ENVIRONMENT_ID:-}" ]; then
  # serve deploys the sandbox itself from this, so there is nothing to route
  # at yet -- the flag IS the target.
  TARGET="--environment ${VERIS_ENVIRONMENT_ID}"
  [ -n "${VERIS_TTL_MINUTES:-}" ] && TARGET="$TARGET --ttl-minutes ${VERIS_TTL_MINUTES}"
elif [ -f "$CONFIG" ]; then
  TARGET="--config $CONFIG"
else
  die "nothing to route: set VERIS_SANDBOX_ID (with VERIS_API_KEY), or mount a
     config at $CONFIG"
fi

# The redirect exempts the proxy by uid, so the proxy MUST NOT share a uid with
# the command under test -- one exemption rule cannot tell them apart, and the
# command would sail past interception with the rules installed and everything
# apparently healthy. Refusing beats a silent no-op.
WORKLOAD_UID="$(id -u)"
if [ "$WORKLOAD_UID" = "$PROXY_UID" ]; then
  die "the command would run as uid ${PROXY_UID}, the proxy's own uid, so the
     redirect exemption would exempt it too. Set VERIS_PROXY_UID to an unused uid."
fi

# Somewhere the DROPPED proxy can write. Dropping privileges does not change
# HOME, so an unprivileged proxy inheriting root's environment would put its CA
# and its sandbox cache under /root and fail on the first write -- in a derived
# image, where nobody thought to chown anything.
STATE_DIR="${VERIS_STATE_DIR:-/veris}"
# Everything the workload container needs lands here, in one directory, so a
# single -v gets the certificate and the environment file onto the host at
# once. Deliberately NOT the CA directory: that also holds the private key, and
# the workload has no business being able to sign with it.
SHARE_DIR="${VERIS_SHARE_DIR:-/veris-share}"
mkdir -p "$STATE_DIR" "$CA_DIR" "$SHARE_DIR"
# serve chowns what it needs before dropping; this only makes the share
# readable by whoever mounted it, which is a different user again.
chmod 0755 "$SHARE_DIR" 2>/dev/null || true

# NAMESPACE MODE: no command, so nothing of yours runs in THIS container. Your
# image runs in its own, joining this one's network namespace
# (`--network container:<this>`), and the two share one set of iptables rules --
# so the redirect applies to your sockets without your image holding a
# capability, containing iptables, or being modified at all.
#
# Decided here, before the proxy starts, because it changes who reads the
# environment file and therefore what format it must be written in: a shell in
# this container, or `docker run --env-file` on the host.
if [ "$#" -eq 0 ]; then
  NAMESPACE_MODE=1
else
  NAMESPACE_MODE=0
fi

# Removed first either way, so a marker from an earlier run cannot answer this
# one's readiness question.
READY_FILE="${STATE_DIR}/ready.$$"
if [ "$NAMESPACE_MODE" = 1 ]; then
  # In the share directory, under a fixed name: its reader is outside this
  # container and cannot guess a pid. The directory itself is per-run.
  READY_FILE="${SHARE_DIR}/ready"
  # A FIXED, predictable path here: its reader is a human on the host, running
  # `docker run --env-file`, who cannot be expected to guess a pid.
  ENV_FILE="${SHARE_DIR}/veris.env"
  ENV_FORMAT=docker
  # Only the certificate is published, and the env file names it where the
  # reader will find it rather than where we keep it.
  CA_PUBLIC="${SHARE_DIR}/veris-ca.pem"
  # Trust variables only. The kernel redirect already routes the workload's
  # traffic, and handing it HTTP_PROXY as well would make a cooperating client
  # take the explicit-proxy path instead -- a different transport from the one
  # this tier exists to provide, and one that would keep working if the
  # redirect silently stopped.
  ENV_SCOPE="--env-trust-only"
else
  ENV_FILE="${STATE_DIR}/env.$$"
  ENV_FORMAT=posix
  CA_PUBLIC=""
  # Same container: the env vars ARE the fallback when the redirect could not
  # be installed, so they carry routing too.
  ENV_SCOPE=""
fi
rm -f "$READY_FILE" "$ENV_FILE"

# One process does the whole setup: it installs the kernel redirect and the CA
# into this container's trust store, drops itself to PROXY_UID, and only then
# binds a listener. So the readiness marker below means interception is
# actually live, not merely that something is listening.
# shellcheck disable=SC2086 # TARGET is a deliberately word-split flag pair

# HOME so the proxy's cache lands somewhere it will still own after it drops
# its own privileges, rather than in root's home.
# Ingress is opt-in by port: there is nothing to publish without one. It is not
# gated on which services declare delivery -- a callback going nowhere because a
# declaration was missing is the failure this refuses.
EXPOSE_ARGS=""
if [ -n "${VERIS_EXPOSE:-}" ]; then
  EXPOSE_ARGS="--expose ${VERIS_EXPOSE}"
  [ -n "${VERIS_EXPOSE_HOST:-}" ] && EXPOSE_ARGS="$EXPOSE_ARGS --expose-host ${VERIS_EXPOSE_HOST}"
  [ -n "${VERIS_TUNNEL_HOSTNAME:-}" ] && EXPOSE_ARGS="$EXPOSE_ARGS --expose-hostname ${VERIS_TUNNEL_HOSTNAME}"
fi

# shellcheck disable=SC2086 # EXPOSE_ARGS is a deliberately word-split flag list
env HOME="$STATE_DIR" veris serve $TARGET $EXPOSE_ARGS --proxy-uid "$PROXY_UID" \
  --ca-dir "$CA_DIR" ${VERIS_STRICT:+--strict} \
  --listen "0.0.0.0:${PROXY_PORT}" \
  --transparent \
  --transparent-http "0.0.0.0:${HTTP_PORT}" \
  --transparent-https "0.0.0.0:${HTTPS_PORT}" \
  --write-env "$ENV_FILE" --env-format "$ENV_FORMAT" \
  ${CA_PUBLIC:+--ca-public-path "$CA_PUBLIC"} $ENV_SCOPE \
  --ready-file "$READY_FILE" \
  --log-level "${VERIS_LOG_LEVEL:-info}" \
  --log-format "${VERIS_LOG_FORMAT:-json}" &
PROXY_PID=$!

# The signal-forwarding trap is installed HERE, not after the readiness loop:
# --environment can spend minutes provisioning before the marker appears, and a
# stop during that window would otherwise SIGKILL the container with the sandbox
# already created and no chance to delete it. Docker signals PID 1 (this shell);
# the proxy is a background child and needs the signal forwarded.
trap 'kill -TERM "$PROXY_PID" 2>/dev/null' TERM INT

# Waiting on a file rather than probing a port: this entrypoint runs in
# whatever image the customer brought, and debian-slim and python-slim have no
# curl, no wget, no nc, and sh has no /dev/tcp. `test` and `sleep` are always
# there. The marker is written last, so its existence also means ENV_FILE is
# complete.
i=0
# --environment provisions a sandbox before the marker is written, and that is
# allowed minutes for scheduling and image pulls. Thirty seconds would kill a
# healthy deployment as a startup failure -- and the host now waits six.
READY_TRIES=300
[ -n "${VERIS_ENVIRONMENT_ID:-}" ] && READY_TRIES=5400  # 9 min at 0.1s, matching the host

while [ ! -s "$READY_FILE" ]; do
  kill -0 "$PROXY_PID" 2>/dev/null || die "the proxy exited during startup"
  [ "$i" -lt "$READY_TRIES" ] || die "the proxy never became ready"
  i=$((i + 1)); sleep 0.1
done

# --- what the proxy could not do for itself -------------------------------
CA_PEM="${CA_DIR}/veris-ca.pem"
[ -f "$CA_PEM" ] || die "CA not found at $CA_PEM"

if [ "$NAMESPACE_MODE" = 1 ]; then
  # A docker-format file is deliberately not sourceable -- JAVA_TOOL_OPTIONS
  # contains spaces -- so read the one value this script needs. check asserts
  # WHICH run answered, not merely that something did.
  [ -s "$ENV_FILE" ] || die "the proxy wrote no environment file"
  CANARY="$(sed -n 's/^VERIS_CANARY=//p' "$ENV_FILE")"
  veris check --proxy "http://127.0.0.1:${PROXY_PORT}" \
    --expect-canary "$CANARY" --quiet \
    || die "the proxy is not intercepting"

  ME="$(hostname)"
  log ""
  log "  interception is live."
  log ""
  log "  Run your image against it -- it needs no capabilities, no iptables,"
  log "  no entrypoint change, and no modification:"
  log ""
  log "    docker run --rm --network container:${ME} --cap-drop=ALL \\"
  log "      --env-file <hostdir>/veris.env -v <hostdir>:${SHARE_DIR} \\"
  log "      <your-image> <your-command>"
  log ""
  log "  where <hostdir> is whatever you mounted at ${SHARE_DIR}. Everything"
  log "  the workload needs is there now: veris.env, veris-ca.pem, the"
  log "  veris-ca-bundle.pem the runtimes that REPLACE their roots read, and"
  log "  veris-truststore.jks for the JVM. The CA private key is NOT among"
  log "  them -- it stays in this container."
  log ""
  log "  veris.env carries CA trust only. Routing is the kernel's job here, so"
  log "  your client is never told a proxy exists."
  log ""

  # The trap forwarding TERM to the proxy is installed up front (above), so it
  # already covers provisioning. Forwarded, the proxy shuts down and ATTEMPTS
  # its deferred cleanup (clearing the callback registration, deleting a sandbox
  # it created). That cleanup is best-effort: a sandbox not deleted promptly is
  # still bounded by the TTL every sandbox carries, so it expires rather than
  # leaking. Prompt deletion is the optimisation; the TTL is the guarantee.
  # `wait` returns as soon as a trap fires, so it is retried until the child is
  # genuinely gone rather than abandoning it mid-cleanup.
  wait "$PROXY_PID"
  rc=$?
  while kill -0 "$PROXY_PID" 2>/dev/null; do
    wait "$PROXY_PID"
    rc=$?
  done
  exit $rc
fi

# Written by the proxy itself at startup, before the redirect existed and while
# it still had the resolved sandbox in hand. Reading it here means no second
# resolve -- which as a root-run command would have had its own control-plane
# call redirected into the proxy, since the redirect exempts only PROXY_UID.
[ -s "$ENV_FILE" ] || die "the proxy wrote no environment file"
# shellcheck source=/dev/null  # written by serve at runtime, not in the repo
. "$ENV_FILE"
export VERIS_PROXY_URL="http://127.0.0.1:${PROXY_PORT}"

# Refuse to run the command if interception is not live. A test suite that
# passes without interception proves nothing, which is worse than a failure.
# The canary comes from the environment file above, so this asserts the proxy
# answering is the one this script started -- not one inherited from an image
# layer or a sidecar pointing at a different sandbox.
veris check --proxy "$VERIS_PROXY_URL" --quiet \
  || die "interception is not live; refusing to run the command"

log "running: $*"
# Under `set -e` a bare "$@" would exit here on a nonzero status, before the
# proxy is stopped -- so a failing test suite leaked the proxy every time.
if "$@"; then STATUS=0; else STATUS=$?; fi

rm -f "$READY_FILE" "$ENV_FILE"
kill -TERM "$PROXY_PID" 2>/dev/null || true
wait "$PROXY_PID" 2>/dev/null || true
exit "$STATUS"
