#!/usr/bin/env bash
# Build cpa-auto-router plugin.
#   ./build.sh windows   -> dist/cpa-auto-router.dll  (cross-compile with zig)
#   ./build.sh linux     -> dist/cpa-auto-router.so
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p dist
case "${1:-windows}" in
  windows)
    CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="zig cc -target x86_64-windows-gnu" \
      go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o dist/cpa-auto-router.dll . ;;
  linux)
    CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -buildmode=c-shared -o dist/cpa-auto-router.so . ;;
  *) echo "usage: $0 [windows|linux]"; exit 1 ;;
esac
echo "built into dist/"
