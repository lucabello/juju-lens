set shell := ["bash", "-c"]

BIN := "juju-lens"
PKG := "./..."
# Version comes from git tags (vX.Y.Z, pushed to release) - no VERSION file.
# `--always` falls back to a short commit hash for untagged dev builds.
LDFLAGS := "-X main.version=$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//') -X main.commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

# List available commands
[private]
@default:
    just --list
    echo ""
    echo "For help with a specific recipe, run: just --usage <recipe>"

# ============================================================================
# Development
# ============================================================================

# Run all quality checks
[group("dev")]
check: format lint test
    @echo "✓ All checks passed!"

# Format the codebase
[group("dev")]
format:
    gofmt -s -w .
    go mod tidy

# Lint the codebase
[group("dev")]
lint:
    go vet {{PKG}}
    @command -v staticcheck >/dev/null && staticcheck {{PKG}} || echo "staticcheck not installed, skipping"
    @command -v govulncheck >/dev/null && govulncheck {{PKG}} || echo "govulncheck not installed, skipping"

# Run tests
[group("dev")]
test:
    go test -race -count=1 {{PKG}}

# ============================================================================
# Build & Demo
# ============================================================================

# Build the binaries into ./bin/ (juju-lens and the eBPF probe)
[group("build")]
build:
    mkdir -p bin
    go build -ldflags "{{LDFLAGS}}" -o bin/{{BIN}} ./cmd/juju-lens
    go build -ldflags "{{LDFLAGS}}" -o bin/{{BIN}}-probe ./cmd/juju-lens-probe

# Cut a release: validate, run checks, then tag and push (CI builds and publishes)
[group("build")]
release VERSION:
    #!/usr/bin/env bash
    set -euo pipefail
    die() { echo "error: $*" >&2; exit 1; }
    version="{{VERSION}}"

    [[ "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must look like vX.Y.Z, got '${version}'"
    [[ "$(git rev-parse --abbrev-ref HEAD)" == main ]] || die "releases are cut from main"
    git fetch --quiet origin main --tags
    [[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/main)" ]] || die "main is not in sync with origin/main"
    git rev-parse -q --verify "refs/tags/${version}" >/dev/null && die "tag ${version} already exists"

    # The version is the tag itself (no VERSION file), so "bumping" means
    # picking a tag that is strictly newer than the latest existing one.
    latest="$(git tag --list 'v*' --sort=-v:refname | head -n1)"
    if [[ -n "${latest}" ]]; then
        newest="$(printf '%s\n%s\n' "${latest}" "${version}" | sort -V | tail -n1)"
        [[ "${newest}" == "${version}" ]] || die "${version} is not newer than the latest tag ${latest}"
    fi

    just check
    [[ -z "$(git status --porcelain)" ]] || die "working tree is dirty (uncommitted changes, or checks reformatted files)"

    echo "latest tag: ${latest:-none}"
    read -r -p "Tag and push ${version}? [y/N] " answer
    [[ "${answer}" == y || "${answer}" == Y ]] || die "aborted"
    git tag -a "${version}" -m "${version}"
    git push origin "${version}"
    echo "pushed ${version}; the release workflow will build and publish it"

# Cross-compile release archives (linux/amd64, linux/arm64) into ./dist/
[group("build")]
dist:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p dist
    VERSION="$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')"
    # Only linux: juju-lens-probe is eBPF/Linux-only, and darwin builds
    # would just ship a functionless stub. `go build`/`just build` still
    # work fine on other platforms for local development.
    for target in linux/amd64 linux/arm64; do
        os="${target%/*}" arch="${target#*/}"
        workdir="$(mktemp -d)"
        trap 'rm -rf "${workdir}"' EXIT
        GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
            go build -trimpath -ldflags "{{LDFLAGS}} -s -w" -o "${workdir}/{{BIN}}" ./cmd/juju-lens
        GOOS="${os}" GOARCH="${arch}" CGO_ENABLED=0 \
            go build -trimpath -ldflags "{{LDFLAGS}} -s -w" -o "${workdir}/{{BIN}}-probe" ./cmd/juju-lens-probe
        archive="dist/{{BIN}}-${VERSION}-${os}-${arch}.tar.gz"
        echo "building ${archive}"
        tar czf "${archive}" -C "${workdir}" {{BIN}} {{BIN}}-probe
        rm -rf "${workdir}"
        trap - EXIT
    done
    (cd dist && sha256sum *.tar.gz > checksums.txt)

# Remove build artifacts
[group("build")]
clean:
    rm -rf bin dist

# End-to-end smoke test: synth a recording and open it in the viewer
[group("run")]
demo: build
    #!/usr/bin/env bash
    set -euo pipefail
    out="recordings/demo-$(date -u +%Y%m%dT%H%M%S)"
    ./bin/{{BIN}} synth trivial --output "${out}"
    echo "recording written to ${out}"
    ./bin/{{BIN}} view "${out}"

# ============================================================================
# Maintenance
# ============================================================================

# Update Go module dependencies
[group("maintenance")]
update:
    go get -u {{PKG}}
    go mod tidy

# Print the local build version, the latest tag and the latest GitHub release
[group("maintenance")]
version:
    #!/usr/bin/env bash
    echo "local build:    $(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//') ($(git rev-parse --short HEAD 2>/dev/null || echo unknown))"
    git fetch --quiet --tags origin 2>/dev/null || echo "(could not fetch tags from origin)"
    echo "latest tag:     $(git tag --list 'v*' --sort=-v:refname | head -n1 | grep . || echo none)"
    if command -v gh >/dev/null; then
        echo "latest release: $(gh release view --json tagName --jq .tagName 2>/dev/null || echo none)"
    else
        echo "latest release: (gh not installed)"
    fi
