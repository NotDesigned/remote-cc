#!/bin/sh
# Builds rcc for all supported A platforms, each embedding Linux rcc-shim
# binaries for the B side. Output: dist/
set -e
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
LDFLAGS="-s -w -X main.version=$VERSION"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" \
    -o internal/embedded/shims/rcc-shim-linux-$arch ./cmd/rcc-shim
done
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
  os=${target%/*}; arch=${target#*/}; ext=; [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
    -o "dist/rcc-$os-$arch$ext" ./cmd/rcc
done
ls -lh dist
