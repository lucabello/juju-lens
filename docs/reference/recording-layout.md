# Recording layout

A recording is a plain directory — nothing is compressed, and every file
under `raw/` is either JSON or JSONL, so the recording stays inspectable
with standard tools even if `juju-lens` itself misbehaves. This page lists
what's in one and what each piece is for. For the reasoning behind these
choices, see [from wire to viewer](../explanation/from-wire-to-viewer.md).

```
recordings/2026-07-03T14-30-12--mycontroller/
├── manifest.json      # controller, models, versions, sources, end reason, attach mode
├── recorder.pid       # PID of the running recorder; removed on clean exit
├── index.db           # SQLite (WAL); rebuildable from raw/
├── raw/
│   ├── rpc/<model>/calls-*.jsonl        # captured Juju API RPCs, one per line
│   ├── juju/<model>/debug-log.jsonl     # juju debug-log, one line per record
│   ├── k8s/<model>/<pod>/<container>/*.log  # workload-container logs (CAAS)
│   └── machine/<model>/<host>/journal.jsonl # journald (IAAS)
└── derived/
    └── export/         # written by `juju-lens export`; see its how-to guide
```

## `manifest.json`

The authoritative description of the recording: the tool version that wrote
it, the controller's name/UUID/version, every model observed, the attach
mode and targets used, the recording's start/end time and end reason, and a
per-source status list (what ran, how many records, any error). The viewer
reads this to know what it's looking at; nothing here is inferred from
`raw/`.

## `raw/`

Everything a source captured, verbatim, split into one subdirectory per
source kind and then per model. Files rotate hourly for high-volume
sources. This is the actual source of truth for a recording — `index.db` is
a cache built from it, so it's always safe to delete and rebuild
(`juju-lens index`, see [how to rebuild the index](../how-to/rebuild-the-index.md)).

## `index.db`

A SQLite database (WAL mode) that the viewer queries read-only. See the
[SQLite index schema reference](sqlite-schema.md) for its tables.

## `derived/`

Cached output from tools that process a recording after the fact —
currently just `juju-lens export`'s output folder.
