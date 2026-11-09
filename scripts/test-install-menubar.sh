#!/bin/sh
# Installer fixtures with stub launchctl/codesign in a throwaway HOME.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
stubs="$root/stubs"; mkdir -p "$stubs"
cat > "$stubs/launchctl" <<'EOF'
#!/bin/sh
if [ "$1" = bootstrap ]; then
  echo "bootstrap $(sed -n 's:.*<array><string>\(.*\)</string></array>.*:\1:p' "$3")" >> "$HOME/launchctl.log"
  [ -f "$HOME/fail-bootstrap" ] && exit 1
  if [ -f "$HOME/fail-bootstrap-once" ]; then rm "$HOME/fail-bootstrap-once"; exit 1; fi
fi
exit 0
EOF
cat > "$stubs/codesign" <<'EOF'
#!/bin/sh
[ -f "$2/Contents/broken-signature" ] && exit 1
exit 0
EOF
chmod 755 "$stubs"/*
export LAUNCHCTL="$stubs/launchctl" CODESIGN="$stubs/codesign"

mkapp() { mkdir -p "$1/Contents/MacOS"; touch "$1/Contents/Info.plist"; printf '#!/bin/sh\n# %s\n' "$2" > "$1/Contents/MacOS/SoulsHouseMenu"; chmod 755 "$1/Contents/MacOS/SoulsHouseMenu"; }
which_app() { sed -n 2p "$1/Contents/MacOS/SoulsHouseMenu" | cut -c3-; }
fail() { printf 'FAIL: %s\n' "$1"; exit 1; }

export HOME="$root/A&B <home>"; mkdir -p "$HOME"
inst="$HOME/Applications/SoulsHouseMenu.app"
mkapp "$root/v1.app" v1; mkapp "$root/v2.app" v2; mkapp "$root/v3.app" v3

sh "$here/install-menubar.sh" "$root/v1.app" >/dev/null
[ "$(which_app "$inst")" = v1 ] || fail "first install"
python3 -c "import plistlib,sys;p=plistlib.load(open(sys.argv[1],'rb'));assert p['ProgramArguments'][0].endswith('SoulsHouseMenu')" "$HOME/Library/LaunchAgents/house.souls.menu.plist" || fail "plist not valid XML for an & < > home"

sh "$here/install-menubar.sh" "$root/v2.app" >/dev/null
[ "$(which_app "$inst")" = v2 ] && [ "$(which_app "$inst.previous")" = v1 ] || fail "upgrade keeps previous"

if sh "$here/install-menubar.sh" "$inst.previous" 2>/dev/null; then fail "accepted .previous as a source"; fi
[ "$(which_app "$inst")" = v2 ] && [ "$(which_app "$inst.previous")" = v1 ] || fail ".previous source changed something"

if sh "$here/install-menubar.sh" "$root/missing.app" 2>/dev/null; then fail "accepted a missing source"; fi
[ "$(which_app "$inst")" = v2 ] || fail "missing source removed the installed copy"

touch "$root/v3.app/Contents/broken-signature"
if sh "$here/install-menubar.sh" "$root/v3.app" 2>/dev/null; then fail "accepted an unsigned source"; fi
rm "$root/v3.app/Contents/broken-signature"
[ "$(which_app "$inst")" = v2 ] || fail "bad signature changed the installed copy"

agent="$HOME/Library/LaunchAgents/house.souls.menu.plist"
printf '<!-- owner edit -->\n' >> "$agent"
before=$(cksum < "$agent")
: > "$HOME/launchctl.log"
touch "$HOME/fail-bootstrap-once"
if sh "$here/install-menubar.sh" "$root/v3.app" 2>"$root/err"; then fail "bootstrap failure reported success"; fi
[ "$(which_app "$inst")" = v2 ] && [ "$(which_app "$inst.previous")" = v1 ] || fail "bootstrap failure did not restore v2/v1"
[ "$(cksum < "$agent")" = "$before" ] || fail "old plist bytes not restored"
[ "$(grep -c bootstrap "$HOME/launchctl.log")" = 2 ] || fail "restored copy was not reloaded"
grep -q "restored" "$root/err" && ! grep -q "could NOT" "$root/err" || fail "recovery not reported"

touch "$HOME/fail-bootstrap"
if sh "$here/install-menubar.sh" "$root/v3.app" 2>"$root/err"; then fail "double failure reported success"; fi
grep -q "could NOT be loaded" "$root/err" || fail "failed recovery reload was hidden"
[ "$(which_app "$inst")" = v2 ] && [ "$(cksum < "$agent")" = "$before" ] || fail "double failure left a changed install"
rm "$HOME/fail-bootstrap"

# A failure publishing the plist (after the app moved) also restores.
mkdir -p "$agent.staging.blocker"
chmod 500 "$HOME/Library/LaunchAgents"
if sh "$here/install-menubar.sh" "$root/v3.app" 2>/dev/null; then chmod 700 "$HOME/Library/LaunchAgents"; fail "unwritable LaunchAgents reported success"; fi
chmod 700 "$HOME/Library/LaunchAgents"; rmdir "$agent.staging.blocker"
[ "$(which_app "$inst")" = v2 ] && [ "$(which_app "$inst.previous")" = v1 ] && [ "$(cksum < "$agent")" = "$before" ] || fail "plist staging failure changed the install"

# First install with no plist before: a failure leaves no plist behind.
export HOME="$root/fresh"; mkdir -p "$HOME"; touch "$HOME/fail-bootstrap"
if sh "$here/install-menubar.sh" "$root/v1.app" 2>/dev/null; then fail "fresh failure reported success"; fi
[ ! -e "$HOME/Library/LaunchAgents/house.souls.menu.plist" ] && [ ! -e "$HOME/Applications/SoulsHouseMenu.app" ] || fail "fresh failure left an install behind"
export HOME="$root/A&B <home>"

sh "$here/install-menubar.sh" --rollback >/dev/null
[ "$(which_app "$inst")" = v1 ] && [ "$(which_app "$inst.previous")" = v2 ] || fail "rollback swaps previous in"
printf 'installer fixtures: ok\n'
