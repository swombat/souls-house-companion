#!/bin/sh
# Build the menu bar app on a Mac (Swift 5.9+), as the owner. Output:
# <out>/souls.house.app, ad-hoc signed like the companion.
#
#   build-menubar.sh [out] [companion-binary] [version]
#
# With a companion binary the app carries it (Contents/MacOS/souls-house-companion)
# and installs or updates it on launch: that is the release build. Without one
# the app only drives a companion that is already installed.
set -eu
out=${1:-dist}
companion=${2:-}
version=${3:-$(git rev-parse --short=12 HEAD)}
printf '%s' "$version" | grep -Eq '^[A-Za-z0-9._-]+$' || { echo "unsafe version: $version" >&2; exit 1; }
mkdir -p "$out"
out=$(cd "$out" && pwd)
(cd menubar && swift build -c release)
app="$out/souls.house.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS"
cp "menubar/.build/release/SoulsHouseMenu" "$app/Contents/MacOS/SoulsHouseMenu"
if [ -n "$companion" ]; then
  cp "$companion" "$app/Contents/MacOS/souls-house-companion"
  chmod 755 "$app/Contents/MacOS/souls-house-companion"
fi
# CFBundleVersion changes with every build: a running copy that sees a
# different one on disk knows it was replaced and reopens the new one.
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>house.souls.menu</string>
  <key>CFBundleName</key><string>souls.house</string>
  <key>CFBundleDisplayName</key><string>souls.house</string>
  <key>CFBundleExecutable</key><string>SoulsHouseMenu</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>$version</string>
  <key>CFBundleVersion</key><string>$version</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>LSUIElement</key><true/>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
</dict></plist>
PLIST
if [ -f menubar/AppIcon.icns ]; then
  mkdir -p "$app/Contents/Resources"
  cp menubar/AppIcon.icns "$app/Contents/Resources/AppIcon.icns"
fi
# Inner code first, then the app; never --deep for signing.
if [ -n "$companion" ]; then
  codesign --force --sign - --identifier house.souls.companion "$app/Contents/MacOS/souls-house-companion"
fi
codesign --force --sign - "$app"
printf 'Built %s (ad-hoc signed; not Developer ID)\n' "$app"
