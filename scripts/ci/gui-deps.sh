#!/usr/bin/env bash
# Installs the Linux build dependencies of the Fyne GUI client.
#
# Used by the CI workflows before any command that type-checks ./... —
# the GUI package needs cgo, and go-gl (GL, GLFW 3.4 with its vendored
# Wayland and X11 backends) requires these development packages on
# headless Debian/Ubuntu runners.
#
# Package map:
#   libgl1-mesa-dev     OpenGL headers (pkg-config: gl)
#   libxcursor-dev, libxrandr-dev, libxinerama-dev, libxi-dev,
#   libxxf86vm-dev      X11 headers for GLFW's X11 backend
#   libwayland-dev      Wayland headers (wayland-client-core.h et al.)
#   libxkbcommon-dev    keymap handling shared by both backends
#   libdecor-0-dev      Wayland window decorations (GLFW 3.4)
set -euo pipefail

sudo apt-get update
sudo apt-get install -y \
  libgl1-mesa-dev libxcursor-dev libxrandr-dev libxinerama-dev \
  libxi-dev libxxf86vm-dev libxkbcommon-dev libwayland-dev libdecor-0-dev
