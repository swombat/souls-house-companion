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

stage_plist() {
  mkdir -p "$HOME/Library/LaunchAgents"
  rm -rf "$plist.staging"
  cat > "$plist.staging" <<PLIST
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

# Everything new is staged; keep the exact old plist (or its absence).
stage_plist || { rm -rf "$stage" "$plist.staging"; die "could not stage the LaunchAgent"; }
had_plist=0
if [ -e "$plist" ]; then
  had_plist=1
  rm -rf "$plist.backup"
  cp -p "$plist" "$plist.backup" || { rm -rf "$stage" "$plist.staging"; die "could not back up the LaunchAgent"; }
fi
had_current=0
[ -d "$dest" ] && had_current=1
rotated_prev=0

# The one restoration path for any failure after the first replacement.
restore() {
  rm -rf "$dest.failed"
  [ -d "$dest" ] && mv "$dest" "$dest.failed"
  if [ "$had_current" = 1 ]; then
    mv "$prev" "$dest"
    [ "$rotated_prev" = 1 ] && mv "$prev.old" "$prev"
  fi
  if [ "$had_plist" = 1 ]; then
    cp -p "$plist.backup" "$plist" && rm -f "$plist.backup"
  else
    rm -f "$plist"
  fi
  rm -rf "$stage" "$plist.staging"
  if [ "$had_current" = 1 ] && [ "$had_plist" = 1 ]; then
    load || die "$1; previous copy and LaunchAgent restored but could NOT be loaded (run: launchctl bootstrap $domain $plist)"
  fi
  die "$1; previous copy and LaunchAgent restored"
}

if [ "$had_current" = 1 ]; then
  rm -rf "$prev.old"
  if [ -d "$prev" ]; then mv "$prev" "$prev.old" || die "could not set aside the old .previous"; rotated_prev=1; fi
  mv "$dest" "$prev" || { [ "$rotated_prev" = 1 ] && mv "$prev.old" "$prev"; die "could not keep the current copy as .previous"; }
fi
mv "$stage" "$dest" || restore "could not move the new copy into place"
mv "$plist.staging" "$plist" || restore "could not publish the LaunchAgent"
load || restore "could not load the new copy (kept as $dest.failed)"
rm -rf "$prev.old" "$plist.backup"
printf 'Installed %s; LaunchAgent %s loaded\n' "$dest" "$label"
