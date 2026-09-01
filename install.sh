#!/bin/sh
# Installs juju-lens + juju-lens-probe from a GitHub Release.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lucabello/juju-lens/main/install.sh | sh
#
# Env overrides:
#   JUJU_LENS_VERSION      release tag to install, e.g. "v0.1.0" (default: latest)
#   JUJU_LENS_INSTALL_DIR  where to put the binaries (default: /usr/local/bin,
#                          falling back to ~/.local/bin if not writable)
#
# Only linux/amd64 and linux/arm64 are published - juju-lens-probe is an
# eBPF tool and only functions on Linux. See ./cmd/juju-lens-probe or
# `go build`/`just build` for other platforms (dev/TUI use only).

set -eu

repo="lucabello/juju-lens"
bin="juju-lens"

log() { printf '%s\n' "$*" >&2; }
die() {
    log "install.sh: $*"
    exit 1
}

need() {
    command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found on PATH"
}

need curl
need tar

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "${os}" in
linux) ;;
*) die "unsupported OS '${os}': only linux binaries are published (juju-lens-probe is eBPF/Linux-only); build from source with 'go install ${repo}/cmd/${bin}@latest' instead" ;;
esac

arch_raw="$(uname -m)"
case "${arch_raw}" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*) die "unsupported architecture '${arch_raw}': only amd64 and arm64 are published" ;;
esac

version="${JUJU_LENS_VERSION:-}"
if [ -z "${version}" ]; then
    log "resolving latest release..."
    version="$(curl -fsSL "https://api.github.com/repos/${repo}/releases/latest" |
        grep '"tag_name"' | head -1 | cut -d'"' -f4)"
    [ -n "${version}" ] || die "could not resolve the latest release tag"
fi
version_num="${version#v}"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

archive="${bin}-${version_num}-${os}-${arch}.tar.gz"
base_url="https://github.com/${repo}/releases/download/${version}"

log "downloading ${archive} (${version})..."
curl -fsSL -o "${workdir}/${archive}" "${base_url}/${archive}" ||
    die "failed to download ${base_url}/${archive} (does that release/target exist?)"
curl -fsSL -o "${workdir}/checksums.txt" "${base_url}/checksums.txt" ||
    die "failed to download checksums.txt for ${version}"

log "verifying checksum..."
(
    cd "${workdir}"
    if command -v sha256sum >/dev/null 2>&1; then
        grep " ${archive}\$" checksums.txt | sha256sum -c - >/dev/null
    elif command -v shasum >/dev/null 2>&1; then
        grep " ${archive}\$" checksums.txt | shasum -a 256 -c - >/dev/null
    else
        die "neither sha256sum nor shasum found; can't verify the download"
    fi
) || die "checksum verification failed for ${archive}"

tar xzf "${workdir}/${archive}" -C "${workdir}"

install_dir="${JUJU_LENS_INSTALL_DIR:-}"
if [ -z "${install_dir}" ]; then
    if [ -w /usr/local/bin ] || [ "$(id -u)" = "0" ]; then
        install_dir="/usr/local/bin"
    else
        install_dir="${HOME}/.local/bin"
    fi
fi
mkdir -p "${install_dir}"

for f in juju-lens juju-lens-probe; do
    install -m 0755 "${workdir}/${f}" "${install_dir}/${f}" 2>/dev/null ||
        cp "${workdir}/${f}" "${install_dir}/${f}"
    chmod 0755 "${install_dir}/${f}"
done

log "installed to ${install_dir}/juju-lens and ${install_dir}/juju-lens-probe"
case ":${PATH}:" in
*":${install_dir}:"*) ;;
*) log "note: ${install_dir} is not on your PATH" ;;
esac

"${install_dir}/juju-lens" version >&2 || true
