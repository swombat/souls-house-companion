#!/bin/sh
# Build the menu bar app on a Mac (Swift 5.9+), as the owner. Output:
# dist/SoulsHouseMenu.app, ad-hoc signed like the companion.
set -eu
out=${1:-dist}
mkdir -p "$out"
out=$(cd "$out" && pwd)
(cd menubar && swift build -c release)
app="$out/SoulsHouseMenu.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS"
cp "menubar/.build/release/SoulsHouseMenu" "$app/Contents/MacOS/SoulsHouseMenu"
cat > "$app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>house.souls.menu</string>
  <key>CFBundleName</key><string>souls.house</string>
  <key>CFBundleExecutable</key><string>SoulsHouseMenu</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1</string>
  <key>LSUIElement</key><true/>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
</dict></plist>
PLIST
codesign --force --sign - "$app"
printf 'Built %s (ad-hoc signed; not Developer ID)\n' "$app"
