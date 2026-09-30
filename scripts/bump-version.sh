#!/bin/bash
# Bump the patch version in version.go (0.0.1 -> 0.0.2).
# Run on EVERY minifactory-go code change, before rebuilding, so the
# reported version moves with each change.
set -euo pipefail
cd "$(dirname "$0")/.."
cur=$(grep -oE 'const version = "[0-9]+\.[0-9]+\.[0-9]+"' version.go | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')
next=$(awk -F. '{printf "%d.%d.%d", $1, $2, $3+1}' <<<"$cur")
sed -i "s/const version = \"$cur\"/const version = \"$next\"/" version.go
echo "$cur -> $next"
