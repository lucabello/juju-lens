# How to rebuild a recording's index

Use this guide to regenerate `index.db` from a recording's raw captured
data. The index is a build artifact, not a source of truth — everything the
viewer needs can always be reconstructed from `raw/`.

## Prerequisites

- A recording directory with a `raw/` tree (any recording produced by
  `record`, `watch`, or `synth`).

## When to rebuild

- You've upgraded `juju-lens` and the SQLite schema changed.
- The recording was captured with an older build and you want it re-indexed
  with the current extraction logic.
- `index.db` is missing, corrupted, or was left stale by a forced
  [`stop --force`](stop-a-recording.md).

## Rebuild it

```bash
juju-lens index ./recordings/my-capture
```

This reads every captured RPC under `raw/rpc/`, pairs requests with
responses into spans, extracts derived state (application/unit status,
databags, and so on), and writes a fresh `index.db` at the recording root.
It's safe to run repeatedly.

## Merge two model identities

A model created partway through a recording can end up attributed under two
different identities — its raw model UUID for early RPCs, its model name for
later logs. `--rename` merges one into the other during rebuild:

```bash
juju-lens index ./recordings/my-capture --rename OLD=NEW
```

This merges `OLD`'s spans, snapshots, and logs into `NEW`, so the viewer
shows a single model instead of two. `--rename` is repeatable if you need to
merge more than one pair.

## Next steps

See the [SQLite index schema reference](../reference/sqlite-schema.md) for
what tables get rebuilt, or open the result with `juju-lens view`.
