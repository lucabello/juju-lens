# juju-lens

A time machine for Juju controllers. `juju-lens` records everything a Juju
controller and its models emit, then plays it back offline in a TUI where
events, state changes, and logs are correlated by time and by trace.

See [VISION.md](./VISION.md) for the full design; this README documents
what is actually implemented today.

## Status

**Milestone M2 — SQLite index + three-column TUI.** Working:

- `juju-lens-probe` attaches eBPF uprobes to `crypto/tls.(*Conn).Read/Write`
  in a target `jujud`/`containeragent` process (resolving symbols from the
  stripped binary's `.gopclntab`) and streams the captured plaintext RPC
  frames to stdout as length-prefixed JSON. Requires Linux ≥ 5.8 and
  `CAP_BPF` (or root).
- `juju-lens record` runs the probe — locally (`--attach local`, for a
  jujud installed on this host) or on a machine controller over `juju ssh`
  (`--attach ssh`) — reassembles the websocket RPC stream, writes each
  captured envelope to `raw/rpc/<model>/calls-*.jsonl`, and builds
  `index.db` on shutdown. **It never mutates the controller** — no config
  is read or set; the only side effect is attaching read-only uprobes.
  Stops on Ctrl-C, on SIGTERM (delivered by
  `juju-lens stop`), on `--max-duration`, or on `--max-size`. Kubernetes
  attach (`kubectl-debug`, `daemonset`) lands in M6.
- `juju-lens stop <recording>` signals a running recorder (found via
  `recorder.pid` in the recording directory) with SIGTERM so it can
  shut down cleanly — useful when the recorder was started with `&` or
  in a systemd unit. `--force --timeout 5s` escalates to SIGKILL.
- `juju-lens synth trivial` writes a byte-for-byte reproducible synthetic
  recording — a stream of captured Juju API RPCs — that mimics two Juju
  applications (grafana, prometheus) forming one relation, sets a workload
  status on each unit, and sets the leader's application status. Ships with
  a pre-built index.
- `juju-lens index <recording>` rebuilds `index.db` from `raw/` — useful
  when the schema changes or when opening a recording captured by an older
  build.
- `juju-lens view` opens a recording in a bubbletea TUI. Three columns:
  - **Applications** — apps → units tree derived from the units RPCs came from.
  - **Timeline + Details** — every synthesised RPC span (`<facade>.<method>`)
    in wall-clock order, with the selected span's envelope fields (params,
    response, timing) rendered in a scrollable pane below.
  - **Status** — two independent sections (Applications, Units) that
    reconstruct the *latest known* status per app and per unit from the
    snapshot store. `unknown` rows mark scopes we haven't seen data for
    yet (recorder started mid-life, or an app/unit hasn't reported).
  A model picker appears when the recording contains more than one Juju
  model; press `m` to switch models at any time.
- `juju-lens export <recording>` writes the same derived narrative the TUI
  shows — hook runs with their failure classification, statuses/databags/
  config they changed, merged with every correlated log line — as a folder
  under `<recording>/derived/export` (`--out` to redirect): `SUMMARY.md` for
  orientation, `timeline.jsonl`/`events.jsonl` for grepping, one
  `details/<span_id>.json` per event for full drill-down, `report.md` for a
  human, and an `AGENTS.md` legend. Meant for an agent to read
  incrementally instead of ingesting the whole recording up front.
  `--parts` restricts which files get written; `--unit`, `--since`/
  `--until`, and `--errors-only` restrict what content is in scope.
  `--format json|md` instead writes a single merged document to stdout, for
  scripting.
- `juju-lens version` prints the build stamp.

Not yet implemented (see VISION.md for milestone plan):

- Ingesting `juju debug-log`, Kubernetes pod logs, `journalctl`, `snap logs`.
- Point-in-time status reconstruction at the cursor (M4 upgrades the pane
  from "latest known" to "walk backward from cursor to nearest snapshot").
- Agent status alongside workload/app status.
- Databag / secret snapshotter, relations sidebar with diff mode.
- Split view, filter overlay, follow mode.

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
just build          # → ./bin/juju-lens and ./bin/juju-lens-probe

# Generate a synthetic recording and open it.
just demo

# Or explicitly:
just synth trivial /tmp/rec-trivial
just view /tmp/rec-trivial
```

Recording live RPCs from a controller:

```bash
just build

# `record` attaches the eBPF probe read-only; it never changes controller
# config. Attach modes: `local` (jujud on this host) or `ssh` (a machine
# controller reached over `juju ssh`). Requires Linux >= 5.8 and CAP_BPF on
# whichever host the target process runs on.
./bin/juju-lens record my-capture \
    --output ./recordings/my-capture \
    --attach ssh \
    --controller my-juju-controller \
    --ssh-target controller/0 \
    --max-duration 30m

# The <name> argument is a label for the recording (used in the directory
# name and stored in manifest.json); it is not passed to `juju`.

# Point --attach at a locally-installed jujud snap instead:
sudo ./bin/juju-lens record local-capture --attach local

# Inspect candidate targets the probe would attach to:
sudo ./bin/juju-lens-probe --list

# Start the recorder in the background, then stop it later with SIGTERM
# via `juju-lens stop`. Stop does the same clean shutdown as Ctrl-C:
# detaches the probes, finalises the manifest, builds the index.
./bin/juju-lens record my-capture --output ./recordings/my-capture &
./bin/juju-lens stop ./recordings/my-capture

# --force after --timeout sends SIGKILL for an unresponsive recorder.
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
cmd/juju-lens-probe/  # standalone eBPF probe binary (TLS-boundary capture)
internal/cli/         # cobra subcommands (record, stop, synth, index, view, version)
internal/probe/       # eBPF attach + frame protocol + symbol resolution + ingest
internal/wire/        # Juju RPC envelope, websocket reassembly, capture format
internal/index/       # SQLite index: schema, span/snapshot writers, extractors
internal/recording/   # on-disk layout, manifest, rotating writer, span reader, pid file
internal/synth/       # deterministic RPC scenario generator
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
├── manifest.json                       # controller, models, versions, sources,
│                                       # end reason, attach mode + targets
├── recorder.pid                        # PID of the running recorder (removed on clean exit)
├── index.db                            # SQLite (WAL); rebuildable from raw/
├── raw/
│   └── rpc/
│       └── default/                    # one directory per model
│           └── calls-2026-07-03T14.jsonl  # captured Juju API RPCs, one per line
└── derived/                            # cached snapshots/diffs (later milestones)
```

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
