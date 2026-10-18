#!/bin/sh
set -eu
# Run from repository root in an allowlisted environment. No Apple toolchain.
out=${1:-dist}
version=$(git rev-parse --short=12 HEAD)
if test -n "$(git status --porcelain)"; then version="$version-dirty"; fi
mkdir -p "$out"
out=$(cd "$out" && pwd)
for arch in arm64 amd64; do
  stage=$(mktemp -d)
  trap 'rm -rf "$stage"' EXIT HUP INT TERM
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
    -ldflags="-s -w -X main.version=$version" -o "$stage/souls-house-companion" ./cmd/companion
  cp scripts/install-macos.sh "$stage/install.sh"
  cp docs/macos.md "$stage/macos.md"
  chmod 755 "$stage/install.sh"
  cp "$stage/souls-house-companion" "$out/companion-darwin-$arch"
  tar -czf "$out/companion-$version-darwin-$arch.tar.gz" -C "$stage" .
  rm -rf "$stage"
  trap - EXIT HUP INT TERM
done
go run ./cmd/artifact-check "$out/companion-darwin-arm64" "$out/companion-darwin-amd64"
(cd "$out"; sha256sum ./*.tar.gz > SHA256SUMS)
printf 'Candidate artifacts: %s (review/runtime acceptance still required)\n' "$out"
