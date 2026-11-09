#!/bin/sh
# Installer fixtures with stub launchctl/codesign in a throwaway HOME.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
stubs="$root/stubs"; mkdir -p "$stubs"
cat > "$stubs/launchctl" <<'EOF'
#!/bin/sh
[ "$1" = bootstrap ] && [ -f "$HOME/fail-bootstrap" ] && exit 1
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

touch "$HOME/fail-bootstrap"
if sh "$here/install-menubar.sh" "$root/v3.app" 2>/dev/null; then fail "bootstrap failure reported success"; fi
[ "$(which_app "$inst")" = v2 ] && [ "$(which_app "$inst.previous")" = v1 ] || fail "bootstrap failure did not restore v2/v1"
rm "$HOME/fail-bootstrap"

sh "$here/install-menubar.sh" --rollback >/dev/null
[ "$(which_app "$inst")" = v1 ] && [ "$(which_app "$inst.previous")" = v2 ] || fail "rollback swaps previous in"
printf 'installer fixtures: ok\n'
