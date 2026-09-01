# How to stop a running recording

Use this guide to stop a `juju-lens record` (or `watch`) process that's
running in the background or in a systemd unit, where you can't just press
`Ctrl-C`.

## Prerequisites

- A recording directory created by a currently-running `record`. You can
  tell it's still running if `recorder.pid` exists at the recording's root —
  `record` removes that file on a clean exit.

## Stop it

```bash
juju-lens stop ./recordings/my-capture
```

This reads the PID from `recorder.pid` inside the recording directory and
sends it `SIGTERM` — the same signal `Ctrl-C` would send in the foreground.
The recorder shuts down cleanly: it detaches its uprobes, finalises
`manifest.json`, and rebuilds `index.db`.

By default, `stop` waits up to 30 seconds for the recorder to exit.

## Force an unresponsive recorder to stop

If the recorder doesn't exit within the timeout, escalate to `SIGKILL`:

```bash
juju-lens stop --force --timeout 5s ./recordings/my-capture
```

Note that a forced kill skips the clean shutdown — `manifest.json` won't be
finalised and `index.db` may be stale or missing. Rebuild it afterwards with
[`juju-lens index`](rebuild-the-index.md).

## Next steps

Open the stopped recording with `juju-lens view ./recordings/my-capture`, or
see the full flag list in the [CLI reference](../reference/cli.md#stop).
