#!/bin/sh
# Sign a companion binary with this Mac's stable local identity (#13).
#
# Ad-hoc signatures make the code hash the binary's identity, so every new
# build loses its Accessibility grant and the owner has to remove and re-add
# it. Signing every build with one local certificate makes the designated
# requirement `identifier "house.souls.companion" and certificate leaf = H"…"`,
# which stays the same across builds, so one grant covers later updates.
#
# The certificate is self-signed and lives in its own keychain under the
# owner's Application Support, created on first use. It is not Developer ID
# and not notarised.
#
# The key IS an authority: a binary signed with it keeps this Mac's
# Accessibility grant, which a replacement ad-hoc binary would lose. So the
# companion treats ~/Library/Application Support/souls-house as protected:
# residents cannot read, copy, change, move or trash it through the
# filesystem tool (by path or alias). Anything running as the owner outside
# the companion's tools (including resident `execute`, which is full user
# authority by design) can still reach it.
#
# Usage: scripts/sign-macos.sh <binary>   (signs in place; run as the owner)
set -eu
test "$(uname -s)" = Darwin || { echo 'macOS only' >&2; exit 1; }
test "$(id -u)" -ne 0 || { echo 'Run as the logged-in owner, never root/sudo.' >&2; exit 1; }
bin=${1:?usage: sign-macos.sh <binary>}
test -f "$bin" && test ! -L "$bin" || { echo "not a regular file: $bin" >&2; exit 1; }

name='souls.house companion (local)'
ident='house.souls.companion'
dir="$HOME/Library/Application Support/souls-house/signing"
kc="$dir/companion-signing.keychain-db"
pw="$dir/keychain-password"
SECURITY=${SECURITY:-security}
CODESIGN=${CODESIGN:-codesign}
OPENSSL=${OPENSSL:-openssl}

umask 077
mkdir -p "$dir"
if ! test -f "$kc"; then
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT HUP INT TERM
  "$OPENSSL" rand -hex 24 > "$pw"
  cat > "$work/req.cnf" <<CNF
[req]
distinguished_name=dn
x509_extensions=ext
prompt=no
[dn]
CN=$name
[ext]
keyUsage=critical,digitalSignature
extendedKeyUsage=critical,codeSigning
basicConstraints=critical,CA:false
CNF
  "$OPENSSL" req -x509 -newkey rsa:2048 -nodes -days 3650 \
    -keyout "$work/key.pem" -out "$work/cert.pem" -config "$work/req.cnf" 2>/dev/null
  p12pw=$("$OPENSSL" rand -hex 16)
  "$OPENSSL" pkcs12 -export -legacy -inkey "$work/key.pem" -in "$work/cert.pem" \
    -out "$work/id.p12" -passout "pass:$p12pw" 2>/dev/null ||
    "$OPENSSL" pkcs12 -export -inkey "$work/key.pem" -in "$work/cert.pem" \
      -out "$work/id.p12" -passout "pass:$p12pw"
  "$SECURITY" create-keychain -p "$(cat "$pw")" "$kc"
  "$SECURITY" set-keychain-settings "$kc"   # no auto-lock timeout
  "$SECURITY" unlock-keychain -p "$(cat "$pw")" "$kc"
  "$SECURITY" import "$work/id.p12" -k "$kc" -P "$p12pw" -T /usr/bin/codesign >/dev/null
  "$SECURITY" set-key-partition-list -S apple-tool:,apple: -s -k "$(cat "$pw")" "$kc" >/dev/null
  rm -rf "$work"
  trap - EXIT HUP INT TERM
fi
"$SECURITY" unlock-keychain -p "$(cat "$pw")" "$kc"
"$CODESIGN" --force --sign "$name" --keychain "$kc" --identifier "$ident" "$bin"
"$CODESIGN" -d -r- "$bin" 2>&1 | sed -n 's/^designated => /designated: /p'
