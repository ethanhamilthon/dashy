#!/bin/sh
# Install the latest dashy release.
# Usage: curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/dashy/main/scripts/install.sh | sh
set -eu

repo="ethanhamilthon/dashy"
bindir="${DASHY_BINDIR:-$HOME/.local/bin}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
amd64 | x86_64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*)
	echo "unsupported architecture: $arch" >&2
	exit 1
	;;
esac
case "$os" in
darwin | linux) ;;
*)
	echo "unsupported operating system: $os" >&2
	exit 1
	;;
esac

version="${DASHY_VERSION:-}"
if [ -z "$version" ]; then
	version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
fi
[ -n "$version" ] ||
	{
		echo "could not resolve the latest release" >&2
		exit 1
	}

asset="dashy_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"
echo "downloading dashy $version ($os/$arch)"
curl -fsSL -o "$tmp/$asset" "$base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"

expected=$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] ||
	{
		echo "checksums.txt does not list $asset" >&2
		exit 1
	}
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
else
	actual=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
fi
[ "$expected" = "$actual" ] ||
	{
		echo "checksum mismatch for $asset" >&2
		exit 1
	}

tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$bindir"
install -m 755 "$tmp/dashy" "$bindir/dashy"
echo "installed $bindir/dashy"
case ":$PATH:" in
*":$bindir:"*) ;;
*) echo "add $bindir to PATH to run dashy" ;;
esac
