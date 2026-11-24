#!/bin/sh
# Builds rcc for all supported A platforms, each embedding Linux rcc-shim
# binaries for the B side. Output: dist/
# TARGETS="linux/amd64" limits the platforms (CI tests); default: all.
set -e
export GOTOOLCHAIN=local   # never auto-download a newer Go toolchain
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
LDFLAGS="-s -w -X main.version=$VERSION"
TARGETS=${TARGETS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w" \
    -o internal/embedded/shims/rcc-shim-linux-$arch ./cmd/rcc-shim
done
rm -rf dist
mkdir -p dist
for target in $TARGETS; do
  os=${target%/*}; arch=${target#*/}; ext=; [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
    -o "dist/rcc-$os-$arch$ext" ./cmd/rcc
done

# Binaries embed the Go runtime and BSD-licensed modules; ship their notices.
{
  echo "rcc $VERSION bundles the following third-party software."
  echo
  printf '==== Go standard library and runtime (%s)\n\n' "$(go env GOVERSION)"
  cat "$(go env GOROOT)/LICENSE"
  for t in linux/amd64 darwin/arm64 windows/amd64; do
    GOOS=${t%/*} GOARCH=${t#*/} go list -deps -f '{{if .Module}}{{.Module.Path}}{{end}}' ./cmd/rcc ./cmd/rcc-shim
  done | sort -u | grep -v '^remote-cc$' | while read -r m; do
    d=$(go list -m -f '{{.Dir}}' "$m")
    printf '\n==== %s %s\n\n' "$m" "$(go list -m -f '{{.Version}}' "$m")"
    cat "$d/LICENSE"
    [ -f "$d/PATENTS" ] && { printf '\n'; cat "$d/PATENTS"; }
  done
} > dist/THIRD_PARTY_LICENSES.txt
cp LICENSE dist/LICENSE.txt
(cd dist && sha256sum rcc-* > SHA256SUMS)
ls -lh dist
