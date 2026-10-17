#!/bin/bash
# Build release tarballs for the popular platforms.
# Usage: scripts/build-release.sh <version> [outdir]
# Output: dist/minifactory-<version>-<os>-<arch>.tar.gz + sha256sums.txt
# Each tarball contains a single static binary named `minifactory`.
set -euo pipefail
cd "$(dirname "$0")/.."

ver="${1:?usage: build-release.sh <version> [outdir] (e.g. 0.0.21)}"
out="${2:-dist}"
rm -rf "$out"
mkdir -p "$out"

for plat in "linux amd64" "linux arm64" "darwin amd64" "darwin arm64"; do
	set -- $plat
	os="$1"
	arch="$2"
	name="minifactory-${ver}-${os}-${arch}"
	echo "building $name"
	mkdir -p "$out/$name"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags="-s -w" -o "$out/$name/minifactory" .
	tar -czf "$out/$name.tar.gz" -C "$out/$name" minifactory
	rm -rf "$out/$name"
done

(cd "$out" && sha256sum minifactory-*.tar.gz > sha256sums.txt)
echo "---"
ls -la "$out"
