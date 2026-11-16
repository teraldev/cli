#!/bin/sh
# Installs the Teral CLI.
#
#   curl -fsSL https://teral.dev/install.sh | sh
#
# Environment:
#   TERAL_VERSION       version to install, e.g. 1.2.0 (default: latest)
#   TERAL_INSTALL_DIR   where to put the binary (default: /usr/local/bin if
#                       writable, else ~/.local/bin)
set -eu

repo="teraldev/cli"

say() { printf '%s\n' "$*" >&2; }
fail() { say "error: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "$1 is required"; }

need uname
need tar
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1"; }
  download() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO- "$1"; }
  download() { wget -qO "$2" "$1"; }
else
  fail "curl or wget is required"
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) fail "unsupported OS $os; download a release from https://github.com/$repo/releases" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture $arch" ;;
esac

version="${TERAL_VERSION:-}"
if [ -z "$version" ]; then
  version=$(fetch "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || fail "could not find the latest release"
fi
version=${version#v}

archive="teral_${version}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/v${version}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "Downloading teral $version for $os/$arch"
download "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
download "$base/checksums.txt" "$tmp/checksums.txt" || fail "checksum download failed"

expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$expected" ] || fail "no checksum for $archive"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
else
  actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
fi
[ "$expected" = "$actual" ] || fail "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" teral

dir="${TERAL_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/teral" "$dir/teral" 2>/dev/null || { cp "$tmp/teral" "$dir/teral" && chmod 0755 "$dir/teral"; }

say "Installed teral $version to $dir/teral"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "Add $dir to your PATH, e.g.: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
esac
say "Get started: teral login"
