# How to rebuild a recording's index

Use this guide to regenerate `index.db` from a recording's raw data. Rebuild it when:

- A new version of `juju-lens` changed the index schema or how it extracts hooks and state.
- `index.db` is missing or damaged.
- The recorder was stopped with [`stop --force`](stop-a-recording.md) and didn't finish indexing.

Rebuilding only reads `raw/`, so it's safe to repeat. See [from wire to viewer](../explanation/from-wire-to-viewer.md#the-index) for why this works.

## Rebuild the index

Pass the recording directory:

```bash
juju-lens index ./recordings/my-capture
```

A model created during a recording can appear twice in the viewer: once under its UUID, for RPCs captured before `juju-lens` learned its name, and once under its name. To merge the first into the second, add `--rename` with the old and new names:

```bash
juju-lens index ./recordings/my-capture --rename 0c4a9e1f-1b2c-4d3e-8f5a-6b7c8d9e0f1a=test-charm-x7k2
```

Repeat `--rename` to merge more than one pair.

For every flag, see the [command line reference](../reference/cli.md#index).
