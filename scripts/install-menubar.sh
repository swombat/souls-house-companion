#!/bin/sh
# Install the menu bar app for this user and start it at login. Keeps the
# previous app as SoulsHouseMenu.app.previous. Never needs sudo.
set -eu
src=${1:?path to SoulsHouseMenu.app}
dest="$HOME/Applications/SoulsHouseMenu.app"
plist="$HOME/Library/LaunchAgents/house.souls.menu.plist"
mkdir -p "$HOME/Applications" "$HOME/Library/LaunchAgents"
launchctl bootout "gui/$(id -u)/house.souls.menu" 2>/dev/null || true
if [ -d "$dest" ]; then rm -rf "$dest.previous"; mv "$dest" "$dest.previous"; fi
cp -R "$src" "$dest"
cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>house.souls.menu</string>
  <key>ProgramArguments</key><array><string>$dest/Contents/MacOS/SoulsHouseMenu</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>ProcessType</key><string>Interactive</string>
</dict></plist>
PLIST
launchctl bootstrap "gui/$(id -u)" "$plist"
printf 'Installed %s; LaunchAgent house.souls.menu loaded\n' "$dest"
