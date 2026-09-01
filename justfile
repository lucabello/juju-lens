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

# Run tests with coverage
[group("dev")]
coverage:
    go test -race -count=1 -coverprofile=coverage.out {{PKG}}
    go tool cover -func=coverage.out | tail -1
    @echo "Full report: go tool cover -html=coverage.out"

# ============================================================================
# Build & Run
# ============================================================================

# Build the binaries into ./bin/ (juju-lens and the eBPF probe)
[group("build")]
build:
    mkdir -p bin
    go build -ldflags "{{LDFLAGS}}" -o bin/{{BIN}} ./cmd/juju-lens
    go build -ldflags "{{LDFLAGS}}" -o bin/{{BIN}}-probe ./cmd/juju-lens-probe

# Install the binaries into $GOBIN (or $GOPATH/bin)
[group("build")]
install:
    go install -ldflags "{{LDFLAGS}}" ./cmd/juju-lens
    go install -ldflags "{{LDFLAGS}}" ./cmd/juju-lens-probe

# Cross-compile release archives (linux/amd64, linux/arm64) into ./dist/
[group("build")]
release:
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
    rm -rf bin dist coverage.out

# Run the binary (arguments after `--`, e.g. `just run -- version`)
[group("run")]
run *ARGS:
    go run -ldflags "{{LDFLAGS}}" ./cmd/juju-lens {{ARGS}}

# Run `record` (eBPF probe capture) into a fresh recording directory
[group("run")]
record CONTROLLER="local":
    #!/usr/bin/env bash
    out="recordings/$(date -u +%Y-%m-%dT%H-%M-%S)--{{CONTROLLER}}"
    mkdir -p "${out%/*}"
    go run ./cmd/juju-lens record {{CONTROLLER}} --output "${out}"

# Signal a running recorder to stop cleanly (detaches probes,
# finalises manifest, builds index).
[group("run")]
stop RECORDING:
    go run ./cmd/juju-lens stop {{RECORDING}}

# Generate a synthetic recording (for viewer development)
[group("run")]
synth SCENARIO="trivial" OUT="recordings/synth-{{SCENARIO}}":
    go run ./cmd/juju-lens synth {{SCENARIO}} --output {{OUT}}

# Open a recording in the viewer
[group("run")]
view RECORDING:
    go run ./cmd/juju-lens view {{RECORDING}}

# Rebuild the SQLite index for a recording from its raw/ tree
[group("run")]
index RECORDING:
    go run ./cmd/juju-lens index {{RECORDING}}

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

# Print the tool version that would be built
[group("maintenance")]
version:
    @echo "$(git describe --tags --always --dirty 2>/dev/null | sed 's/^v//') ($(git rev-parse --short HEAD 2>/dev/null || echo unknown))"
