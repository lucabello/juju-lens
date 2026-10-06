# Recording layout

The files and directories in a recording. [From wire to viewer](../explanation/from-wire-to-viewer.md) describes how they're written.

```text
recordings/2026-07-03T14-30-12--my-capture/
├── manifest.json
├── recorder.pid
├── index.db
├── raw/
│   ├── rpc/<model>/calls-<hour>.jsonl
│   ├── juju/<model>/debug-log-<hour>.log
│   ├── k8s/<model>/<pod>/<container>/logs-<hour>.log
│   ├── machine/<model>/<host>/journal-<hour>.log
│   └── status/<model>/
│       ├── bootstrap.json
│       ├── databags.json
│       └── config.json
└── derived/
    └── export/
```

## Top-level files

| Path | Contents |
|---|---|
| `manifest.json` | The `juju-lens` version that wrote the recording; the controller's name, UUID, and version; the models recorded; the attach mode and targets; start and end times; why the recording ended; and the status of each data source. |
| `recorder.pid` | The process ID of the running recorder, used by [`juju-lens stop`](../how-to/stop-a-recording.md). Removed when the recorder exits normally. |
| `index.db` | A SQLite database in WAL mode, built from `raw/`. The viewer reads only this file. See the [SQLite index schema](sqlite-schema.md). |
| `derived/export/` | The default output directory of [`juju-lens export`](../how-to/export-for-an-agent.md). |

## Raw data

Files under `raw/` that grow during a recording start a new file every hour.

| Path | Contents |
|---|---|
| `rpc/` | Captured Juju API messages, one JSON object per line. Requests and responses are separate lines. |
| `juju/` | `juju debug-log` output for each model, as printed. |
| `k8s/` | Workload-container logs from Kubernetes models, one directory per pod and container. |
| `machine/` | The systemd journal of each machine in machine models. |
| `status/<model>/bootstrap.json` | `juju status --format=json`, taken when the recording started. |
| `status/<model>/databags.json` | `juju show-unit --format=json` for each unit, taken when the recording started. |
| `status/<model>/config.json` | `juju config <app> --format=json` for each application, taken when the recording started. |
