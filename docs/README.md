# juju-lens documentation

`juju-lens` records what a Juju controller and its models do, and lets you browse the recording offline: hooks, status changes, relation data, and logs on one timeline. For installation, see the [project README](../README.md#install).

## Tutorials

- [Your first recording](tutorials/first-recording.md): record a controller while a charm's integration tests run against it, then explore the result.

## How-to guides

- [Record a Juju controller](how-to/record-a-controller.md)
- [Watch a live model](how-to/watch-a-live-model.md)
- [Stop a running recording](how-to/stop-a-recording.md)
- [Rebuild a recording's index](how-to/rebuild-the-index.md)
- [Export a recording for an AI agent](how-to/export-for-an-agent.md)
- [Generate a synthetic recording](how-to/generate-a-synthetic-recording.md), without a Juju controller

## Reference

- [Command line](reference/cli.md)
- [Recording layout](reference/recording-layout.md)
- [SQLite index schema](reference/sqlite-schema.md)
- [Viewer keybindings](reference/viewer-keybindings.md)

## Explanation

These pages follow captured traffic from the agent to the Events pane. Each builds on the one before, so read them in order:

1. [How capture works](explanation/how-capture-works.md): reading Juju API traffic from inside the agents with eBPF.
2. [From wire to viewer](explanation/from-wire-to-viewer.md): the probe, the recorder, the index, and what can be lost along the way.
3. [From RPCs to hooks](explanation/rpcs-to-hooks.md): how RPCs become hooks, state changes, and what the viewer shows.

[Why the wire](explanation/why-the-wire.md) is an aside on why `juju-lens` doesn't use the charm process or the Juju API instead.
