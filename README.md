# juju-lens

A time machine for Juju controllers. `juju-lens` records everything a Juju
controller and its models emit, then plays it back offline in a TUI where
events, state changes, and logs are correlated by time and by trace.

See [VISION.md](./VISION.md) for the full design; this README documents
what is actually implemented today (milestone M1).

## Status

**Milestone M1 — Skeleton.** Working:

- `juju-lens record` starts an OTLP gRPC server, writes every trace payload
  it receives as JSONL under `raw/otlp/`, and stops cleanly on Ctrl-C, on
  `--max-duration`, or on `--max-size`.
- `juju-lens synth trivial` writes a byte-for-byte reproducible synthetic
  recording that mimics two Juju applications (grafana, prometheus) forming
  one relation. Useful for developing the viewer without a live controller.
- `juju-lens view` opens a recording in a bubbletea TUI with a scrollable
  timeline and a details pane showing every span attribute.
- `juju-lens version` prints the build stamp.

Not yet implemented (see VISION.md for milestone plan):

- Auto-configuring the controller's `open-telemetry-*` keys.
- Ingesting `juju debug-log`, Kubernetes pod logs, `journalctl`, `snap logs`.
- Deriving databag / status / secret snapshots from spans.
- The SQLite index, the applications / machines / relations sidebars, split
  view, filter overlay, follow mode.

## Requirements

- **Go** ≥ 1.24.
- **just** (recipe runner) — install from
  [github.com/casey/just](https://github.com/casey/just).

## Getting started

```bash
just build          # → ./bin/juju-lens

# Generate a synthetic recording and open it.
just demo

# Or explicitly:
just synth trivial /tmp/rec-trivial
just view /tmp/rec-trivial
```

Recording live traces from a controller (manual OTEL config for M1):

```bash
just build
./bin/juju-lens record my-controller \
    --output ./recordings/my-controller \
    --otlp-addr 127.0.0.1:4317 \
    --max-duration 30m

# In another shell, tell the controller to send traces here.
# The record command prints the exact snippet to copy.
juju controller-config \
    open-telemetry-enabled=true \
    open-telemetry-endpoint=127.0.0.1:4317 \
    open-telemetry-insecure=true \
    open-telemetry-sample-ratio=1.0
```

Open the recording later:

```bash
./bin/juju-lens view ./recordings/my-controller
```

Viewer keys:

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | move cursor |
| `PgUp`/`b`, `PgDn`/` `/`f` | page |
| `Home`/`g`, `End`/`G` | jump to first / last event |
| `q`, `Esc`, `Ctrl+C` | quit |

## Repository layout

```
cmd/juju-lens/        # tiny main; delegates to internal/cli
internal/cli/         # cobra subcommands (record, synth, view, version)
internal/otlpsink/    # embedded OTLP gRPC receiver + JSON writer helper
internal/recording/   # on-disk layout, manifest, rotating writer, span reader
internal/synth/       # deterministic OTLP scenario generator
internal/viewer/      # bubbletea TUI
VISION.md             # full design document
justfile              # build / test / run / demo recipes
```

## Recording on disk

A recording is a plain directory. Nothing is compressed; the SQLite index
is optional and rebuildable from `raw/`.

```
recordings/2026-07-03T14-30-12--mycontroller/
├── manifest.json                       # controller, models, versions, sources, end reason
├── raw/
│   └── otlp/
│       └── traces-2026-07-03T14.jsonl  # OTLP protobufs, one JSON per line
└── derived/                            # cached snapshots/diffs (later milestones)
```

## Just recipes

```
just build        # → ./bin/juju-lens
just install      # go install into $GOBIN
just release      # cross-compile linux/darwin × amd64/arm64 → ./dist/
just run -- version
just record my-controller
just synth trivial /tmp/rec
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
