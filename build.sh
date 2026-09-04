#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "==> Building selftunnel-server (native)..."
go build -o selftunnel-server ./cmd/selftunnel-server
go vet ./...

echo "==> Building selftunnel-server (linux/amd64 for Docker)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags='-w -s' -o selftunnel-server-linux ./cmd/selftunnel-server

echo "==> Building selftunnel-client (cross-platform portable)..."
mkdir -p dist
build_client() {
  local os=$1 arch=$2 suffix=$3
  local out="dist/selftunnel-client-${os}-${arch}${suffix}"
  echo "    ${out}"
  GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 go build -ldflags='-w -s' -o "${out}" ./cmd/selftunnel-client
}
build_client windows amd64 .exe
build_client darwin amd64 ""
build_client darwin arm64 ""
build_client linux amd64 ""
build_client linux arm64 ""

echo "==> Building selftunnel-client-gui (native GUI)..."
GUI_SUFFIX=""
GUI_LDFLAGS='-w -s'
if [ "$(go env GOOS)" = "windows" ]; then
  GUI_SUFFIX=".exe"
  GUI_LDFLAGS='-w -s -H=windowsgui'
fi
go build -ldflags="${GUI_LDFLAGS}" -o "dist/selftunnel-client-gui-$(go env GOOS)-$(go env GOARCH)${GUI_SUFFIX}" ./cmd/selftunnel-client-gui

echo "==> Building selftunnel-client-gui (windows/amd64 GUI)..."
if command -v x86_64-w64-mingw32-gcc >/dev/null 2>&1; then
  CC=x86_64-w64-mingw32-gcc GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -ldflags='-w -s -H=windowsgui' -o dist/selftunnel-client-gui-windows-amd64.exe ./cmd/selftunnel-client-gui
else
  echo "    skipping (x86_64-w64-mingw32-gcc not found; install mingw-w64 to cross-compile Windows GUI)"
fi

echo "==> Building selftunnel-client-gui (darwin/amd64 GUI)..."
if [ "$(go env GOOS)" = "darwin" ]; then
  CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -ldflags='-w -s' -o dist/selftunnel-client-gui-darwin-amd64 ./cmd/selftunnel-client-gui
else
  echo "    skipping (cross-compiling darwin/amd64 GUI requires building on macOS)"
fi

echo "==> Done."
echo "    selftunnel-server"
echo "    selftunnel-server-linux"
echo "    dist/selftunnel-client-*"
echo "    dist/selftunnel-client-gui-*"
