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

### 2.1 Primary source: eBPF uprobes on the Juju API TLS boundary (both Juju 3.6 and 4.x)

Every `jujud` process — controller *and* unit agents, machine *and* k8s — talks to peers over TLS via Go's `crypto/tls` package. We attach eBPF uprobes to `crypto/tls.(*Conn).Write` and `crypto/tls.(*Conn).Read` in the target process and capture bytes just before encryption and just after decryption. The result is the full, verbatim Juju API stream in plaintext, without touching Juju itself, its configuration, or its running state.

The wire format on the other side of TLS is Juju's own JSON-over-websocket RPC codec (`rpc/jsoncodec/codec.go`). Every message is a self-describing envelope:

```go
type inMsgV1 struct {
    RequestId  uint64          `json:"request-id"`
    Type       string          `json:"type"`       // facade name, e.g. "Uniter"
    Version    int             `json:"version"`
    Id         string          `json:"id"`
    Request    string          `json:"request"`    // method name, e.g. "CommitHookChanges"
    Params     json.RawMessage `json:"params"`     // full arguments
    Error      string          `json:"error"`
    ErrorCode  string          `json:"error-code"`
    Response   json.RawMessage `json:"response"`
    TraceID    string          `json:"trace-id"`   // populated when tracing is on upstream
    SpanID     string          `json:"span-id"`
    TraceFlags int             `json:"trace-flags"`
}
```

`request-id` pairs each request with its response, so we synthesise a span per RPC (start = write timestamp, end = matching read timestamp, name = `<facade>.<method>`). `trace-id`/`span-id` propagate the caller's causal context when the upstream has tracing configured; when they are empty we fall back to per-process request-id chains.

Why this works on both 3.6 and 4.x:

- We depend only on `crypto/tls` (stdlib) and on the JSON wire format, both unchanged across Juju minor versions.
- No controller config is read, set, or restored. The recorder is invisible to Juju.
- The same technique attaches to the machine controller's `jujud`, the k8s controller pod's `jujud`, and the k8s unit sidecar's `containeragent` — all three are the same build (see `caas/Dockerfile`), so probe attachment code is identical.

Symbol resolution on stripped release binaries uses `.gopclntab` (present in every Go binary, parseable with `debug/gosym`) — the technique demonstrated by `gojue/ecapture` for stripped Go TLS probes. Per-Go-version argument register layouts are tracked in a small table maintained by the recorder; one row per Juju Go toolchain bump.

Operational requirements: Linux kernel ≥ 5.8 on the host running the target process, and `CAP_BPF` (root, or the specific capability) on the attaching process. The technique is not k8s-specific — it works anywhere Linux does — but on k8s the standard shape is either `kubectl debug node/<n> --profile=sysadmin` for one-shot recordings or a privileged DaemonSet for long-lived setups (see §4.1).

Prior art we lean on: `gojue/ecapture` (Apache 2.0) is the reference implementation of Go-TLS uprobes on stripped release binaries; Pixie's `gotls` probes are structurally identical. Neither is a hard dependency — the ~200 lines of BPF C that matter are readable and vendorable.

### 2.2 Why not just talk to the Juju API?

We considered building this on the AllWatcher / WatchAll model-events stream and rejected it — it is broken on both target versions:

1. **Juju 4.x removed it.** `apiserver/facades/client/client/client.go:75` and `apiserver/facades/client/controller/controller.go:488` return `errors.NotImplementedf("WatchAll")` / `"WatchAllModels"`. The release notes describe a replacement "lighter event API" but it isn't implemented in main yet. On 4.x the only client-facing streams left are `WatchDebugLog` and `WatchActionsProgress`.
2. **Even on 3.6, AllWatcher does not stream relation databag contents.** Databags live behind the Uniter facade which authenticates unit agents, not admin clients. `juju show-unit` is a one-shot inspection, not a stream.

Uprobes are the primary source. The Juju API is used only for the things it does well and works on both versions:

- `Status()` once on connect and on RPC-derived model-change signals — to bootstrap the app/unit/machine inventory
- `WatchDebugLog` for the model-wide Juju log stream
- `WatchActionsProgress` for action progress
- Kubernetes / systemd APIs for underlying container/host logs

### 2.3 What we derive from captured RPCs

Each captured RPC carries the full JSON `params` and `response` verbatim. The recorder classifies calls by `(type, request)` — the facade + method — and extracts state where relevant. Timing and causality come from `request-id` pairing (start = write, end = matching read) and from `trace-id`/`span-id` when the caller populates them.

| Signal | Source RPC | What we snapshot |
|---|---|---|
| Hook fired | `Uniter.CommitHookChanges` + surrounding `Uniter.Set*Status` calls | hook kind, relation, remote unit, remote app, storage id, secret URI, exit code, duration |
| Databag write | `Uniter.CommitHookChanges` (`RelationUnitSettings`, `RelationApplicationSettings`) | per-relation, per-unit and per-app databag after each hook |
| Relation lifecycle | `Uniter.EnterScope`, `LeaveScope`, `SetRelationStatus` | membership, suspended, status |
| Unit / app status | Uniter status setters | workload/agent/app status + message |
| Unit stored state | `Uniter.SetState` (server side of `state-set`) | KV blob per unit |
| Secrets | `SecretsManager.*` | metadata, rotation policy, grants, revisions, obsolete revisions |
| Open ports | `Uniter.OpenPorts` etc. | per-endpoint port ranges |
| Leadership | leadership facade | leader changes, claim expiry |
| Charm URL / mod version | uniter facade | charm upgrades |
| Actions | action facade + `WatchActionsProgress` | id, name, params, status, results, messages |
| Model config | model config facade | key-value changes |
| jujuc RPCs (intra-hook) | direct uprobe on `jujuc.(*Jujuc).Main` (see §5.1) | every hook tool the charm invoked, in order |

For each write we compute a JSON diff against the previous snapshot for the same scope, hash the content, and only persist a new snapshot when the hash changes. The viewer never re-derives state; it queries the snapshots directly.

### 2.4 Logs (correlated but treated as a separate stream)

For every recording we pick up log sources automatically as the model evolves:

- **`juju debug-log --tail --format json`** — always on, one process per model. Gives every agent's and every charm's log line in a single stream.
- **Kubernetes pod logs** (CAAS controllers) — a shared-informer watches Pods in the model's namespace, keyed by Juju labels (`juju.io/model-uuid`, `juju.io/unit`). Every new container gets a follower goroutine that calls `CoreV1().Pods().GetLogs(pod, {Follow: true, Container: c, Timestamps: true})`. Deleted pods stop cleanly.
- **`journalctl -f -o json -u <unit>`** (machine controllers) — one SSH session per host discovered via `juju ssh`, filtered to `jujud-machine-*.service`, `jujud-unit-*.service`, and `snap.juju.*.service`. When new machines appear in `Status()`, new sessions are opened.
- **`snap logs -f <snap>`** where relevant.

Logs are normalised to a common record shape and, when Juju emits `trace-id`/`span-id` as slog attributes on a log line, those become first-class join keys against the RPC store (the same IDs propagate in the `trace-id`/`span-id` fields of the JSON envelope we already capture on the wire).

Log ingesters start/stop dynamically as the recorder learns about new units/pods/machines from status deltas and RPC-derived hints. Nothing is hard-coded per recording.

### 2.5 What we intentionally do not capture (v1)

- **Continuous profiling** (pprof). Documented as future work; the eBPF attacher already has the target processes' PIDs so pprof spans would be a small extension.
- **Metrics.** Not needed for the timeline; may be added later as an at-a-glance density indicator.
- **Charm source code.** The viewer references file:line from logs and RPC error payloads; it does not embed sources.

## 3. On-disk format

A recording is a **directory**. Everything the tool wrote is human-inspectable text; the SQLite index is a build artifact that the viewer opens read-only.

```
recordings/2026-07-03T14-30-12--mycontroller/
├── manifest.json                        # controller, models, versions, sources enabled, clock offsets
├── index.db                             # SQLite (WAL); rebuildable from raw/
├── raw/
│   ├── rpc/
│   │   └── default/                      # per-model directory (model name)
│   │       └── calls-2026-07-03T14.jsonl # captured Juju API RPCs, one message per line
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
- **`manifest.json`** captures everything the viewer needs to interpret the data: controller name and UUID, models observed, Juju/agent versions per model, controller version, source list with per-source status, clock offsets between sources, recording start/end, and the attacher deployment mode used (see §4.1).
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
  name TEXT,                          -- '<facade>.<method>' for RPCs, 'jujuc.<tool>' for hook tools
  service TEXT,                       -- 'apiserver','uniter','containeragent','jujuc'
  attrs_json TEXT, status TEXT,
  raw_offset INTEGER, raw_file TEXT   -- pointer back to raw/rpc/*.jsonl for the full envelope
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

The `spans` table stores synthesised spans, one per captured RPC. A span's `trace_id` and `parent_span_id` come from the wire envelope's `trace-id` fields when present; otherwise they are synthesised from the `request-id` chain within each process. This keeps the viewer's join model (§7) unchanged whether or not any upstream tracing is configured.

## 4. CLI surface

```
juju-lens record <controller> [flags]
    --model <name>            # repeatable; default: all models
    --output <dir>            # default: ./recordings/<ts>--<controller>
    --attach <mode>           # ssh | kubectl-debug | daemonset (default: auto-detect from controller kind)
    --max-duration <dur>      # e.g. 30m, 2h
    --max-size <size>         # e.g. 500M, 2G
    --redact <pattern,...>    # additional key patterns to redact in databags/secrets
    --sources rpc,debug-log,k8s,journal,snap  # opt out of specific sources

juju-lens view <recording> [flags]
    --model <name>            # focus on a single model at startup
    --follow                  # tail an in-progress recording

juju-lens index <recording>   # rebuild index.db from raw/
juju-lens verify <recording>  # sanity-check integrity, list gaps
juju-lens export <recording> [--format=json]  # for external analysis
juju-lens synth <scenario>    # generate a synthetic recording for viewer development
```

### 4.1 `record` lifecycle

1. Parse `~/.local/share/juju` (or `JUJU_DATA`) to reach the controller with existing credentials.
2. `Status()` for each selected model → seed inventory.
3. Choose an attach mode based on the controller kind (or `--attach`):
   - **machine controllers**: open one `juju ssh` session per host holding a `jujud`/`containeragent` PID of interest and `scp` a small static Go attacher binary (`juju-lens-probe`, ~a few MB, built with `cilium/ebpf`) into a tmp path.
   - **k8s controllers**: for each node hosting a controller pod or unit sidecar, launch a `kubectl debug node/<n> --profile=sysadmin --image=juju-lens-probe` ephemeral pod. For long-lived recordings, `--attach daemonset` applies the same image as a privileged DaemonSet on the model's nodes and cleans it up on exit.
4. On each attach target the probe: (a) enumerates candidate PIDs (`jujud`, `containeragent`), (b) resolves `crypto/tls.(*Conn).Write`/`.Read` (plus optionally `jujuc.(*Jujuc).Main` on the same process) by reading `.gopclntab` from `/proc/<pid>/root/<binary>`, (c) attaches the uprobes, (d) streams captured `(ts, pid, direction, plaintext)` frames back over stdout/stdin as length-prefixed JSON.
5. The recorder demultiplexes those streams into per-model `raw/rpc/<model>/calls-*.jsonl` files, pairing writes to their matching reads by `request-id`, and emits synthesised span records to the indexer.
6. For each selected model, start log ingesters based on source availability.
7. A background reconciler watches `Status()` deltas (via facade watchers where available, or driven by newly-seen `juju.unit`/`juju.model` values in captured RPCs on 4.x) and adds/removes attach targets and log ingesters as units/pods/machines appear and disappear.
8. Write raw payloads as they arrive; a separate indexer goroutine feeds the SQLite index.
9. On `SIGINT`/`SIGTERM`/max-cap: detach all uprobes, stop probe subprocesses, terminate any ephemeral debug pods / DaemonSets, flush indexer, finalize `manifest.json`.

### 4.2 `view` lifecycle

1. Open `index.db` read-only, load `manifest.json`.
2. Render TUI with focused model (or model picker if multiple exist).
3. All navigation is SQL-backed with prepared statements; the viewer never reads raw files unless the user requests raw log context.
4. If `--follow`, poll `MAX(ts)` from each table every 250 ms and advance the timeline.

## 5. Recorder internals

### 5.1 eBPF attacher (`juju-lens-probe`)

A small Go binary that runs on the host containing the target process (via SSH on machine controllers; via `kubectl debug node` or a privileged DaemonSet on k8s). Uses `github.com/cilium/ebpf` for probe management — pure Go, no libbpf/BCC runtime dependency.

For each PID of interest:

1. Read the target ELF at `/proc/<pid>/root/<binary>` (works uniformly for the machine-controller `jujud`, the k8s controller pod's `jujud`, and the k8s unit sidecar's `containeragent`, since all three are the same build).
2. Locate `.gopclntab` (falling back to `.data.rel.ro.gopclntab` for PIE builds and to a magic-number scan of `.data.rel.ro` for aggressively stripped builds — the same fallback chain used by `gojue/ecapture`).
3. Parse the pclntab with `debug/gosym` and resolve the entry addresses of `crypto/tls.(*Conn).Write`, `crypto/tls.(*Conn).Read`, and — on machine unit agents and the k8s unit sidecar — `github.com/juju/juju/internal/worker/uniter/runner/jujuc.(*Jujuc).Main`.
4. Attach entry and return uprobes; the entry probe stashes the buffer pointer keyed by `(tgid, goroutine-id)`, the return probe reads the actual byte count and emits a `(ts, pid, direction, bytes)` event through a `perf_event_array` or `ringbuf` map.
5. Userspace strips websocket framing (RFC 6455), reassembles fragmented frames, and JSON-decodes the resulting envelope into the `inMsgV1` shape described in §2.1.

Go-version-specific argument register layouts are kept in a small `map[goVersion]symOffsets` table maintained by the recorder. Currently one row per Juju Go toolchain (Go 1.24, 1.25); each new juju/juju release that bumps Go adds one row after a five-minute DWARF inspection of the new toolchain.

On detach the probes are removed cleanly; the target process is unaffected end-to-end (this is the same operational profile as Pixie's Stirling and Grafana Beyla in production).

### 5.2 RPC ingest and pairing

Captured writes and reads arrive from the attacher as an interleaved stream. The ingester:

1. Reassembles websocket frames per `(pid, conn-id)` where `conn-id` is derived from the `*Conn` pointer captured at probe time.
2. JSON-decodes each complete message into the `inMsgV1` envelope.
3. Buckets requests by `(pid, request-id)`; when the matching response arrives, emits a synthesised span with `name = "<type>.<request>"`, `service` derived from the source binary, `ts_start` = write timestamp, `ts_end` = read timestamp, and the full envelope written to `raw/rpc/<model>/calls-*.jsonl`.
4. If the wire envelope carries `trace-id`/`span-id`, those become the span's IDs; otherwise IDs are synthesised deterministically from `(pid, request-id)`.
5. Emits the raw envelope offset plus the synthesised span to the indexer channel.

### 5.3 Indexer

A single goroutine drains channels from every source (RPC ingest + log ingesters + Juju API events) into SQLite, batched by 100 records or 200 ms, whichever comes first. WAL mode; `synchronous=NORMAL`. The DB write is fire-and-forget from the ingesters' perspective; if the indexer falls behind, ingesters continue to write raw files and the DB catches up.

### 5.4 Model / inventory reconciler

Because Juju 4.x has no client-facing model watcher, we drive inventory reconciliation from two sources:

- On connect: `Status()` for a full snapshot.
- Ongoing: whenever a captured RPC carries a new unit tag, model UUID, or machine id we haven't seen, or when `debug-log` mentions one, the reconciler runs a targeted `Status()` for that model.

This is not polling — the reconciler runs *because* something changed, not on a timer. On 3.6 we optionally also open `WatchAll` and use its deltas as an extra signal source.

### 5.5 Databag reconstruction

Databag before/after per hook is reconstructed entirely from the RPC stream:

1. Match a `Uniter.CommitHookChanges` RPC to the hook it terminates — the request body itself identifies the unit, the relation, and the settings written.
2. Maintain an in-memory per-scope current-value map, updated on every commit.
3. Diff against the map. If different: insert a `snapshots` row with `producing_span_id` set to the synthesised RPC span for the commit, and update the map.
4. When we cannot see a scope's initial value (recorder started mid-life), we mark it `initial=false` in the snapshot; the viewer draws that snapshot with a "state at recording start" chevron so users know it wasn't a change event.

### 5.6 Log ingesters

Each ingester is a `type Ingester interface { Run(ctx) error; Stop() }` implementation. The reconciler owns a map of `key → Ingester` and starts/stops them based on inventory diffs. Ingesters write to their own file under `raw/` and emit `LogRecord` messages to the indexer channel.

Trace/span correlation for logs:

- Juju's `debug-log --format=json` includes structured fields — when a log line was produced inside a spanned context, `trace-id` and `span-id` are present. The ingester copies them into the log record.
- For k8s/journal/snap logs, we usually don't have trace ids. The viewer falls back to time-window + unit-name matching for those.

### 5.7 Redaction

`--redact` accepts a comma-separated list of glob patterns applied to databag keys, secret content keys, and log JSON fields. Defaults always redact:

- `*token*`, `*password*`, `*secret*`, `*key*` (case-insensitive) in databag/secret values
- `secret-content-*` fields in captured RPC payloads

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
- **Span** is a synthesised unit of work — one per captured Juju API RPC, plus one per jujuc invocation on the unit side. Start/end come from the write/read timestamps captured at the TLS boundary.
- **Snapshot** is a state observation extracted from a write-side RPC payload (`CommitHookChanges`, `SetStatus`, `SetState`, `SecretsManager.*`, …).
- **Log record** is a text observation attached to either a span (when `trace-id`/`span-id` are present) or to a time window on a unit.

The offline join is:

- Span↔span by `trace_id` / `parent_span_id` when the wire envelope carries them, or by `(pid, request-id)` chains when it doesn't.
- Log↔span by `span_id` (exact) or by `(unit, time-window)` (fuzzy fallback).
- Snapshot↔span by `producing_span_id`.
- Snapshot↔time by `scope` and `ts` (viewer picks latest `ts ≤ selection`).

## 8. Non-obvious design decisions

1. **Time authority.** We treat the eBPF timestamp (nanoseconds since boot, converted to wall-clock via a boot-time offset read once per attach) as canonical for RPC-derived spans. Log sources with their own clock (kubelet, journald) store both the original timestamp and an *adjusted* timestamp computed from an offset the recorder learns by comparing overlapping events. Offsets are stored per source in `manifest.json`.
2. **Gap tolerance.** eBPF ring buffers can drop records under extreme load. The attacher reports drop counts per interval; drops are recorded as `inventory` rows of kind `gap` and shown as red bands on the timeline so users know the recording is incomplete during those intervals.
3. **PII redaction is on by default** with a conservative regex. `--redact-off` requires an explicit flag.
4. **Recording immutability.** After `record` exits, the recording is treated as immutable. If the user runs `record` again they get a new directory that may reference the old one via `manifest.json.parent`.
5. **Version drift.** `manifest.json` records `juju.version` and `juju.schema.version` per model, and the Go toolchain version of each attached binary. Attribute extractors in the recorder are versioned (`extractors/uniter/v3_6.go`, `extractors/uniter/v4_0.go`) and selected per model. Uprobe register-layout offsets are keyed by Go toolchain version.
6. **Multi-model, multi-controller.** All storage is keyed by `(controller, model)`. The viewer can open one recording at a time but sees all models within it; a future `juju-lens merge` may combine recordings across controllers.
7. **Deterministic test corpus.** `juju-lens synth <scenario>` produces a fully-formed recording from a YAML scenario file, used both for viewer development and regression tests.
8. **The recorder never mutates the target.** It attaches read-only uprobes at the TLS boundary; it never sets controller config, never touches Dqlite, never modifies application/relation state, never installs agents. Detach is clean. This is the same operational profile as production continuous-profiling agents.
9. **Kernel and privilege requirements are explicit.** Recording needs Linux ≥ 5.8 on the host running the target process, and `CAP_BPF` on the attacher. `juju-lens record` checks both up front and refuses with a clear message rather than partially attaching.

## 9. Milestones

Each milestone ends with a working, useful tool.

1. **M1 — Skeleton** *(≈2 days)*
   - Standalone `juju-lens-probe` binary: attaches `crypto/tls.(*Conn).Write`/`.Read` uprobes to a given PID, emits length-prefixed `(ts, pid, dir, bytes)` JSON events on stdout. Verified against the Juju 3.6 snap's `jujud` locally.
   - `record` invokes the probe against a machine controller over `juju ssh`, writes `raw/rpc/*.jsonl` and `manifest.json`.
   - `view` opens a recording, lists synthesised spans in a scrollable table.
   - `synth trivial` emits a fake recording for iteration.
2. **M2 — Index + basic layout** *(≈1 week)*
   - Websocket-frame reassembly + JSON envelope decoder + `request-id` pairing → synthesised spans with `<facade>.<method>` names.
   - SQLite indexer with the schema above.
   - bubbletea skeleton with the three-column layout: apps sidebar (from RPC-derived unit tags), timeline (event list), details pane (RPC envelope + timing).
   - Model picker if the recording has >1 model.
   - **Status pane (right column)** with two independent sections: **Applications** (leader-set app status per application) and **Units** (per-unit status). Latest-known values only; recomputes on selection change from the snapshot store. Sets up the plumbing (`app-status:*`, `unit-status:*` snapshot scopes; extractors) that M4 fills with real data derived from captured RPCs.
   - **Recorder lifecycle**: SSH-based attach on machine controllers with clean detach on shutdown; write a `recorder.pid` file; add `juju-lens stop <recording>` so background recorders can be signalled without shell job control.
3. **M3 — Log ingest** *(≈3 days)*
   - `juju debug-log --tail` ingester with `trace-id`/`span-id` extraction.
   - Details pane shows correlated logs for the selected event.
4. **M4 — Snapshots + relations pane** *(≈1 week)*
   - Databag/state snapshotter driven by `Uniter.CommitHookChanges` and friends.
   - Relations sidebar with expandable databag history and diff mode.
   - Upgrade the Status pane from "latest known" (M2) to **true point-in-time**: press `s` on any timeline event to see application and unit statuses exactly as they would have appeared in `juju status` at that instant, walking backward from the cursor to the nearest snapshot per scope.
5. **M5 — Follow mode** *(≈2 days)*
   - `view --follow` for live recordings.
   - Switch the `record` indexer from post-hoc to incremental so `view --follow` can tail growing recordings.
6. **M6 — K8s/machine log ingesters** *(≈3 days)*
   - K8s workload-log ingester: `kubectl logs -f --timestamps` per workload container (the charm sidecar is skipped — debug-log already covers the container-agent), with dynamic pod/model discovery, written under `raw/k8s/<model>/<pod>/<container>/`.
   - Machine journald ingester: `juju ssh … journalctl -f -o json` per machine with dynamic host/model discovery, written under `raw/machine/<model>/<host>/`.
   - Both share the log-record model and correlate to spans exactly like debug-log; each reconciles on a ticker and self-heals dropped streams.
   - Out of scope (deferred): eBPF **attach** on Kubernetes — `--attach kubectl-debug`/`--attach daemonset` modes and a shared informer for probe placement. The RPC probe already attaches to `containeragent` where it runs; only these CAAS/sidecar staging modes are deferred, since they need a probe container image and registry to test.
7. **M7 — Polish** *(open-ended)*
   - Filter overlay, time-jump, split view, mouse support, themes, help overlay.
   - `verify`, `export`, `index` subcommands.
   - Documentation, demo GIFs, packaging as a snap.

## 10. Libraries

| Concern | Library |
|---|---|
| CLI subcommands | `github.com/spf13/cobra` |
| Config / flags | `github.com/spf13/viper` (optional) |
| eBPF | `github.com/cilium/ebpf` (pure Go, no libbpf/BCC runtime dep) |
| Go binary symbols | `debug/elf` + `debug/gosym` (stdlib) |
| Reference impl for Go-TLS uprobes | `github.com/gojue/ecapture` (Apache 2.0, vendorable) |
| Websocket frame decode | `github.com/gorilla/websocket` (same lib Juju uses on the wire) |
| SQLite | `modernc.org/sqlite` (pure Go, no CGo) |
| TUI | `github.com/charmbracelet/bubbletea` + `bubbles` + `lipgloss` + `bubblezone` |
| Juju API | `github.com/juju/juju/api` (for `Status`, `WatchDebugLog`, `WatchActionsProgress`) |
| Kubernetes | `k8s.io/client-go` (informers + pod log streams + `kubectl debug node` orchestration) |
| SSH (machine hosts) | `golang.org/x/crypto/ssh` |
| systemd journal (optional native) | `github.com/coreos/go-systemd/v22/sdjournal` |
| JSON diff | `github.com/wI2L/jsondiff` |
| Structured logging | `log/slog` (stdlib) |
| Test doubles / snapshots | `github.com/hexops/autogold` |

## 11. Explicit non-goals for v1

- **Not a monitoring tool.** No alerting, no dashboards on live data — that's what COS + Grafana is for. `juju-lens` is a debugger's time machine.
- **Not a charm profiler.** We don't instrument charm Python code. Charm-side visibility comes from captured jujuc RPCs and from logs.
- **Not a controller replacement.** We never impose configuration on Juju itself, and we never modify state; the only side effect is attaching read-only eBPF probes to running processes.
- **Not a remote UI.** Viewer is local TUI over a local recording. Sharing means shipping the recording directory to another machine.

## 12. Open questions

- Should the recorder tolerate multiple concurrent instances against the same controller? (Probably yes — each attaches its own uprobes and writes its own recording; the kernel handles concurrent attachers on the same PID cleanly.)
- What is the right story for cross-model relations (offers)? Two recordings likely, joined in the viewer.
- Do we want a small web UI (localhost) as an alternative to the TUI? Probably not for v1, but the SQLite index makes it trivial to bolt on later.
- Does Juju negotiate websocket `permessage-deflate` compression on the API socket? If so, decode after userspace zlib inflation; needs one-time confirmation against a live controller.
- How aggressively should the reconciler run `Status()`? Every 1 s is cheap; every RPC-derived hint is cheaper.
