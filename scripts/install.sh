#!/bin/sh
# veris-proxy installer.
#
#   gh api -H 'Accept: application/vnd.github.raw+json' \
#     repos/veris-ai/veris-proxy/contents/scripts/install.sh | sh
#
# Fetches the latest release binary for this OS/arch and installs it to
# $VERIS_INSTALL_DIR (default ~/.local/bin). No root, no package manager:
# the binary is static, so this works the same on a laptop, a CI runner,
# or inside a container being built.
#
# The repository is private, so release assets need repo access. The
# download uses, in order: an authenticated `gh` (`gh auth login`), a
# GH_TOKEN/GITHUB_TOKEN via the REST API, and finally the plain public
# release URL (which works once the repo or its releases are public).
set -eu

REPO="veris-ai/veris-proxy"
INSTALL_DIR="${VERIS_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${VERIS_PROXY_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "veris-proxy install: unsupported OS: $os (windows: download the .exe from https://github.com/$REPO/releases)" >&2; exit 1 ;;
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
    fail "could not read $release_url with the token (401: bad token; 404: no access to $REPO, or $VERSION is not a release)"
  asset_id=$(printf '%s' "$release_json" | tr -d '\n' | tr '{' '\n' |
    grep 'releases/assets/' | grep "\"name\": *\"$asset\"" |
    sed -n 's|.*releases/assets/\([0-9][0-9]*\)".*|\1|p' | head -n 1)
  [ -n "$asset_id" ] || fail "release $VERSION has no asset named $asset"
  curl -fsSL -o "$tmp" -H "Authorization: Bearer $token" -H "Accept: application/octet-stream" "$api/assets/$asset_id" ||
    fail "download of $asset (asset $asset_id) failed"
}

token="${GH_TOKEN:-${GITHUB_TOKEN:-}}"

if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
  # 1. An authenticated gh resolves the release and streams the asset,
  #    private repo or not.
  echo "downloading $asset ($VERSION) with gh release download"
  tag=""
  [ "$VERSION" = "latest" ] || tag="$VERSION"
  if ! gh release download ${tag:+"$tag"} --repo "$REPO" --pattern "$asset" -O "$tmp" --clobber; then
    # gh's tag lookup also goes through GraphQL, which has outages of its
    # own; the REST path with the same token does not depend on it.
    token=$(gh auth token 2>/dev/null) || token=""
    [ -n "$token" ] || fail "gh release download failed (does this gh account have access to $REPO? is $VERSION a release with a $asset asset?)"
    echo "gh release download failed; retrying through the REST API with gh's token"
    download_with_token
  fi
elif [ -n "$token" ]; then
  # 2. A token without gh (CI, minimal containers).
  download_with_token
else
  # 3. No credentials: the plain release URL. This is the whole story once
  #    the repo (or its releases) is public; while it is private it 404s.
  if [ "$VERSION" = "latest" ]; then
    url="https://github.com/$REPO/releases/latest/download/$asset"
  else
    url="https://github.com/$REPO/releases/download/$VERSION/$asset"
  fi
  echo "downloading $url"
  code=$(curl -sSL -o "$tmp" -w '%{http_code}' "$url" || true)
  case "$code" in
    200) ;;
    404) fail "$url answered 404 — repo access is required while $REPO is private: authenticate gh (gh auth login) or export GH_TOKEN, then rerun (if you already have access, check that $VERSION is a release with a $asset asset)" ;;
    *) fail "download of $url failed (HTTP ${code:-000})" ;;
  esac
fi

chmod +x "$tmp"

# Refuse to install something that is not the expected binary.
"$tmp" version >/dev/null 2>&1 || {
  echo "veris-proxy install: downloaded file is not a runnable veris-proxy binary" >&2
  exit 1
}

mv "$tmp" "$INSTALL_DIR/veris-proxy"
trap - EXIT
echo "installed $("$INSTALL_DIR/veris-proxy" version) to $INSTALL_DIR/veris-proxy"

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) echo "note: $INSTALL_DIR is not on your PATH; add it to your shell profile" ;;
esac
