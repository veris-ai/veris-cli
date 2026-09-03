#!/bin/sh
# veris installer.
#
#   curl -LsSf https://raw.githubusercontent.com/veris-ai/veris-proxy/main/scripts/install.sh | sh
#
# Fetches the release binary for this OS/arch and installs it to
# $VERIS_INSTALL_DIR (default ~/.local/bin). No root, no package manager:
# the binary is static, so this works the same on a laptop, a CI runner,
# or inside a container being built.
#
#   VERIS_PROXY_VERSION=v0.8.0   pin a version (default: latest)
#   VERIS_INSTALL_DIR=/usr/local/bin   install somewhere else
#
# Releases up to v0.8.1 publish the binary as veris-proxy-<os>-<arch>; later
# ones as veris-<os>-<arch>. Both names are tried, so a pinned older version
# and the latest release install the same way.
#
# GH_TOKEN/GITHUB_TOKEN, or an authenticated `gh`, are used as a fallback
# when the plain download is refused -- which also covers a GitHub API rate
# limit on a shared CI runner.
set -eu

REPO="veris-ai/veris-proxy"
INSTALL_DIR="${VERIS_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VERIS_PROXY_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "veris install: unsupported OS: $os (Windows: use scripts/install.ps1, or download the .exe from https://github.com/$REPO/releases)" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64)  arch="amd64" ;;
  *) echo "veris install: unsupported architecture: $arch" >&2; exit 1 ;;
esac

asset="veris-$os-$arch"
legacy_asset="veris-proxy-$os-$arch"

mkdir -p "$INSTALL_DIR"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

fail() { echo "veris install: $*" >&2; exit 1; }

# The plain release URL for one asset name. This is the whole story for a
# public release.
download_public_asset() {
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/$1"
  else
    url="https://github.com/$REPO/releases/download/$VERSION/$1"
  fi
  echo "downloading $url"
  code=$(curl -sSL -o "$tmp" -w '%{http_code}' "$url" || true)
  [ "$code" = "200" ]
}

# The new asset name first, then the name releases carried before the rename.
download_public() {
  download_public_asset "$asset" || download_public_asset "$legacy_asset"
}

# Resolve the release through the REST API and fetch the asset by id with
# $token. curl drops the Authorization header on the cross-host redirect to
# the asset CDN, which is what the CDN's signed URL requires. No jq: one
# asset object per line, then the line naming our asset.
download_with_token() {
  api="https://api.github.com/repos/$REPO/releases"
  if [ "$VERSION" = "latest" ]; then
    release_url="$api/latest"
  else
    release_url="$api/tags/$VERSION"
  fi
  echo "downloading $asset ($VERSION) with a token via $release_url"
  release_json=$(curl -fsSL -H "Authorization: Bearer $token" -H "Accept: application/vnd.github+json" "$release_url") ||
    return 1
  asset_id=$(printf '%s' "$release_json" | tr -d '\n' | tr '{' '\n' |
    grep 'releases/assets/' | grep -e "\"name\": *\"$asset\"" -e "\"name\": *\"$legacy_asset\"" |
    sed -n 's|.*releases/assets/\([0-9][0-9]*\)".*|\1|p' | head -n 1)
  [ -n "$asset_id" ] || return 1
  curl -fsSL -o "$tmp" -H "Authorization: Bearer $token" -H "Accept: application/octet-stream" "$api/assets/$asset_id"
}

if ! download_public; then
  token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
  if [ -z "$token" ] && command -v gh >/dev/null 2>&1; then
    token=$(gh auth token 2>/dev/null) || token=""
  fi
  [ -n "$token" ] ||
    fail "could not download $asset ($VERSION) from $url (HTTP ${code:-000}). Check that $VERSION is a release with a $asset (or $legacy_asset) asset; if you are rate limited, export GH_TOKEN and rerun"
  download_with_token ||
    fail "could not download $asset ($VERSION) with a token either (is $VERSION a release with a $asset or $legacy_asset asset?)"
fi

chmod +x "$tmp"

# Refuse to install something that is not the expected binary.
"$tmp" version >/dev/null 2>&1 ||
  fail "downloaded file is not a runnable veris binary"

mv "$tmp" "$INSTALL_DIR/veris"
trap - EXIT
echo "installed $("$INSTALL_DIR/veris" version) to $INSTALL_DIR/veris"

# The binary used to be called veris-proxy, and scripts, skills and CI
# configs still invoke it by that name. A shim beside the binary keeps every
# one of them working. It prefers its sibling, which works before
# $INSTALL_DIR is on the PATH, and falls back to whatever `veris` the PATH
# finds, so a shim reached through a symlink from elsewhere still runs.
# Written to a temp file and moved into place: on an upgrade the old name
# is the previous real binary, which Linux refuses to truncate while a
# `veris-proxy serve` from it is still running (ETXTBSY); a rename replaces
# the inode instead.
shim=$(mktemp "$INSTALL_DIR/.veris-proxy.XXXXXX")
printf '#!/bin/sh\nd=$(dirname "$0")\n[ -x "$d/veris" ] && exec "$d/veris" "$@"\nexec veris "$@"\n' > "$shim"
chmod +x "$shim"
mv "$shim" "$INSTALL_DIR/veris-proxy"
echo "installed a veris-proxy shim to $INSTALL_DIR/veris-proxy"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: $INSTALL_DIR is not on your PATH; add it to your shell profile" ;;
esac
