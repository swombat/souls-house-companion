#!/bin/sh
# Install the menu bar app for this user and start it at login, or roll back.
#
#   install-menubar.sh path/to/SoulsHouseMenu.app   install (keeps the current
#                                                    copy as .previous)
#   install-menubar.sh --rollback                   swap .previous back in
#
# Nothing installed is touched until the new copy is staged and verified. If
# loading it fails, the previous copy is put back and loaded. Never needs sudo.
set -eu
LAUNCHCTL=${LAUNCHCTL:-launchctl}
CODESIGN=${CODESIGN:-codesign}
label=house.souls.menu
apps="$HOME/Applications"
dest="$apps/SoulsHouseMenu.app"
prev="$dest.previous"
stage="$dest.staging"
plist="$HOME/Library/LaunchAgents/$label.plist"
domain="gui/$(id -u)"

die() { printf 'install-menubar: %s\n' "$1" >&2; exit 1; }
xml() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\&apos;/g"; }
real() { (cd "$1" 2>/dev/null && pwd -P); }

valid_app() {
  [ -d "$1" ] && [ ! -L "$1" ] && [ -f "$1/Contents/Info.plist" ] && [ -x "$1/Contents/MacOS/SoulsHouseMenu" ] &&
    "$CODESIGN" --verify "$1" >/dev/null 2>&1
}

write_plist() {
  mkdir -p "$HOME/Library/LaunchAgents"
  tmp="$plist.tmp"
  cat > "$tmp" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$(xml "$dest/Contents/MacOS/SoulsHouseMenu")</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>ProcessType</key><string>Interactive</string>
</dict></plist>
PLIST
  mv "$tmp" "$plist"
}

load() { "$LAUNCHCTL" bootout "$domain/$label" >/dev/null 2>&1 || true; "$LAUNCHCTL" bootstrap "$domain" "$plist"; }

if [ "${1:-}" = "--rollback" ]; then
  valid_app "$prev" || die "no valid previous copy to roll back to"
  rm -rf "$stage"
  cp -R "$prev" "$stage"
  set -- "$stage"
  rollback=1
else
  rollback=0
fi

src=${1:?usage: install-menubar.sh path/to/SoulsHouseMenu.app | --rollback}
valid_app "$src" || die "source is not a valid signed SoulsHouseMenu.app"
mkdir -p "$apps"
if [ "$rollback" = 0 ]; then
  for p in "$dest" "$prev" "$stage"; do
    if [ -e "$p" ] && [ "$(real "$src")" = "$(real "$p")" ]; then
      die "source is an installed copy; use --rollback to return to .previous"
    fi
  done
  rm -rf "$stage"
  cp -R "$src" "$stage"
fi
valid_app "$stage" || { rm -rf "$stage"; die "staged copy failed verification"; }

# Rotate only now: current -> .previous, staged -> current.
had_current=0
if [ -d "$dest" ]; then
  had_current=1
  rm -rf "$prev.old"
  [ -d "$prev" ] && mv "$prev" "$prev.old"
  mv "$dest" "$prev"
fi
mv "$stage" "$dest"
write_plist
if load; then
  rm -rf "$prev.old"
  printf 'Installed %s; LaunchAgent %s loaded\n' "$dest" "$label"
  exit 0
fi

# Loading failed: put the previous copy back and load it.
rm -rf "$dest.failed"
mv "$dest" "$dest.failed"
if [ "$had_current" = 1 ]; then
  mv "$prev" "$dest"
  [ -d "$prev.old" ] && mv "$prev.old" "$prev"
  load || true
fi
die "could not load the new copy (kept as $dest.failed); previous copy restored"
