# How to export a recording for an AI agent

Use this guide to export a recording as a folder of plain-text files that an AI agent can read a piece at a time, without querying SQLite. The export includes the hooks, their outcomes, the state they changed, and the logs, so the agent doesn't have to work them out from raw RPCs.

## Prerequisites

- A recording with an up-to-date index. If you're not sure, [rebuild the index](rebuild-the-index.md).

## Export a recording

Pass the recording directory:

```bash
juju-lens export ./recordings/my-capture
```

The files are written to `derived/export/` in the recording directory. Use `--out` to choose a different directory.

| File | Contents |
|---|---|
| `AGENTS.md` | Instructions for the agent, describing the other files. |
| `SUMMARY.md` | Time range, units, and how many hooks ended each way. |
| `timeline.jsonl` | Events and log lines in time order, one JSON object per line. |
| `events.jsonl` | Events only. |
| `details/<span_id>.json` | One file per event: the statuses it set, relation data and configuration changes, and failure details. |
| `report.md` | Everything above as a single document for people to read. |

Point your agent at `AGENTS.md`.

To write only some of the files, list them with `--parts`. The parts are `summary`, `timeline`, `events`, `details`, and `report`; `AGENTS.md` is always written.

```bash
juju-lens export ./recordings/my-capture --parts summary,report
```

To write one JSON or Markdown document to standard output instead of a folder, use `--format`:

```bash
juju-lens export ./recordings/my-capture --format json > capture.json
juju-lens export ./recordings/my-capture --format md > capture.md
```

## Limit the export

To export one model, some units, or a time range:

```bash
juju-lens export ./recordings/my-capture \
    --model cos \
    --unit grafana/0 \
    --since 2026-08-20T14:00:00Z \
    --until 2026-08-20T15:00:00Z
```

`--model` is required when the recording has more than one model. `--unit` takes a unit or an application name, and can be repeated.

To export only the hooks that went wrong, with the hook before and after each one on the same unit:

```bash
juju-lens export ./recordings/my-capture --errors-only
```

This includes hooks that errored, were retried, or had an RPC error. [From RPCs to hooks](../explanation/rpcs-to-hooks.md#how-a-hook-run-ended) describes each outcome.

For every flag, see the [command line reference](../reference/cli.md#export).
