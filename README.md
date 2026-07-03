# juju-lens

A time machine for Juju controllers. `juju-lens` records everything a Juju
controller and its models emit, then plays it back offline in a TUI where
events, state changes, and logs are correlated by time and by trace.

See [VISION.md](./VISION.md) for the full design; this README documents
what is actually implemented today.

## Status

**Milestone M2 — SQLite index + three-column TUI.** Working:

- `juju-lens record` starts an OTLP gRPC server, writes every trace payload
  it receives as JSONL under `raw/otlp/`, and builds `index.db` on shutdown.
  Unless `--no-set-otel` is passed, it also configures the target
  controller's `open-telemetry-*` keys via the `juju` CLI and restores the
  previous values on clean shutdown. The target is `--controller` when
  set, otherwise the juju CLI's currently active controller. Stops on
  Ctrl-C, on SIGTERM (delivered by `juju-lens stop`), on `--max-duration`,
  or on `--max-size`.
- `juju-lens stop <recording>` signals a running recorder (found via
  `recorder.pid` in the recording directory) with SIGTERM so it can
  shut down cleanly — useful when the recorder was started with `&` or
  in a systemd unit. `--force --timeout 5s` escalates to SIGKILL.
- `juju-lens synth trivial` writes a byte-for-byte reproducible synthetic
  recording that mimics two Juju applications (grafana, prometheus) forming
  one relation, sets a workload status on each unit, and sets the leader's
  application status. Ships with a pre-built index.
- `juju-lens index <recording>` rebuilds `index.db` from `raw/` — useful
  when the schema changes or when opening a recording captured by an older
  build.
- `juju-lens view` opens a recording in a bubbletea TUI. Three columns:
  - **Applications** — apps → units tree derived from unit-bearing spans.
  - **Timeline + Details** — every span in wall-clock order, with the
    selected span's attributes rendered in a scrollable pane below.
  - **Status** — two independent sections (Applications, Units) that
    reconstruct the *latest known* status per app and per unit from the
    snapshot store. `unknown` rows mark scopes we haven't seen data for
    yet (recorder started mid-life, or an app/unit hasn't reported).
  A model picker appears when the recording contains more than one Juju
  model; press `m` to switch models at any time.
- `juju-lens version` prints the build stamp.

Not yet implemented (see VISION.md for milestone plan):

- Ingesting `juju debug-log`, Kubernetes pod logs, `journalctl`, `snap logs`.
- Point-in-time status reconstruction at the cursor (M4 upgrades the pane
  from "latest known" to "walk backward from cursor to nearest snapshot").
- Agent status alongside workload/app status.
- Databag / secret snapshotter, relations sidebar with diff mode.
- Split view, filter overlay, follow mode.

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

Recording live traces from a controller:

```bash
just build

# `record` auto-sets the controller's open-telemetry-* keys (via the
# `juju` CLI) and restores them on clean shutdown. When --controller is
# omitted the juju CLI's active controller is used, mirroring how every
# other juju command behaves. Pass --no-set-otel to skip that and copy
# the printed snippet by hand.
./bin/juju-lens record my-capture \
    --output ./recordings/my-capture \
    --controller my-juju-controller \
    --otlp-addr 127.0.0.1:4317 \
    --max-duration 30m

# The <name> argument is a label for the recording (used in the directory
# name and stored in manifest.json); it is not passed to `juju`.

# If the controller runs on a different host than the recorder, tell it
# where to send traces with --advertise-endpoint. Common patterns:
#   - k8s controller: kubectl port-forward, then --otlp-addr 127.0.0.1:4317
#   - remote host:    SSH reverse tunnel, then --advertise-endpoint <host>:4317

# Start the recorder in the background, then stop it later with SIGTERM
# via `juju-lens stop`. Stop does the same clean shutdown as Ctrl-C:
# restores OTEL config, finalises the manifest, builds the index.
./bin/juju-lens record my-capture --output ./recordings/my-capture &
./bin/juju-lens stop ./recordings/my-capture

# --force after --timeout sends SIGKILL. That skips OTEL restoration, so
# you'll need to reset the controller keys by hand afterwards.
./bin/juju-lens stop --force --timeout 5s ./recordings/my-capture
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
| `Tab` | cycle focus between panes (M2: timeline-only) |
| `m` | pick a different model (multi-model recordings) |
| `q`, `Esc`, `Ctrl+C` | quit |

## Repository layout

```
cmd/juju-lens/        # tiny main; delegates to internal/cli
internal/cli/         # cobra subcommands (record, stop, synth, index, view, version)
internal/index/       # SQLite index: schema, span/snapshot writers, extractors
internal/juju/        # thin wrapper around the juju CLI (OTEL config get/set)
internal/otlpsink/    # embedded OTLP gRPC receiver + JSON writer helper
internal/recording/   # on-disk layout, manifest, rotating writer, span reader, pid file
internal/synth/       # deterministic OTLP scenario generator
internal/viewer/      # bubbletea TUI (3-column layout + model picker)
VISION.md             # full design document
justfile              # build / test / run / demo recipes
```

## Recording on disk

A recording is a plain directory. Nothing is compressed. The SQLite index
is a build artifact and can be re-created at any time with
`juju-lens index <recording>`.

```
recordings/2026-07-03T14-30-12--mycontroller/
├── manifest.json                       # controller, models, versions, sources, end reason,
│                                       # previous OTEL config for restoration
├── recorder.pid                        # PID of the running recorder (removed on clean exit)
├── index.db                            # SQLite (WAL); rebuildable from raw/
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
