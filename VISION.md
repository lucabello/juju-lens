# juju-lens — Vision

> A time machine for Juju: record everything a controller and its models do, then browse the history offline as a correlated, navigable timeline of events, state changes, and logs.

## 1. Problem

Debugging Juju charms today means jumping between many surfaces:

- `juju status` and `juju status --relations --watch` for a moving snapshot of the model
- `juju debug-log` for what agents and charms print
- `juju show-unit` for a static view of relation databag contents
- `kubectl logs -f` / `journalctl -f` / `snap logs -f` for the container or host underneath
- `juju exec -- relation-get` incantations to inspect databags after the fact
- If tracing is enabled, a Grafana Tempo or Jaeger UI in a browser

The information is all there but scattered across tools, transient (streams get lost the moment you close them), and impossible to reason about after the fact. Reproducing an issue often means running it again, hoping the same race triggers, and staring at four terminals simultaneously.

`juju-lens` records everything a controller and its models emit into a single on-disk *recording*, then plays it back offline in a single TUI where events, state deltas, and logs are correlated by time, unit, relation, and — where possible — by trace ID.

## 2. What we capture

Juju's runtime is entirely push/watcher-based already. `juju-lens` captures at those push points; it never polls the model for state.

### 2.1 Primary source: OpenTelemetry traces (both Juju 3.6 and 4.x)

Juju has native OTLP tracing built in since 3.5. Once enabled via controller config, every API facade call and every uniter operation is spanned automatically:

- `apiserver/root.go` spans every facade RPC (client and agent).
- `internal/worker/uniter/operation/executor.go` spans every uniter operation on every unit — this includes every hook run, tagged with `executor.state` and `executor.unit`, and `pprof.Do` labels stamp `otel.traceid` so profiles align with traces.

Controller config keys (settable pre- or post-bootstrap):

- `open-telemetry-enabled`
- `open-telemetry-endpoint`
- `open-telemetry-insecure`
- `open-telemetry-stack-traces`
- `open-telemetry-sample-ratio` (default 0.10 — we set 1.0 for recording)
- `open-telemetry-tail-sampling-threshold`

`juju-lens record` sets these to point at the recorder's embedded OTLP receiver, restores them on shutdown, and remembers the previous values.

### 2.2 Why not just talk to the Juju API?

We considered building this on top of the AllWatcher / WatchAll model-events stream. This has two blocking problems, both verified against `juju/juju@main`:

1. **Juju 4.x removed it.** `apiserver/facades/client/client/client.go:75` and `apiserver/facades/client/controller/controller.go:488` return `errors.NotImplementedf("WatchAll")` / `"WatchAllModels"`. The release notes describe a replacement "lighter event API" but it isn't implemented in main yet. On 4.x the only client-facing streams left are `WatchDebugLog` and `WatchActionsProgress`.
2. **Even on 3.6, AllWatcher does not stream relation databag contents.** Databags live behind the Uniter facade which authenticates unit agents, not admin clients. `juju show-unit` is a one-shot inspection, not a stream.

So tracing is the primary source. The Juju API is used only for the things it does well and works on both versions:

- `Status()` once on connect and on tracer-derived model-change signals — to bootstrap the app/unit/machine inventory
- `WatchDebugLog` for the model-wide Juju log stream
- `WatchActionsProgress` for action progress
- Kubernetes / systemd APIs for underlying container/host logs

### 2.3 What we derive from spans

Spans carry timing and causality. State comes from span *attributes* on write-side facade calls. The recorder builds a state history by watching these:

| Signal | Source span | What we snapshot |
|---|---|---|
| Hook fired | `runHook.Execute` (uniter) | hook kind, relation, remote unit, remote app, storage id, secret URI, exit code, duration |
| Databag write | `UniterAPI.CommitHookChanges`, `RelationUnitSettings` in payload | per-relation, per-unit and per-app databag after each hook |
| Relation lifecycle | `UniterAPI.EnterScope`, `LeaveScope`, `SetRelationStatus` | membership, suspended, status |
| Unit / app status | `UniterAPI.SetStatus` and status service calls | workload/agent/app status + message |
| Unit stored state | `UniterAPI.SetState` (client-side of `state-set`) | KV blob per unit |
| Secrets | `SecretsManagerAPI.*` | metadata, rotation policy, grants, revisions, obsolete revisions |
| Open ports | `UniterAPI.OpenPorts` etc. | per-endpoint port ranges |
| Leadership | leadership facade | leader changes, claim expiry |
| Charm URL / mod version | uniter facade | charm upgrades |
| Actions | action facade + `WatchActionsProgress` | id, name, params, status, results, messages |
| Model config | model config facade | key-value changes |
| jujuc RPCs (intra-hook) | `Jujuc.Main` per-tool | every hook tool the charm invoked, in order |

For each write we compute a JSON diff against the previous snapshot for the same scope, hash the content, and only persist a new snapshot when the hash changes. The viewer never re-derives state; it queries the snapshots directly.

### 2.4 Logs (correlated but treated as a separate stream)

For every recording we pick up log sources automatically as the model evolves:

- **`juju debug-log --tail --format json`** — always on, one process per model. Gives every agent's and every charm's log line in a single stream.
- **Kubernetes pod logs** (CAAS controllers) — a shared-informer watches Pods in the model's namespace, keyed by Juju labels (`juju.io/model-uuid`, `juju.io/unit`). Every new container gets a follower goroutine that calls `CoreV1().Pods().GetLogs(pod, {Follow: true, Container: c, Timestamps: true})`. Deleted pods stop cleanly.
- **`journalctl -f -o json -u <unit>`** (machine controllers) — one SSH session per host discovered via `juju ssh`, filtered to `jujud-machine-*.service`, `jujud-unit-*.service`, and `snap.juju.*.service`. When new machines appear in `Status()`, new sessions are opened.
- **`snap logs -f <snap>`** where relevant.

Logs are normalised to a common record shape and, when Juju emits `trace_id`/`span_id` in structured logs (native slog attributes since 3.5), those become first-class join keys against the trace store.

Log ingesters start/stop dynamically as the recorder learns about new units/pods/machines from status deltas and span attributes. Nothing is hard-coded per recording.

### 2.5 What we intentionally do not capture (v1)

- **Continuous profiling** (pprof). Documented as future work; the `otel.traceid` pprof label is preserved so this is a drop-in extension.
- **Metrics.** Not needed for the timeline; may be added later as an at-a-glance density indicator.
- **Charm source code.** The viewer references file:line from spans/logs; it does not embed sources.

## 3. On-disk format

A recording is a **directory**. Everything the tool wrote is human-inspectable text; the SQLite index is a build artifact that the viewer opens read-only.

```
recordings/2026-07-03T14-30-12--mycontroller/
├── manifest.json                        # controller, models, versions, sources enabled, clock offsets
├── index.db                             # SQLite (WAL); rebuildable from raw/
├── raw/
│   ├── otlp/
│   │   ├── traces-2026-07-03T14.jsonl   # OTLP protobufs, one message per line, JSON-encoded
│   │   ├── logs-2026-07-03T14.jsonl
│   │   └── metrics-2026-07-03T14.jsonl  # (future)
│   ├── juju/
│   │   └── default/                     # per-model directory (model name)
│   │       └── debug-log.jsonl
│   ├── k8s/
│   │   └── default/                     # per-model
│   │       ├── grafana-0/
│   │       │   ├── charm.log
│   │       │   ├── grafana.log
│   │       │   └── metadata.json        # container list, image ids, restarts
│   │       └── prometheus-0/
│   │           └── ...
│   ├── machine/
│   │   └── controller/                  # per-model
│   │       └── 0/
│   │           └── journal.jsonl
│   └── snap/
│       └── controller/
│           └── 0/juju.log
└── derived/
    ├── snapshots/                       # optional cached JSON snapshots keyed by scope+ts
    └── diffs/                           # optional cached diffs
```

Design decisions:

- **Plain text everywhere.** No gzipping. `raw/` files are `.jsonl` or `.log`, greppable with standard tools. This is a firm requirement — the tool must remain useful when the TUI itself misbehaves.
- **Per-model subdirectories** under every source. A recording of a controller with three models produces three sibling folders under each source.
- **Rotation by wall-clock hour** for high-volume raw files; a symlink `current.jsonl` points at the active file for tail-friendliness.
- **`manifest.json`** captures everything the viewer needs to interpret the data: controller name and UUID, models observed, Juju/agent versions per model, controller version, source list with per-source status, clock offsets between sources, recording start/end, and whether the recorder set the OTEL config keys (for restoration).
- **The DB is a build artifact.** `juju-lens index <recording>` can rebuild `index.db` from `raw/`. This means we can evolve the schema and re-index old recordings.
- **Size/duration caps stop the recorder cleanly.** No ring-buffer eviction — that would leave gaps mid-timeline. `--max-duration 2h` and `--max-size 500M` are hard stops; when reached the recorder writes a `manifest.json` `end_reason` field and exits.

### 3.1 SQLite index schema (rebuildable from raw)

```sql
CREATE TABLE controllers (
  id INTEGER PRIMARY KEY, uuid TEXT UNIQUE, name TEXT, version TEXT
);
CREATE TABLE models (
  id INTEGER PRIMARY KEY, controller_id INTEGER, uuid TEXT UNIQUE,
  name TEXT, type TEXT   -- 'iaas' | 'caas'
);

CREATE TABLE spans (
  trace_id BLOB, span_id BLOB PRIMARY KEY, parent_span_id BLOB,
  model_id INTEGER,
  ts_start INTEGER, ts_end INTEGER,
  name TEXT, service TEXT,
  attrs_json TEXT, status TEXT
);
CREATE INDEX spans_ts        ON spans(model_id, ts_start);
CREATE INDEX spans_trace     ON spans(trace_id);
CREATE INDEX spans_parent    ON spans(parent_span_id);

CREATE TABLE log_records (
  id INTEGER PRIMARY KEY,
  model_id INTEGER,
  ts INTEGER, source TEXT,           -- 'debug-log','k8s','journal','snap'
  host TEXT, unit TEXT, container TEXT,
  severity TEXT, body TEXT,
  trace_id BLOB, span_id BLOB,
  attrs_json TEXT, raw_offset INTEGER, raw_file TEXT
);
CREATE INDEX log_ts    ON log_records(model_id, ts);
CREATE INDEX log_span  ON log_records(span_id);
CREATE INDEX log_unit  ON log_records(unit, ts);

CREATE TABLE snapshots (
  id INTEGER PRIMARY KEY,
  model_id INTEGER,
  ts INTEGER, kind TEXT,              -- 'databag','unit-status','app-status','app-config','secret','ports','stored-state','leadership'
  scope TEXT,                         -- canonical id: 'databag:3:grafana/0' or 'unit-status:prometheus/0'
  content_hash BLOB,
  body_json TEXT,
  producing_span_id BLOB
);
CREATE INDEX snap_scope_ts ON snapshots(model_id, scope, ts);

CREATE TABLE inventory (              -- authoritative "what existed at time t" per model
  id INTEGER PRIMARY KEY,
  model_id INTEGER, ts INTEGER,
  kind TEXT,                          -- 'app','unit','machine','relation'
  name TEXT, present INTEGER,         -- boolean: appeared (1) or disappeared (0)
  attrs_json TEXT
);
CREATE INDEX inv_ts ON inventory(model_id, ts);
```

## 4. CLI surface

```
juju-lens record <controller> [flags]
    --model <name>            # repeatable; default: all models
    --output <dir>            # default: ./recordings/<ts>--<controller>
    --endpoint <host:port>    # OTLP endpoint we advertise back to Juju (auto-detected)
    --tunnel                  # set up an SSH/kubectl port-forward automatically
    --max-duration <dur>      # e.g. 30m, 2h
    --max-size <size>         # e.g. 500M, 2G
    --no-set-otel             # don't touch controller config; assume already configured
    --redact <pattern,...>    # additional key patterns to redact in databags/secrets
    --sources debug-log,k8s,journal,snap  # opt out of specific log sources

juju-lens view <recording> [flags]
    --model <name>            # focus on a single model at startup
    --follow                  # tail an in-progress recording

juju-lens index <recording>   # rebuild index.db from raw/
juju-lens verify <recording>  # sanity-check integrity, list gaps
juju-lens export <recording> [--format=json|otlp]  # for external analysis
juju-lens synth <scenario>    # generate a synthetic recording for viewer development
```

### 4.1 `record` lifecycle

1. Parse `~/.local/share/juju` (or `JUJU_DATA`) to reach the controller with existing credentials.
2. `Status()` for each selected model → seed inventory.
3. Read controller config for `open-telemetry-*` keys; snapshot current values into `manifest.json`.
4. Unless `--no-set-otel`, set `open-telemetry-enabled=true`, `open-telemetry-endpoint=<us>`, `open-telemetry-sample-ratio=1.0`, `open-telemetry-insecure=true` (unless a cert path is provided).
5. If `--tunnel`, spawn `juju ssh -m controller 0 -R <port>:localhost:<port>` for machine controllers or `kubectl -n controller-<name> port-forward svc/controller <port>:<port>` for K8s; `--endpoint` becomes `localhost:<port>` on the controller side.
6. Start embedded OTLP receiver (gRPC and HTTP).
7. For each selected model, start log ingesters based on source availability.
8. Start a background reconciler that watches `Status()` deltas (via facade watchers where available, or driven by spans on 4.x) and adds/removes log ingesters as units/pods/machines appear and disappear.
9. Write raw payloads as they arrive; a separate indexer goroutine feeds the SQLite index.
10. On `SIGINT`/`SIGTERM`/max-cap: stop receivers, flush indexer, restore controller OTEL config, finalize `manifest.json`.

### 4.2 `view` lifecycle

1. Open `index.db` read-only, load `manifest.json`.
2. Render TUI with focused model (or model picker if multiple exist).
3. All navigation is SQL-backed with prepared statements; the viewer never reads raw files unless the user requests raw log context.
4. If `--follow`, poll `MAX(ts)` from each table every 250 ms and advance the timeline.

## 5. Recorder internals

### 5.1 OTLP ingest

Embed the OpenTelemetry Collector's OTLP receiver as a library:

```go
import (
    "go.opentelemetry.io/collector/receiver/otlpreceiver"
    "go.opentelemetry.io/collector/consumer"
)
```

We instantiate the receiver directly with a `Config` for gRPC (`:4317`) and HTTP (`:4318`), and pass our own `consumer.Traces` / `consumer.Logs` / `consumer.Metrics` implementations that:

1. Append raw OTLP-encoded payload to the appropriate `raw/otlp/*.jsonl` file.
2. Emit each record to the indexer channel.

This gives us OTLP semver compatibility, retries, batching, backpressure, and the HTTP endpoint for free — all things we would spend a week reimplementing.

### 5.2 Indexer

A single goroutine drains channels from every source (OTLP traces/logs/metrics + log ingesters + Juju API events) into SQLite, batched by 100 records or 200 ms, whichever comes first. WAL mode; `synchronous=NORMAL`. The DB write is fire-and-forget from the ingesters' perspective; if the indexer falls behind, ingesters continue to write raw files and the DB catches up.

### 5.3 Model / inventory reconciler

Because Juju 4.x has no client-facing model watcher, we drive inventory reconciliation from two sources:

- On connect: `Status()` for a full snapshot.
- Ongoing: whenever a span with a new `service.instance.id` / `juju.unit` / `juju.model` attribute arrives, or when `debug-log` mentions a unit we don't know about, the reconciler runs a targeted `Status()` for that model.

This is not polling — the reconciler runs *because* something changed, not on a timer. On 3.6 we optionally also open `WatchAll` and use its deltas as an extra signal source.

### 5.4 Databag reconstruction

The trickiest single piece. The recorder must produce a "before/after databag" for every hook execution. Approach:

1. `runHook.Execute` span has trace attributes identifying the hook, relation id, remote unit.
2. `CommitHookChanges` span (child, or later sibling on the controller side) carries the write batch as attributes, including `relation-unit-settings` and `relation-app-settings`.
3. The recorder maintains an in-memory per-scope current-value map. On each `CommitHookChanges`:
   - Compute the new value per scope.
   - Diff against the map.
   - If different: insert a `snapshots` row with `producing_span_id=<CommitHookChanges span>`, update the map.
4. When we cannot see a scope's initial value (recorder started mid-life), we mark it `initial=false` in the snapshot; the viewer draws that snapshot with a "state at recording start" chevron so users know it wasn't a change event.

### 5.5 Log ingesters

Each ingester is a `type Ingester interface { Run(ctx) error; Stop() }` implementation. The reconciler owns a map of `key → Ingester` and starts/stops them based on inventory diffs. Ingesters write to their own file under `raw/` and emit `LogRecord` messages to the indexer channel.

Trace/span correlation for logs:

- Juju's `debug-log --format=json` includes structured fields — when a log line was produced inside a spanned context, `trace_id` and `span_id` are present. The ingester copies them into the log record.
- For k8s/journal/snap logs, we usually don't have trace ids. The viewer falls back to time-window + unit-name matching for those.

### 5.6 Redaction

`--redact` accepts a comma-separated list of glob patterns applied to databag keys, secret content keys, and log JSON fields. Defaults always redact:

- `*token*`, `*password*`, `*secret*`, `*key*` (case-insensitive) in databag/secret values
- `secret-content-*` span attributes

Redaction happens *before* writing to `raw/` — the recording never contains the sensitive bytes on disk.

## 6. Viewer internals

Framework: **bubbletea + bubbles + lipgloss**, plus `bubblezone` for mouse zones. `lipgloss` for the layout math; `bubbles/viewport` for scrolling panels; `bubbles/list` for the sidebars; `bubbles/table` for tabular views; `bubbles/textinput` for filters; `bubbles/help` for keybinding hints.

### 6.1 Model architecture

One top-level `tea.Model` holds:

```go
type Model struct {
    db       *sql.DB
    manifest Manifest

    focus     PaneID              // which pane has keyboard focus
    layout    Layout              // computed by lipgloss on WindowSizeMsg
    theme     Theme

    // Left column
    apps       AppsPane           // apps → units tree (lazy-loaded)
    machines   MachinesPane
    models     ModelsPane

    // Center column
    timeline   TimelinePane       // main event list
    details    DetailsPane        // span + logs + snapshot for selected event

    // Right column
    relations  RelationsPane      // relations tree; opens databag history

    // Overlays
    filter     FilterOverlay
    timeJump   TimeJumpOverlay
    helpOv     HelpOverlay
    split      Option[SplitState] // optional side-by-side comparison
}
```

Each pane is its own `tea.Model` that receives only messages relevant to it (parent model routes). Query results arrive as `tea.Msg` from goroutines; SQL never blocks the event loop.

### 6.2 Layout

```
┌ Applications ──────────┬ Timeline ─────────────────────────────────────┬ Status ─────────────────┐
│ ▾ grafana        ●   │  14:30:12.104  grafana/0    install            │ Applications            │
│    grafana/0  idle   │  14:30:14.882  grafana/0    config-changed     │  ● grafana     active   │
│    grafana/1  active │  14:30:15.301  grafana/0    start              │      Ready              │
│ ▾ prometheus    ●   │  14:30:16.552  grafana:src  relation-created   │  ● prometheus  waiting  │
│    prometheus/0 idle │▶ 14:30:17.010  prom/0       relation-joined    │      waiting for peers  │
│ ▾ traefik            │  14:30:17.244  grafana/0    relation-changed   │  ○ traefik     unknown  │
│    traefik/0  active │  14:30:17.812  prom/0       relation-changed   │                         │
├ Machines ──────────────┤              (▲ selected event)              │ Units                   │
│ 0  started           │─ Details ─────────────────────────────────────  │  ● grafana/0    active  │
│ 1  started           │ span:  runHook relation-joined (grafana-src)   │      Ready              │
├ Models ───────────────┤ dur:   842 ms   exit: 0                        │  ● grafana/1    active  │
│ ● default            │ cause: prom/0 wrote databag (ChgVer 17→18)     │  ○ prometheus/0 waiting │
│ ○ cos                │ ↳ jujuc calls (12): relation-get, relation-set │      installing         │
└────────────────────────┤                    (5), status-set…           │  ● traefik/0    active  │
                        │ ↳ databag after (relation 3, grafana/0):       │                         │
                        │    ingress-address: 10.1.2.3                   │ Relations               │
                        │    scrape_url:      http://…                   │ ▾ grafana-source        │
                        │ ↳ logs (7): [expandable]                       │    grafana ↔ prometheus │
                        │                                                │    databag ▸ view       │
                        │                                                │ ▸ certificates          │
                        └─────────────────────────────────────────────────┴─────────────────────────┘
[q]uit [/] filter [t] time-jump [Tab] focus [Ctrl+\] split [a/u] app/unit view [d] diff [f] follow [s] status
```

The **Status** pane on the right shows a `juju status`-style ribbon reconstructed at the currently-selected timeline instant. It has two independent sections — **Applications** and **Units** — because in Juju they are first-class independent state: an application has its own status set by the leader unit via `status-set --application`, and each unit has its own per-unit status; the app status is *not* an aggregate of unit statuses. Both are surfaced verbatim.

As the user moves the timeline cursor, the pane recomputes both sections from the snapshot store — `SELECT body_json FROM snapshots WHERE model_id=? AND scope=? AND ts<=? ORDER BY ts DESC LIMIT 1` for every known `app-status:<app>` and `unit-status:<unit>` scope. When no snapshot exists yet (recorder started mid-life, or an app/unit hasn't reported yet) the row is dimmed and marked `unknown`. Below the Units section, the same pane hosts the **Relations** tree, since relations are visually and semantically adjacent to status.

### 6.3 View modes

The center pane has three modes, toggled with keys:

- **Unit view (`u`)** — default. Rows are units, timeline shows their hooks and status changes.
- **App view (`a`)** — rows are applications; per-unit dots on each event indicate which unit fired it.
- **Relation view (`r`)** — pivot on relations; timeline shows every databag write, colour-coded by originator.
- **Log view (`l`)** — raw log firehose filtered by current selection.

`d` toggles diff mode: databag/state snapshots render as a JSON diff against the previous snapshot instead of the full value.

### 6.4 Selection semantics

Selecting a timeline event fires a set of queries:

```sql
-- The span itself
SELECT * FROM spans WHERE span_id = ?;

-- Its subtree (jujuc calls, sub-operations)
WITH RECURSIVE sub(span_id) AS (
  SELECT ? UNION ALL
  SELECT s.span_id FROM spans s JOIN sub ON s.parent_span_id = sub.span_id
)
SELECT * FROM spans WHERE span_id IN sub;

-- Logs correlated to the span
SELECT * FROM log_records
 WHERE span_id = ?
    OR (unit = ? AND ts BETWEEN ?-500ms AND ?+span_duration+500ms)
 ORDER BY ts;

-- State at that instant, per scope of interest
SELECT scope, body_json FROM snapshots
 WHERE ts <= ? AND scope LIKE ?
 GROUP BY scope HAVING MAX(ts);
```

### 6.5 Split view

`Ctrl+\` splits the timeline pane into two synchronized timelines with independent selection. Perfect for comparing what grafana and prometheus did during a relation setup.

### 6.6 Time control

- Arrow keys and mouse wheel scroll through events (event-quantised).
- `T` opens a time-jump prompt (`14:30:17.5`, `+5s`, `-2m`, `t=<span_id>`).
- A **minimap** along the bottom shows event density per second and lets the user jump by clicking.
- `f` toggles follow mode for live recordings.

### 6.7 Filtering

`/` opens a filter overlay with a small query language:

```
unit=grafana/0 kind=hook severity>=warning
relation=grafana-source
text~"connection refused"
```

Filters compile to `WHERE` fragments; the timeline pane rerenders live as the user types (debounced 100 ms).

### 6.8 Themes and accessibility

`lipgloss` styles are grouped into a `Theme` struct with a `--theme` flag (`dark`, `light`, `high-contrast`). Colours degrade gracefully on 16-colour terminals. No emoji in default theme; icons come from Nerd Font glyphs with ASCII fallback.

## 7. Correlation model

The viewer's mental model:

- **Trace** is a causal chain rooted in some external stimulus (a `juju config`, a machine coming up, a `relation-changed` on another unit).
- **Span** is a unit of work, either on the controller (facade call) or the agent (uniter operation, hook, jujuc RPC).
- **Snapshot** is a state observation attached to the span that produced it.
- **Log record** is a text observation attached to either a span (when `trace_id`/`span_id` are present) or to a time window on a unit.

The offline join is:

- Span↔span by `trace_id` / `parent_span_id`.
- Log↔span by `span_id` (exact) or by `(unit, time-window)` (fuzzy fallback).
- Snapshot↔span by `producing_span_id`.
- Snapshot↔time by `scope` and `ts` (viewer picks latest `ts ≤ selection`).

## 8. Non-obvious design decisions

1. **Time authority.** We treat controller wall-clock (from OTEL span timestamps) as canonical. Log sources with their own clock (kubelet, journald) store both the original timestamp and an *adjusted* timestamp computed from an offset the recorder learns by comparing overlapping events. Offsets are stored per source in `manifest.json`.
2. **Gap tolerance.** OTLP over gRPC can drop on reconnect. Each receiver stream gets a sequence number; gaps are recorded as `inventory` rows of kind `gap` and shown as red bands on the timeline so users know the recording is incomplete during those intervals.
3. **PII redaction is on by default** with a conservative regex. `--redact-off` requires an explicit flag.
4. **Recording immutability.** After `record` exits, the recording is treated as immutable. If the user runs `record` again they get a new directory that may reference the old one via `manifest.json.parent`.
5. **Version drift.** `manifest.json` records `juju.version` and `juju.schema.version` per model. Attribute extractors in the recorder are versioned (`extractors/uniter/v3_6.go`, `extractors/uniter/v4_0.go`) and selected per model.
6. **Multi-model, multi-controller.** All storage is keyed by `(controller, model)`. The viewer can open one recording at a time but sees all models within it; a future `juju-lens merge` may combine recordings across controllers.
7. **Deterministic test corpus.** `juju-lens synth <scenario>` produces a fully-formed recording from a YAML scenario file, used both for viewer development and regression tests.
8. **The recorder must be safe to run in production.** It only *reads* from the controller and *sets* documented public config keys. It never touches Dqlite, never modifies application/relation state, never installs agents. On shutdown it restores the exact prior OTEL config.

## 9. Milestones

Each milestone ends with a working, useful tool.

1. **M1 — Skeleton** *(≈2 days)*
   - `record` starts an OTLP gRPC receiver, writes `raw/otlp/traces-*.jsonl`, writes `manifest.json`.
   - `view` opens a recording, lists spans in a scrollable table.
   - `synth trivial` emits a fake recording for iteration.
2. **M2 — Index + basic layout** *(≈1 week)*
   - SQLite indexer with the schema above.
   - bubbletea skeleton with the three-column layout: apps sidebar (from spans), timeline (event list), details pane (span attrs).
   - Model picker if the recording has >1 model.
   - **Status pane (right column)** with two independent sections: **Applications** (leader-set app status per application) and **Units** (per-unit status). Latest-known values only; recomputes on selection change from the snapshot store. Sets up the plumbing (`app-status:*`, `unit-status:*` snapshot scopes; extractors) that M4 fills with real data derived from spans.
   - **Recorder lifecycle**: auto-configure the controller's `open-telemetry-*` keys via the `juju` CLI on start and restore them on clean shutdown (opt-out with `--no-set-otel`); write a `recorder.pid` file; add `juju-lens stop <recording>` so background recorders can be signalled without shell job control.
3. **M3 — Log ingest** *(≈3 days)*
   - `juju debug-log --tail` ingester with `trace_id`/`span_id` extraction.
   - Details pane shows correlated logs for the selected event.
4. **M4 — Snapshots + relations pane** *(≈1 week)*
   - Databag/state snapshotter driven by `CommitHookChanges` and friends.
   - Relations sidebar with expandable databag history and diff mode.
   - Upgrade the Status pane from "latest known" (M2) to **true point-in-time**: press `s` on any timeline event to see application and unit statuses exactly as they would have appeared in `juju status` at that instant, walking backward from the cursor to the nearest snapshot per scope.
5. **M5 — Follow mode** *(≈2 days)*
   - `view --follow` for live recordings.
   - Switch the `record` indexer from post-hoc to incremental so `view --follow` can tail growing recordings.
6. **M6 — K8s + machine log ingesters** *(≈1 week)*
   - Kubernetes shared informer per model, dynamic pod log followers.
   - `juju ssh` + `journalctl -f -o json` ingester with dynamic host discovery.
7. **M7 — Polish** *(open-ended)*
   - Filter overlay, time-jump, split view, mouse support, themes, help overlay.
   - `verify`, `export`, `index` subcommands.
   - Documentation, demo GIFs, packaging as a snap.

## 10. Libraries

| Concern | Library |
|---|---|
| CLI subcommands | `github.com/spf13/cobra` |
| Config / flags | `github.com/spf13/viper` (optional) |
| OTLP receiver | `go.opentelemetry.io/collector/receiver/otlpreceiver` |
| OTLP protos | `go.opentelemetry.io/proto/otlp/...` |
| SQLite | `modernc.org/sqlite` (pure Go, no CGo) |
| TUI | `github.com/charmbracelet/bubbletea` + `bubbles` + `lipgloss` + `bubblezone` |
| Juju API | `github.com/juju/juju/api` (for `Status`, `WatchDebugLog`, `WatchActionsProgress`) |
| Kubernetes | `k8s.io/client-go` (informers + pod log streams) |
| SSH (machine hosts) | `golang.org/x/crypto/ssh` |
| systemd journal (optional native) | `github.com/coreos/go-systemd/v22/sdjournal` |
| JSON diff | `github.com/wI2L/jsondiff` |
| Structured logging | `log/slog` (stdlib) |
| Test doubles / snapshots | `github.com/hexops/autogold` |

## 11. Explicit non-goals for v1

- **Not a monitoring tool.** No alerting, no dashboards on live data — that's what COS + Grafana is for. `juju-lens` is a debugger's time machine.
- **Not a charm profiler.** We don't instrument charm Python code. Charm-side visibility comes from Juju's spans around hook execution and from logs.
- **Not a controller replacement.** We never impose configuration beyond the six OTEL keys, and only when the user asks.
- **Not a remote UI.** Viewer is local TUI over a local recording. Sharing means shipping the recording directory to another machine.

## 12. Open questions

- Should the recorder tolerate multiple concurrent instances against the same controller? (Probably yes — OTLP fan-out, but only one may set/restore OTEL config.)
- What is the right story for cross-model relations (offers)? Two recordings likely, joined in the viewer.
- Do we want a small web UI (localhost) as an alternative to the TUI? Probably not for v1, but the SQLite index makes it trivial to bolt on later.
- How aggressively should the reconciler run `Status()`? Every 1 s is cheap; every span-derived hint is cheaper.
