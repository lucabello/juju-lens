# How to export a recording for an agent

Use this guide to turn a recording into a folder of plain-text artifacts
meant for an AI agent (or a human skimming in a text editor) to read
incrementally, instead of ingesting an entire recording or querying SQLite
directly. The export carries the same hook and failure classification the
TUI shows — whether a unit was actually broken, or errored and recovered —
so a reader doesn't have to re-derive it from raw RPC spans.

## Prerequisites

- A recording with a built index (see [how to rebuild the
  index](rebuild-the-index.md) if you're not sure it's current).

## Export the default set of files

```bash
juju-lens export ./recordings/my-capture
```

This writes to `<recording>/derived/export/` by default (override with
`--out`):

| File | Contents |
|---|---|
| `SUMMARY.md` | Orientation: time range, units involved, hook-outcome counts. |
| `timeline.jsonl` | Events and logs merged, one JSON object per line, chronological — `grep -C` around a line for real context. |
| `events.jsonl` | The same event lines, without the log volume. |
| `details/<span_id>.json` | Full detail for one event: statuses set, databag/config diffs, failure explanation. |
| `report.md` | The same data as one narrated document, for a human. |
| `AGENTS.md` | A legend naming whichever of the above files are present. |

## Export only some files

```bash
juju-lens export ./recordings/my-capture --parts summary,report
```

Valid values: `summary`, `timeline`, `events`, `details`, `report`.

## Narrow what's in scope

These flags restrict content regardless of which files it lands in:

```bash
juju-lens export ./recordings/my-capture \
    --model cos-lite \
    --unit grafana/0 \
    --since 2026-08-20T14:00:00Z \
    --until 2026-08-20T15:00:00Z
```

`--model` is required if the recording has more than one model. `--unit` is
repeatable and accepts either a unit or an application name.

## Export only the failures

```bash
juju-lens export ./recordings/my-capture --errors-only
```

Restricts to hook runs that ended errored, retried, or with an RPC warning,
plus one same-unit neighbour on each side for context.

## Export a single document instead of a folder

```bash
juju-lens export ./recordings/my-capture --format json > capture.json
juju-lens export ./recordings/my-capture --format md > capture.md
```

`--format` writes one merged document to stdout and skips the folder
entirely — useful for piping into another tool.

## Next steps

See the full flag list in the [CLI reference](../reference/cli.md#export),
or [the hook timeline explanation](../explanation/hook-timeline.md) for what
the failure classification in `report.md` actually means.
