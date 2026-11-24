#!/bin/sh
set -eu

# Installer and upgrader for souls.house Companion on Linux.
# Downloads SHA256SUMS and the platform tarball to a temporary directory,
# verifies the SHA-256 checksum before extracting, and invokes `upgrade-from`
# on the verified staged binary so both fresh installs and upgrades from legacy
# builds (such as commit-stamped builds that lack `upgrade-from`) share one
# upgrade engine.

RELEASE_VERSION=""
REPO="swombat/souls-house-companion"

usage() {
  cat <<'EOF'
Usage: sh install-linux.sh [flags]

Flags:
  --version VER             Release version to install (default: version stamped into this script)
  --install-dir PATH        Install directory for binaries (default: ~/.local/bin)
  --state-dir PATH          Private state directory (default: ~/.local/share/souls-house-companion)
  --force                   Allow reinstalling the same version or downgrading
  --no-setup                Do not launch guided setup after a fresh install
  -h, --help                Show this help
EOF
}

version="$RELEASE_VERSION"
install_dir="${HOME:?HOME must be set}/.local/bin"
install_dir_explicit=0
state_dir="${HOME}/.local/share/souls-house-companion"
state_dir_explicit=0
release_base_url=""
force=0
no_setup=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || { echo "install-linux.sh: --version requires a value" >&2; exit 2; }
      version="$2"
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || { echo "install-linux.sh: --install-dir requires a value" >&2; exit 2; }
      install_dir="$2"
      install_dir_explicit=1
      shift 2
      ;;
    --state-dir)
      [ "$#" -ge 2 ] || { echo "install-linux.sh: --state-dir requires a value" >&2; exit 2; }
      state_dir="$2"
      state_dir_explicit=1
      shift 2
      ;;
    --release-base-url)
      [ "$#" -ge 2 ] || { echo "install-linux.sh: --release-base-url requires a value" >&2; exit 2; }
      release_base_url="$2"
      shift 2
      ;;
    --force)
      force=1
      shift
      ;;
    --no-setup)
      no_setup=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "install-linux.sh: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

os=$(uname -s)
if [ "$os" != "Linux" ]; then
  echo "install-linux.sh: Linux required (detected $os)" >&2
  exit 1
fi

machine=$(uname -m)
case "$machine" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *)
    echo "install-linux.sh: unsupported Linux architecture: $machine (expected amd64 or arm64)" >&2
    exit 1
    ;;
esac

download_file() {
  url="$1"
  out="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --proto '=https' --tlsv1.2 -o "$out" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
  else
    echo "install-linux.sh: curl or wget is required" >&2
    exit 1
  fi
}

if [ -z "$version" ]; then
  echo "install-linux.sh: --version is required when running an unstamped script" >&2
  exit 1
fi

if ! printf '%s' "$version" | grep -Eq '^[A-Za-z0-9._-]+$'; then
  echo "install-linux.sh: unsafe version string: $version" >&2
  exit 1
fi

allow_loopback_http=0
if [ -z "$release_base_url" ]; then
  release_base_url="https://github.com/${REPO}/releases/download/${version}"
else
  case "$release_base_url" in
    https://*) ;;
    http://127.0.0.1:*|http://127.0.0.1/*|http://localhost:*|http://localhost/*)
      allow_loopback_http=1
      ;;
    *)
      echo "install-linux.sh: --release-base-url must use https:// (or loopback http:// for local tests)" >&2
      exit 1
      ;;
  esac
fi
release_base_url="${release_base_url%/}"

asset_dir="souls-house-companion-${version}-linux-${arch}"
asset_file="${asset_dir}.tar.gz"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

sums_path="${tmpdir}/SHA256SUMS"
tar_path="${tmpdir}/${asset_file}"

echo "Downloading souls.house Companion ${version} (${arch})..."
if [ "$allow_loopback_http" -eq 1 ]; then
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$sums_path" "${release_base_url}/SHA256SUMS"
    curl -fsSL -o "$tar_path" "${release_base_url}/${asset_file}"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$sums_path" "${release_base_url}/SHA256SUMS"
    wget -qO "$tar_path" "${release_base_url}/${asset_file}"
  else
    echo "install-linux.sh: curl or wget is required" >&2
    exit 1
  fi
else
  download_file "${release_base_url}/SHA256SUMS" "$sums_path"
  download_file "${release_base_url}/${asset_file}" "$tar_path"
fi

# Verify SHA-256 before extracting or touching any existing installation.
want_sha=$(awk -v f="$asset_file" '
  /^[[:space:]]*#/ || NF < 2 { next }
  {
    name = $2
    sub(/^\*/, "", name)
    sub(/^\.\//, "", name)
    if (name == f) { print tolower($1); exit }
  }
' "$sums_path")

if [ -z "$want_sha" ] || ! printf '%s' "$want_sha" | grep -Eq '^[0-9a-f]{64}$'; then
  echo "install-linux.sh: SHA256SUMS does not contain a valid digest for ${asset_file}" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  got_sha=$(sha256sum "$tar_path" | awk '{print tolower($1)}')
elif command -v shasum >/dev/null 2>&1; then
  got_sha=$(shasum -a 256 "$tar_path" | awk '{print tolower($1)}')
else
  echo "install-linux.sh: sha256sum or shasum is required to verify download" >&2
  exit 1
fi

if [ "$got_sha" != "$want_sha" ]; then
  echo "install-linux.sh: SHA-256 checksum mismatch for ${asset_file} (aborting without modifying installation)" >&2
  exit 1
fi

extract_dir="${tmpdir}/extract"
mkdir -p "$extract_dir"
tar -xzf "$tar_path" -C "$extract_dir"

staged_dir="${extract_dir}/${asset_dir}"
staged_bin="${staged_dir}/souls-house-companion"
if [ -L "$staged_bin" ] || [ ! -f "$staged_bin" ] || [ ! -x "$staged_bin" ]; then
  echo "install-linux.sh: verified archive is missing executable souls-house-companion" >&2
  exit 1
fi

# Invoke upgrade-from on the verified staged binary (not any pre-existing binary,
# which may be a legacy commit-stamped build that does not know upgrade-from).
set -- upgrade-from
if [ "$state_dir_explicit" -eq 1 ]; then
  set -- "$@" --state-dir "$state_dir"
fi
if [ "$install_dir_explicit" -eq 1 ]; then
  set -- "$@" --install-dir "$install_dir"
fi
if [ "$force" -eq 1 ]; then
  set -- "$@" --force
fi
set -- "$@" "$staged_dir"
"$staged_bin" "$@"

unit_file="${HOME}/.config/systemd/user/souls-house-companion.service"
if [ "$no_setup" -eq 0 ] && [ ! -f "${state_dir}/state.json" ] && [ ! -e "$unit_file" ]; then
  if [ -t 0 ]; then
    "${install_dir}/souls-house-companion" setup --state-dir "$state_dir"
  else
    echo "Installed to ${install_dir}/souls-house-companion."
    echo "Run '${install_dir}/souls-house-companion setup' to pair this computer."
  fi
fi
