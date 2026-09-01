# SQLite index schema

`index.db` is a SQLite database in WAL mode, rebuildable at any time from a
recording's `raw/` tree with `juju-lens index`. The viewer only ever reads
it through prepared statements; it never touches `raw/` directly. This page
lists the tables as of schema version 4. For what feeds them, see [from
wire to viewer](../explanation/from-wire-to-viewer.md).

## `models`

One row per Juju model in the recording.

| Column | Meaning |
|---|---|
| `id` | Primary key, referenced by every other table. |
| `name` | Model name. |
| `uuid` | Model UUID, when known. |

## `spans`

One row per synthesised RPC span — the unit of work behind every hook,
status change, and relation action.

| Column | Meaning |
|---|---|
| `span_id` | Primary key. |
| `trace_id`, `parent_span_id` | Causal chain this span belongs to. |
| `model_id` | Which model. |
| `ts_start`, `ts_end` | Write/read timestamps (unix nanoseconds). |
| `name` | `<facade>.<method>`, e.g. `Uniter.CommitHookChanges`. |
| `service` | Which process emitted it. |
| `unit`, `app` | The unit this span belongs to, and its application. |
| `hook` | The hook this span ran inside, if any. |
| `relation` | The relation this span concerns, if any. |
| `status`, `status_msg` | Status this span set, if any. |
| `raw_file`, `raw_offset` | Pointer back to the exact line in `raw/rpc/*.jsonl`. |
| `attrs_json` | The full envelope, as captured. |

## `snapshots`

Point-in-time state observations — application/unit status and databags —
extracted from spans.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `ts` | Which model, and when this snapshot was taken. |
| `kind` | `app-status`, `unit-status`, `databag`, … |
| `scope` | Canonical id, e.g. `unit-status:grafana/0`. |
| `body_json` | The snapshot's content. |
| `content_hash` | Hash of `body_json`; deduplicates no-op re-writes. |
| `origin` | `rpc` (derived from captured traffic) or `bootstrap` (seeded from `juju status` at recording start). |
| `producing_span_id` | The span that produced this snapshot, if any. |

The viewer reconstructs "state as of this instant" by querying, per scope,
the latest snapshot with `ts <= selection`.

## `log_records`

One row per ingested log line, from any source.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `ts` | Which model, and when. |
| `source` | `debug-log`, `k8s`, or `journal`. |
| `entity`, `unit` | The raw Juju entity tag, and the unit it resolves to. |
| `level`, `module` | Log level and module. |
| `body` | The log line's text. |
| `trace_id`, `span_id` | Present when the log source carried them, letting a line join exactly to the span that caused it. |
| `raw_file`, `raw_line` | Pointer back to the source file. |

## `meta`

A small key/value table for schema bookkeeping.
