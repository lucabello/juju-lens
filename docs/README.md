# juju-lens documentation

This is the documentation for `juju-lens`, a time machine for Juju
controllers: it records everything a controller and its models emit, then
plays it back offline in a TUI where events, state changes, and logs are
correlated by time and by trace. If you're new here, start with the
[README](../README.md) for a one-paragraph pitch and current project status,
or [VISION.md](../VISION.md) for the full long-term design (implemented and
not-yet-implemented alike).

The docs below are organised by what you're trying to do:

## Tutorials

Learn `juju-lens` by doing, with no real Juju controller required.

- [Your first recording](tutorials/first-recording.md) — generate a
  synthetic recording and explore it in the viewer.

## How-to guides

Steps for a specific task, once you already know the basics.

- [How to record a live Juju controller](how-to/record-a-controller.md)
- [How to watch a live model](how-to/watch-a-live-model.md)
- [How to stop a running recording](how-to/stop-a-recording.md)
- [How to rebuild a recording's index](how-to/rebuild-the-index.md)
- [How to export a recording for an agent](how-to/export-for-an-agent.md)

## Reference

Terse, lookup-oriented material.

- [CLI reference](reference/cli.md) — every subcommand and flag.
- [Recording layout](reference/recording-layout.md) — what's on disk and why.
- [SQLite index schema](reference/sqlite-schema.md) — the tables the viewer queries.
- [Viewer keybindings](reference/viewer-keybindings.md)

## Explanation

Understanding-oriented: what `juju-lens` does and why it's built this way.

- [How capture works](explanation/how-capture-works.md) — eBPF uprobes at the TLS boundary.
- [From wire to viewer](explanation/from-wire-to-viewer.md) — how a captured byte becomes a row on screen.
- [Why the wire, not the charm or the API](explanation/why-the-wire.md)
- [The correlation model](explanation/correlation-model.md) — traces, spans, snapshots, logs.
- [The hook timeline](explanation/hook-timeline.md) — how the event list is derived.
