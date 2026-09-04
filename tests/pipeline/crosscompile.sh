#!/usr/bin/env bash
# Cross-compilation gate for selftunnel.
#
# The CGO-free server and CLI client must build for every supported
# deployment platform (spec §6.4). This is also the only automated check
# that compiles the platform-specific sleep-prevention files
# (power_windows.go, power_linux.go) on their native GOOS. The GUI client
# needs cgo and is not cross-compilable here; it is built natively by
# build.sh and CI.
set -euo pipefail
cd "$(dirname "$0")/../.."

for target in windows/amd64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  echo "crosscompile: ${os}/${arch}"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build ./cmd/selftunnel-server ./cmd/selftunnel-client
done
echo "crosscompile: all platforms OK"
