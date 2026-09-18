#!/usr/bin/env bash
# Package a darwin GUI binary as a double-clickable .app bundle plus a
# drag-install DMG image.
#
# Usage: scripts/release/package-darwin.sh <binary> <appname> <bundleid>
#
# Produces in dist/:
#   <appname>.app                      (no Terminal window on double-click)
#   <appname>-darwin-<goarch>.dmg      (compressed image with an
#                                       Applications symlink inside)
#
# hdiutil, codesign and the rest of the toolchain ship with macOS, so this
# runs identically on a developer Mac and on GitHub Actions macos runners.
set -euo pipefail

BIN="${1:?usage: package-darwin.sh <binary> <appname> <bundleid>}"
APPNAME="${2:?usage: package-darwin.sh <binary> <appname> <bundleid>}"
BUNDLEID="${3:?usage: package-darwin.sh <binary> <appname> <bundleid>}"

if [ "$(go env GOOS)" != "darwin" ]; then
  echo "package-darwin: must run on macOS" >&2
  exit 1
fi

APP="dist/${APPNAME}.app"
ICON="assets/icon.icns"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN" "$APP/Contents/MacOS/${APPNAME}"
ICON_PLIST=""
if [ -f "$ICON" ]; then
  cp "$ICON" "$APP/Contents/Resources/icon.icns"
  ICON_PLIST="  <key>CFBundleIconFile</key><string>icon</string>"
fi
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key><string>${APPNAME}</string>
  <key>CFBundleIdentifier</key><string>${BUNDLEID}</string>
  <key>CFBundleName</key><string>${APPNAME}</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>CFBundleVersion</key><string>0.1.0</string>
  <key>NSHighResolutionCapable</key><true/>
${ICON_PLIST}
</dict>
</plist>
PLIST

# Ad-hoc sign so Gatekeeper treats the bundle consistently on this machine.
codesign --force --sign - "$APP" >/dev/null 2>&1 || true
echo "    ${APP} (double-clickable, no Terminal window)"

STAGING="$(mktemp -d)"
trap 'rm -rf "$STAGING"' EXIT
cp -R "$APP" "$STAGING/"
ln -s /Applications "$STAGING/Applications"
DMG="dist/${APPNAME}-darwin-$(go env GOARCH).dmg"
rm -f "$DMG"
hdiutil create -volname "$APPNAME" -srcfolder "$STAGING" -ov -format UDZO "$DMG" >/dev/null
echo "    ${DMG}"
