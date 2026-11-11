#!/bin/sh
set -eu
# Builds the Linux command-line companion for amd64 and arm64 and packages each
# as a tarball. Run from the repository root. Usage:
#   sh scripts/build-linux.sh [out-dir (default dist/linux)] [version]
# The version defaults to the short commit hash. Static (CGO_ENABLED=0), so no
# system libraries are assumed; it still needs a systemd user manager to run
# as a service (see docs/linux.md).
out=${1:-dist/linux}
version=${2:-$(git rev-parse --short=12 HEAD)}
printf '%s' "$version" | grep -Eq '^[A-Za-z0-9._-]+$' || { echo "unsafe version: $version" >&2; exit 1; }
mkdir -p "$out"
out=$(cd "$out" && pwd)

# ELF e_machine (2 bytes, little-endian, offset 18): 62 = x86-64, 183 = AArch64.
elf_machine() {
  head -c 4 "$1" | od -An -c | tr -d ' \n' | grep -q '^177ELF$' || { echo "not ELF: $1" >&2; return 1; }
  od -An -tu2 -j18 -N2 "$1" | tr -d ' \n'
}

for arch in amd64 arm64; do
  case $arch in amd64) want=62 ;; arm64) want=183 ;; esac
  name="souls-house-companion-$version-linux-$arch"
  stage=$(mktemp -d)
  trap 'rm -rf "$stage"' EXIT HUP INT TERM
  mkdir "$stage/$name"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -ldflags="-s -w -X main.version=$version" -o "$stage/$name/souls-house-companion" ./cmd/companion
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
    -ldflags="-s -w" -o "$stage/$name/souls-house-tray" ./cmd/tray
  for bin in souls-house-companion souls-house-tray; do
    got=$(elf_machine "$stage/$name/$bin")
    [ "$got" = "$want" ] || { echo "linux/$arch $bin: ELF machine $got, want $want" >&2; exit 1; }
  done
  cp docs/linux.md "$stage/$name/README.md"
  cp LICENSE "$stage/$name/LICENSE"
  chmod 755 "$stage/$name/souls-house-companion" "$stage/$name/souls-house-tray"
  chmod 644 "$stage/$name/README.md" "$stage/$name/LICENSE"
  tar -czf "$out/$name.tar.gz" --owner=0 --group=0 --numeric-owner -C "$stage" "$name"
  rm -rf "$stage"
  trap - EXIT HUP INT TERM
  echo "linux/$arch: ELF machine $got ok"
done
(cd "$out" && sha256sum ./*-linux-*.tar.gz | sed 's# \./# #' > SHA256SUMS)
cat "$out/SHA256SUMS"
