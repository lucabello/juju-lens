# juju-lens

A time machine for Juju controllers. `juju-lens` records everything a Juju
controller and its models emit, then plays it back offline in a TUI where
events, state changes, and logs are correlated by time and by trace.

See [`docs/`](./docs/README.md) for how it works and how to use it, and
[VISION.md](./VISION.md) for the full long-term design — this README covers
what's implemented today.

## Status

Working:

- **`juju-lens record`** attaches a read-only eBPF probe to a `jujud`/
  `containeragent` process — locally, or on a machine controller over `juju
  ssh` — and captures Juju API RPCs plus `juju debug-log`, Kubernetes
  workload-container logs, and machine journald. It never mutates the
  controller. See [how to record a controller](docs/how-to/record-a-controller.md).
- **`juju-lens watch`** does the same, live: record and view in one command,
  so you can follow hooks, status, and relations in real time. See [how to
  watch a live model](docs/how-to/watch-a-live-model.md).
- **`juju-lens stop`** signals a running recorder to shut down cleanly. See
  [how to stop a recording](docs/how-to/stop-a-recording.md).
- **`juju-lens index`** rebuilds the SQLite index from raw captured data.
  See [how to rebuild the index](docs/how-to/rebuild-the-index.md).
- **`juju-lens view`** opens a recording in a bubbletea TUI: Events and
  Status side by side, Logs spanning beneath, all synchronised to the
  selected instant. See the [viewer keybindings
  reference](docs/reference/viewer-keybindings.md).
- **`juju-lens export`** writes a recording's derived narrative — hook runs
  with failure classification, the state they changed, correlated logs — as
  a folder meant for an agent to read incrementally. See [how to export for
  an agent](docs/how-to/export-for-an-agent.md).
- **`juju-lens synth trivial`** writes a byte-for-byte reproducible
  synthetic recording, for trying the tool with no Juju controller at all.
  See [how to generate a synthetic recording](docs/how-to/generate-a-synthetic-recording.md).

Kubernetes-native probe attach (`kubectl-debug`, `daemonset` — capturing a
CAAS unit's own RPC traffic) is designed in VISION.md but not yet built;
`record`/`watch` currently support `--attach local` and `--attach ssh`
only. Kubernetes *log* ingestion (workload-container stdout) is unaffected
and already works.

## Documentation

- [Your first recording](docs/tutorials/first-recording.md) — capture a
  real Juju controller while a charm's integration tests run against it.
- [How-to guides](docs/README.md#how-to-guides) — recording, watching,
  stopping, exporting, or generating a synthetic recording instead.
- [Reference](docs/README.md#reference) — CLI flags, on-disk layout, SQLite
  schema, keybindings.
- [Explanation](docs/README.md#explanation) — how capture works, how a
  captured byte becomes a viewer row, why this architecture.

## Requirements

- **Go** ≥ 1.25.
- **just** (recipe runner) — install from
  [github.com/casey/just](https://github.com/casey/just).

## Installing

```bash
curl -fsSL https://raw.githubusercontent.com/lucabello/juju-lens/main/install.sh | sh
```

Downloads and verifies the latest release from
[GitHub Releases](https://github.com/lucabello/juju-lens/releases) and
installs `juju-lens` + `juju-lens-probe` to `/usr/local/bin` (or
`~/.local/bin` if that isn't writable). Only `linux/amd64` and
`linux/arm64` are published — `juju-lens-probe` is eBPF-based and only
functions on Linux; see [Just recipes](#just-recipes) below to build from
source on other platforms (dev/TUI use only). Set `JUJU_LENS_VERSION` to
pin a specific release, or `JUJU_LENS_INSTALL_DIR` to change where the
binaries go.

Releases are cut by pushing a tag (`git tag v0.1.0 && git push origin
v0.1.0`); a GitHub Actions workflow builds and publishes from there — see
`.github/workflows/release.yml`.

Prefer to build it yourself? `go install
github.com/lucabello/juju-lens/cmd/juju-lens@latest` (and, separately,
`.../cmd/juju-lens-probe@latest`) works with just a Go toolchain, no
release infrastructure involved.

## Getting started

```bash
just build   # → ./bin/juju-lens and ./bin/juju-lens-probe
just demo    # generate a synthetic recording and open it in the viewer
```

That's the fastest way to see the viewer, with a fabricated recording and
no Juju controller involved. For a walkthrough that captures a real
deployment instead, see the [first-recording
tutorial](docs/tutorials/first-recording.md).

## Repository layout

```
cmd/juju-lens/        # tiny main; delegates to internal/cli
cmd/juju-lens-probe/  # standalone eBPF probe binary (TLS-boundary capture)
internal/cli/         # cobra subcommands (record, watch, stop, synth, index, view, export, version)
internal/probe/       # eBPF attach + frame protocol + symbol resolution + ingest
internal/wire/        # Juju RPC envelope, websocket reassembly, capture format
internal/index/       # SQLite index: schema, span/snapshot writers, extractors
internal/recording/   # on-disk layout, manifest, rotating writer, span reader, pid file
internal/narrative/   # derives hook runs/failure state/status attribution from spans
internal/synth/       # deterministic RPC scenario generator
internal/viewer/      # bubbletea TUI (Events + Status + Logs, model picker)
internal/export/      # writes the derived-narrative export folder
docs/                 # user documentation (tutorials, how-to, reference, explanation)
VISION.md             # full design document
justfile              # build / test / run / demo recipes
```

## Recording on disk

A recording is a plain directory; nothing is compressed, and the SQLite
index is a rebuildable build artifact. See the [recording layout
reference](docs/reference/recording-layout.md) for the full breakdown.

## Just recipes

```
just build        # → ./bin/juju-lens
just install      # go install into $GOBIN
just release      # linux/amd64 + linux/arm64 archives + checksums → ./dist/
just run -- version
just record my-controller
just stop /path/to/recording  # SIGTERM the background recorder
just synth trivial /tmp/rec
just index /tmp/rec           # rebuild index.db from raw/
just view /tmp/rec
just demo         # synth + open in the viewer
just test         # go test -race -count=1 ./...
just coverage
just check        # format + lint + test
just format       # gofmt + go mod tidy
just lint         # go vet, staticcheck, govulncheck (skipped if not installed)
just update       # go get -u ./...
just clean
```

## License

Apache-2.0.
