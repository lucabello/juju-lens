#!/bin/sh
# Installs juju-lens from a GitHub Release. It is one binary: it carries the
# probe it uploads to units, for linux/amd64 and linux/arm64.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lucabello/juju-lens/main/install.sh | sh
#
# Env overrides:
#   JUJU_LENS_VERSION      release tag to install, e.g. "v0.1.0" (default: latest)
#   JUJU_LENS_INSTALL_DIR  where to put the binary (default: /usr/local/bin,
#                          falling back to ~/.local/bin if not writable)
#
# Only linux/amd64 and linux/arm64 are published - the probe is an eBPF tool
# and only functions on Linux. See `just build` for other platforms (viewing,
# and recording with --attach unit, work there).

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
*) die "unsupported OS '${os}': only linux binaries are published; build from source with 'just build' instead (see the README)" ;;
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
    # Resolve via the /releases/latest redirect, not api.github.com: the API
    # allows only 60 unauthenticated requests/hour per IP, and CI runners share
    # IPs, so it intermittently returns 403.
    version="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
        "https://github.com/${repo}/releases/latest")" ||
        die "could not resolve the latest release tag"
    version="${version##*/}"
    [ -n "${version}" ] && [ "${version}" != "latest" ] || die "could not resolve the latest release tag"
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

install -m 0755 "${workdir}/${bin}" "${install_dir}/${bin}" 2>/dev/null ||
    cp "${workdir}/${bin}" "${install_dir}/${bin}"
chmod 0755 "${install_dir}/${bin}"

# Earlier releases shipped the probe as a second binary, which they need;
# later ones carry it inside juju-lens, so a copy left by an earlier install
# is removed.
if [ -e "${workdir}/juju-lens-probe" ]; then
    install -m 0755 "${workdir}/juju-lens-probe" "${install_dir}/juju-lens-probe" 2>/dev/null ||
        cp "${workdir}/juju-lens-probe" "${install_dir}/juju-lens-probe"
    chmod 0755 "${install_dir}/juju-lens-probe"
elif [ -e "${install_dir}/juju-lens-probe" ]; then
    rm -f "${install_dir}/juju-lens-probe" &&
        log "removed ${install_dir}/juju-lens-probe, which juju-lens now carries inside itself"
fi

log "installed to ${install_dir}/${bin}"
case ":${PATH}:" in
*":${install_dir}:"*) ;;
*) log "note: ${install_dir} is not on your PATH" ;;
esac

"${install_dir}/juju-lens" version >&2 || true
