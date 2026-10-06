# SQLite index schema

The tables in `index.db`, as of schema version 4, apart from the internal `meta` table. [From wire to viewer](../explanation/from-wire-to-viewer.md#the-index) describes how they're filled.

## `models`

One row per Juju model in the recording.

| Column | Meaning |
|---|---|
| `id` | Primary key, referenced by every other table. |
| `name` | Model name. |
| `uuid` | Model UUID, when known. |

## `spans`

One row per RPC: a request paired with its response.

| Column | Meaning |
|---|---|
| `span_id` | Primary key. |
| `trace_id`, `parent_span_id` | From Juju's tracing when enabled, otherwise derived from the request. |
| `model_id` | Which model. |
| `ts_start`, `ts_end` | When the request was written and the response read, in Unix nanoseconds. |
| `name` | `<facade>.<method>`, such as `Uniter.CommitHookChanges`. |
| `service` | The agent that made the call. |
| `unit`, `app` | The unit this span belongs to, and its application. |
| `hook` | The hook this span ran inside, if any. |
| `relation` | The relation this span concerns, if any. |
| `status`, `status_msg` | Status this span set, if any. |
| `raw_file`, `raw_offset` | Pointer back to the exact line in `raw/rpc/*.jsonl`. |
| `attrs_json` | The request and response, as captured. |

## `snapshots`

State at a point in time, extracted from spans or from `raw/status/`.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `ts` | Which model, and when this snapshot was taken. |
| `kind` | `app-status`, `unit-status`, `agent-status`, `databag`, `config`, or `leadership`. |
| `scope` | What the snapshot describes, such as `unit-status:grafana/0`. |
| `body_json` | The snapshot's content. |
| `content_hash` | Hash of `body_json`; deduplicates no-op re-writes. |
| `origin` | `rpc` if extracted from a span, `bootstrap` if from `raw/status/`. |
| `producing_span_id` | The span that produced this snapshot, if any. |

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
| `trace_id`, `span_id` | The trace and span the line belongs to, when the log source wrote them. |
| `raw_file`, `raw_line` | Pointer back to the source file. |
