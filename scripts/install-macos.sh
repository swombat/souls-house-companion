#!/bin/sh
set -eu
test "$(uname -s)" = Darwin || { echo 'macOS only' >&2; exit 1; }
test "$(id -u)" -ne 0 || { echo 'Run as the logged-in owner, never root/sudo.' >&2; exit 1; }
test "${1:-}" = --accept-preview || {
  echo 'Read macos.md, then invoke install.sh --accept-preview.' >&2
  echo 'This is an ad-hoc-signed, non-notarised command-line preview.' >&2
  exit 1
}
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
source="$here/souls-house-companion"
test -f "$source" && test ! -L "$source" || exit 1
# Never remove quarantine flags or weaken Gatekeeper/TCC automatically.
"$source" version
dest="$HOME/.local/bin"
mkdir -p "$dest"
test ! -L "$HOME/.local" && test ! -L "$dest" || {
  echo 'Refusing symlink installation directories.' >&2; exit 1;
}
target="$dest/souls-house-companion"
if test -e "$target" || test -L "$target"; then
  echo 'Existing installation: pause it and retain the previous binary before updating.' >&2
  echo 'This first-install helper never overwrites a working installation.' >&2
  exit 1
fi
umask 077
stage=$(mktemp "$dest/.companion-install.XXXXXX")
trap 'rm -f "$stage"' EXIT HUP INT TERM
cp "$source" "$stage"
chmod 755 "$stage"
# Hard-link publication refuses a racing destination; no overwrite.
ln "$stage" "$target"
rm -f "$stage"
trap - EXIT HUP INT TERM
"$target" install --user
printf '%s\n' 'Binary and LaunchAgent configuration installed. Not enrolled or started.'
printf '%s\n' 'Follow macos.md for owner-approved pairing, local ceiling, execute opt-in and resume.'
printf '%s\n' 'Broad write grants can plant user persistence; execute is full-user authority, not a sandbox.'
