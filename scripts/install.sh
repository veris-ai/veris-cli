#!/bin/sh
# veris-proxy installer.
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
  *) echo "veris-proxy install: unsupported OS: $os (Windows: use scripts/install.ps1, or download the .exe from https://github.com/$REPO/releases)" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
  arm64|aarch64) arch="arm64" ;;
  x86_64|amd64)  arch="amd64" ;;
  *) echo "veris-proxy install: unsupported architecture: $arch" >&2; exit 1 ;;
esac

asset="veris-proxy-$os-$arch"

mkdir -p "$INSTALL_DIR"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

fail() { echo "veris-proxy install: $*" >&2; exit 1; }

# The plain release URL. This is the whole story for a public release.
download_public() {
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/$asset"
  else
    url="https://github.com/$REPO/releases/download/$VERSION/$asset"
  fi
  echo "downloading $url"
  code=$(curl -sSL -o "$tmp" -w '%{http_code}' "$url" || true)
  [ "$code" = "200" ]
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
    grep 'releases/assets/' | grep "\"name\": *\"$asset\"" |
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
    fail "could not download $asset ($VERSION) from $url (HTTP ${code:-000}). Check that $VERSION is a release with a $asset asset; if you are rate limited, export GH_TOKEN and rerun"
  download_with_token ||
    fail "could not download $asset ($VERSION) with a token either (is $VERSION a release with a $asset asset?)"
fi

chmod +x "$tmp"

# Refuse to install something that is not the expected binary.
"$tmp" version >/dev/null 2>&1 ||
  fail "downloaded file is not a runnable veris-proxy binary"

mv "$tmp" "$INSTALL_DIR/veris-proxy"
trap - EXIT
echo "installed $("$INSTALL_DIR/veris-proxy" version) to $INSTALL_DIR/veris-proxy"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: $INSTALL_DIR is not on your PATH; add it to your shell profile" ;;
esac
