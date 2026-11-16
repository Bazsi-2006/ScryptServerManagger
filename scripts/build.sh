#!/bin/sh
# Build standalone Linux executables (x86_64 and ARM64) into dist/.
# Requires only the Go toolchain; no C compiler, Node.js or other runtime.
set -eu
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
mkdir -p dist
for arch in amd64 arm64; do
  echo "building linux/$arch ($VERSION)"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "dist/server-manager-linux-$arch" ./cmd/server-manager
done
(cd dist && sha256sum server-manager-linux-amd64 server-manager-linux-arm64 > SHA256SUMS)
ls -l dist
