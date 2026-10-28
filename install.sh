#!/bin/sh
# Installs ccgp and ccgpy from the GitHub release for this OS and CPU (Linux, macOS).
#
#   curl -fsSL https://raw.githubusercontent.com/PHPCraftdream/cc-go-proxy/main/install.sh | sh
#
# Environment:
#   CCGP_VERSION      version to install, e.g. 0.1.0 (default: the latest release)
#   CCGP_INSTALL_DIR  where to put the binaries (default: ~/.local/bin)
#
# The archive is checked against the release's SHA256SUMS before anything is
# installed. For signed build provenance see docs/releasing.md.
set -eu

REPO="PHPCraftdream/cc-go-proxy"
BASE="https://github.com/${REPO}/releases"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

fetch() { # fetch URL FILE
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 3 -o "$2" "$1" || die "download failed: $1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1" || die "download failed: $1"
  else
    die "need curl or wget"
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d ' ' -f 1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d ' ' -f 1
  else
    die "need sha256sum or shasum to verify the download"
  fi
}

latest_version() {
  # /releases/latest redirects to /releases/tag/vX.Y.Z; no API rate limit applies.
  if command -v curl >/dev/null 2>&1; then
    url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${BASE}/latest") || die "cannot look up the latest release"
  else
    url=$(wget -q --max-redirect=0 -S -O /dev/null "${BASE}/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tr -d '\r' | head -n 1) || true
  fi
  tag=${url##*/}
  case "$tag" in
    v[0-9]*) printf '%s' "${tag#v}" ;;
    *) die "cannot find the latest release (got '${url}')" ;;
  esac
}

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  FreeBSD) os=freebsd ;;
  *) die "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  armv6* | armv7* | armhf) arch=arm ;; # one 32-bit ARM build, made for armv6 and runs on armv7
  riscv64) arch=riscv64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac
case "${os}/${arch}" in
  darwin/arm | darwin/riscv64 | freebsd/arm | freebsd/riscv64) die "no build for ${os}/${arch}" ;;
esac

version=${CCGP_VERSION:-}
version=${version#v}
[ -n "$version" ] || version=$(latest_version)
dir=${CCGP_INSTALL_DIR:-$HOME/.local/bin}

name="ccgp_${version}_${os}_${arch}"
archive="${name}.tar.gz"

tmp=$(mktemp -d) || die "cannot create a temporary directory"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Installing ccgp ${version} (${os}/${arch}) into ${dir}"
fetch "${BASE}/download/v${version}/${archive}" "${tmp}/${archive}"
fetch "${BASE}/download/v${version}/SHA256SUMS" "${tmp}/SHA256SUMS"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "${tmp}/SHA256SUMS")
[ -n "$want" ] || die "${archive} is not listed in SHA256SUMS"
got=$(sha256_of "${tmp}/${archive}")
[ "$got" = "$want" ] || die "checksum mismatch for ${archive} (expected ${want}, got ${got})"

tar -xzf "${tmp}/${archive}" -C "$tmp" || die "cannot unpack ${archive}"
for exe in ccgp ccgpy; do
  [ -f "${tmp}/${name}/${exe}" ] || die "${exe} is missing from the archive"
done

mkdir -p "$dir" || die "cannot create ${dir}"
for exe in ccgp ccgpy; do
  # Write beside the target and rename: replaces a binary that is running.
  cp "${tmp}/${name}/${exe}" "${dir}/.${exe}.new" || die "cannot write to ${dir}"
  chmod 755 "${dir}/.${exe}.new"
  mv -f "${dir}/.${exe}.new" "${dir}/${exe}"
done

say "Installed: ${dir}/ccgp, ${dir}/ccgpy"
case ":${PATH}:" in
  *":${dir}:"*) ;;
  *) say "Note: ${dir} is not on your PATH. Add it, for example:"
     say "  echo 'export PATH=\"${dir}:\$PATH\"' >> ~/.profile" ;;
esac
say "Next: ccgp --help   (you also need Claude Code: 'claude' on PATH)"
