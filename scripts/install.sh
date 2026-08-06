#!/bin/sh
# veris-proxy installer.
#
#   curl -fsSL https://raw.githubusercontent.com/veris-ai/veris-proxy/main/scripts/install.sh | sh
#
# Fetches the latest release binary for this OS/arch and installs it to
# $VERIS_INSTALL_DIR (default ~/.local/bin). No root, no package manager:
# the binary is static, so this works the same on a laptop, a CI runner,
# or inside a container being built.
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
if [ "$VERSION" = "latest" ]; then
  url="https://github.com/$REPO/releases/latest/download/$asset"
else
  url="https://github.com/$REPO/releases/download/$VERSION/$asset"
fi

mkdir -p "$INSTALL_DIR"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

echo "downloading $url"
curl -fsSL -o "$tmp" "$url"
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
