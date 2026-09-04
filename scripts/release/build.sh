#!/usr/bin/env bash
# Release artifact builder for selftunnel.
#
# Usage: scripts/release/build.sh <goos> <goarch> [--no-gui]
#
# Produces in dist/:
#   selftunnel-server-<goos>-<goarch>[.exe]        (CGO-free)
#   selftunnel-client-<goos>-<goarch>[.exe]        (CGO-free)
#   selftunnel-client-gui-<goos>-<goarch>[.exe]    (cgo; skipped with --no-gui)
#
# GUI platform rules (spec §6.4 / README):
#   - linux:   built natively — the runner's arch must match goarch
#              (glibc + X11/Wayland make cross-compilation unreliable)
#   - darwin:  built on macOS — arm64 natively, amd64 cross-compiled
#   - windows: cross-compiled from Linux via mingw-w64 (CC set below)
set -euo pipefail
cd "$(dirname "$0")/../.."

GOOS_TARGET="${1:?usage: build.sh <goos> <goarch> [--no-gui]}"
GOARCH_TARGET="${2:?usage: build.sh <goos> <goarch> [--no-gui]}"
BUILD_GUI="${3:-}"

SUFFIX=""
if [ "$GOOS_TARGET" = "windows" ]; then
  SUFFIX=".exe"
fi

LDFLAGS='-w -s'
GUI_LDFLAGS="$LDFLAGS"
if [ "$GOOS_TARGET" = "windows" ]; then
  GUI_LDFLAGS='-w -s -H=windowsgui'
fi

mkdir -p dist

echo "==> selftunnel-server ${GOOS_TARGET}/${GOARCH_TARGET}"
CGO_ENABLED=0 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
  go build -ldflags="$LDFLAGS" \
  -o "dist/selftunnel-server-${GOOS_TARGET}-${GOARCH_TARGET}${SUFFIX}" \
  ./cmd/selftunnel-server

echo "==> selftunnel-client ${GOOS_TARGET}/${GOARCH_TARGET}"
CGO_ENABLED=0 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
  go build -ldflags="$LDFLAGS" \
  -o "dist/selftunnel-client-${GOOS_TARGET}-${GOARCH_TARGET}${SUFFIX}" \
  ./cmd/selftunnel-client

if [ "$BUILD_GUI" = "--no-gui" ]; then
  echo "==> selftunnel-client-gui skipped (--no-gui)"
  ls -l dist/
  exit 0
fi

# The GUI client needs cgo; assemble the toolchain per platform.
case "${GOOS_TARGET}/${GOARCH_TARGET}" in
  linux/*)
    if [ "$(go env GOOS)" != "linux" ] || [ "$(go env GOARCH)" != "$GOARCH_TARGET" ]; then
      echo "release: the linux GUI must be built natively on a ${GOARCH_TARGET} runner" >&2
      exit 1
    fi
    ;;
  darwin/*)
    if [ "$(go env GOOS)" != "darwin" ]; then
      echo "release: the darwin GUI must be built on macOS" >&2
      exit 1
    fi
    ;;
  windows/amd64)
    export CC=x86_64-w64-mingw32-gcc
    if ! command -v "$CC" >/dev/null; then
      echo "release: $CC not found (on Debian/Ubuntu: apt install gcc-mingw-w64-x86-64)" >&2
      exit 1
    fi
    ;;
  *)
    echo "release: unsupported GUI target ${GOOS_TARGET}/${GOARCH_TARGET}" >&2
    exit 1
    ;;
esac

echo "==> selftunnel-client-gui ${GOOS_TARGET}/${GOARCH_TARGET}"
CGO_ENABLED=1 GOOS="$GOOS_TARGET" GOARCH="$GOARCH_TARGET" \
  go build -ldflags="$GUI_LDFLAGS" \
  -o "dist/selftunnel-client-gui-${GOOS_TARGET}-${GOARCH_TARGET}${SUFFIX}" \
  ./cmd/selftunnel-client-gui

ls -l dist/
