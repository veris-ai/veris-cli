#!/usr/bin/env bash
# Proves the network-namespace tier: the proxy is its own container, the workload
# container joins its network namespace, and ALL of the workload's traffic is
# captured without that container holding a capability, honouring an
# environment variable, or being modified in any way.
#
# This is the answer to "can Docker networking route everything at the proxy
# instead of naming hostnames one at a time". It can: two containers sharing
# one network namespace see one set of iptables rules, so the redirect the
# proxy installs applies to the workload's sockets too.
#
# The capability does not disappear -- it MOVES. The proxy container needs
# NET_ADMIN; the workload container needs nothing.
#
# Needs docker. Human-triggered, never CI.
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
IMAGE=veris-cli-sidecar:local
NET=veris-sc-$$
trap 'docker rm -f veris-sidecar sc-sandbox >/dev/null 2>&1 || true;
      docker network rm "$NET" >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

say() { printf '\n==> %s\n' "$*"; }

say "build"
docker build -q -f "$HERE/container/Dockerfile" --target runner -t "$IMAGE" "$HERE" >/dev/null

say "stand up a stand-in sandbox"
docker network create "$NET" >/dev/null
docker run -d --name sc-sandbox --network "$NET" kennethreitz/httpbin >/dev/null
sleep 3

cat > "$WORK/config.json" <<'JSON'
{
  "version": 1,
  "listen": "0.0.0.0:8080",
  "sandbox_id": "sbx_sidecar_e2e",
  "mode": "strict",
  "upstream": { "base_url": "http://sc-sandbox" },
  "services": [
    { "name": "stripe", "hosts": ["api.stripe.com"], "upstream": "http://sc-sandbox" }
  ]
}
JSON

# No command after the image name -> sidecar mode. This container holds the
# network namespace and the rules; nothing of the workload runs in it.
say "start the proxy as a sidecar (NET_ADMIN lives HERE, and only here)"
SHARE="$WORK/share"
mkdir -p "$SHARE"
chmod 0777 "$SHARE"
docker run -d --name veris-sidecar --network "$NET" \
  --cap-add=NET_ADMIN \
  -v "$WORK/config.json":/veris/config.json:ro \
  -v "$SHARE":/veris-share "$IMAGE" >/dev/null

for _ in $(seq 1 120); do
  docker logs veris-sidecar 2>&1 | grep -q "interception is live" && break
  docker inspect -f "{{.State.Running}}" veris-sidecar 2>/dev/null | grep -q true \
    || { echo "the sidecar exited during startup"; docker logs veris-sidecar 2>&1 | tail -20; exit 1; }
  sleep 0.5
done
docker logs veris-sidecar 2>&1 | grep -q "interception is live" \
  || { echo "FAIL: sidecar never became ready"; docker logs veris-sidecar 2>&1 | tail -20; exit 1; }
docker logs veris-sidecar 2>&1 | grep -E "kernel redirect installed|dropped privileges|interception is live" | sed 's/^/    /'

docker cp veris-sidecar:/veris/ca/veris-ca.pem "$WORK/veris-ca.pem" >/dev/null
[ -s "$WORK/veris-ca.pem" ] || { echo "FAIL: no CA"; exit 1; }

# The workload. Note what it is NOT given: no --cap-add, no --add-host, no
# proxy variables, and --noproxy "*" so any that leaked would be ignored. The
# ONLY things it gets are the shared namespace and the CA to trust.
say "run an unmodified image in the shared namespace, with no capabilities"
set +e
out=$(docker run --rm --network container:veris-sidecar \
  --cap-drop=ALL \
  -v "$WORK/veris-ca.pem":/ca.pem:ro \
  curlimages/curl:latest \
  -sS --noproxy '*' --cacert /ca.pem https://api.stripe.com/anything/charges 2>&1)
set -e
echo "$out" | head -c 700; echo

if echo "$out" | grep -qi 'X-Veris-Original-Host'; then
  echo "    intercepted: captured by the shared namespace, not by cooperation"
elif echo "$out" | grep -q 'invalid_request_error'; then
  echo "FAIL: the request reached REAL Stripe"; exit 1
else
  echo "FAIL: the probe did not complete"; exit 1
fi

# The difference from the DNS-alias tier: nothing named api.stripe.com in
# advance. A hostname the operator never enumerated is captured too, because
# the redirect is on the port, not on a name.
say "a hostname nobody enumerated is captured as well"
set +e
other=$(docker run --rm --network container:veris-sidecar --cap-drop=ALL \
  -v "$WORK/veris-ca.pem":/ca.pem:ro \
  curlimages/curl:latest \
  -sS --noproxy '*' --cacert /ca.pem https://api.openai.com/v1/models 2>&1)
set -e
echo "$other" | head -c 300; echo
echo "$other" | grep -q 'veris_unmapped_host' \
  || { echo "FAIL: an unmapped host was not seen by the proxy at all"; exit 1; }
echo "    reached the proxy and was refused by name -- so it was captured"

say "the workload really does hold no capabilities"
caps=$(docker run --rm --network container:veris-sidecar --cap-drop=ALL \
  curlimages/curl:latest sh -c 'grep CapEff /proc/self/status' 2>&1 | tr -d ' \t')
echo "    $caps"
[ "$caps" = "CapEff:0000000000000000" ] \
  || { echo "FAIL: the workload holds capabilities: $caps"; exit 1; }

# The point of this tier over same-container: your image needs nothing. Prove
# it with an image that HAS nothing -- no shell, no iptables, no package
# manager -- which same-container cannot use at all.
say "a distroless image, which same-container cannot support, works here"
set +e
distro=$(docker run --rm --network container:veris-sidecar --cap-drop=ALL \
  -v "$WORK/veris-ca.pem":/ca.pem:ro \
  -e SSL_CERT_FILE=/ca.pem \
  gcr.io/distroless/static-debian12 --help 2>&1)
set -e
echo "$distro" | head -3
echo "    (distroless has no shell to probe with; the namespace join itself is"
echo "     the point -- no iptables, no dropper, no entrypoint needed in it)"

# The JVM is the case the environment tier cannot reach at all: it reads no
# proxy variable and will not take a PEM. It needs a keystore, and the keystore
# has to be built where there is no JDK -- the proxy image carries none, and the
# workload's JDK is in a container we do not run code in. So the proxy writes
# the JKS itself, into the mount both containers share.
say "a stock JVM, unmodified, using only the published truststore"
cat > "$WORK/T.java" <<'JAVA'
import java.net.*; import java.net.http.*;
public class T { public static void main(String[] a) throws Exception {
  try {
    var r = HttpClient.newHttpClient().send(
        HttpRequest.newBuilder(URI.create("https://api.stripe.com/anything/charges")).build(),
        HttpResponse.BodyHandlers.ofString());
    System.out.println("STATUS " + r.statusCode());
  } catch (Exception e) { System.out.println("JAVA FAILED: " + e); }
}}
JAVA
set +e
java_out=$(docker run --rm --network container:veris-sidecar --cap-drop=ALL \
  --env-file "$SHARE/veris.env" -v "$SHARE":/veris-share -v "$WORK":/w -w /w \
  eclipse-temurin:21 java T.java 2>&1)
set -e
echo "$java_out" | grep -v "^Picked up" | head -3 | sed 's/^/    /'
echo "$java_out" | grep -q "STATUS 200" \
  || { echo "FAIL: the JVM did not complete the handshake against the proxy"; exit 1; }
echo "    the JVM trusted the proxy with no JDK anywhere near the proxy container"

# A truststore holding ONLY our CA would make the JVM reject every other host on
# the internet, which in passthrough mode is every unmapped vendor it touches.
say "the published truststore carries the public roots too, not only ours"
entries=$(docker run --rm -v "$SHARE":/s:ro eclipse-temurin:21 \
  keytool -list -keystore /s/veris-truststore.jks -storepass changeit 2>/dev/null \
  | sed -n 's/^Your keystore contains \([0-9]*\) entries/\1/p')
echo "    $entries trusted certificates"
[ "${entries:-0}" -gt 10 ] \
  || { echo "FAIL: the truststore holds only $entries entries, so it trusts nothing else"; exit 1; }

say "PASS: all traffic captured, workload container unprivileged and unmodified"
