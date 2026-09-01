# How to watch a live model

Use this guide when you want to follow a model's activity as it happens,
instead of recording first and viewing later. `juju-lens watch` runs
`record` and `view --follow` together in one command: it starts a background
recording, opens the viewer once there's data to show, and stops the
recording cleanly when you quit the viewer.

## Prerequisites

- The same requirements as [recording a controller](record-a-controller.md):
  Linux ≥ 5.8 and `CAP_BPF`/root on the host running the target process,
  reachable Juju credentials.
- `watch` needs root for eBPF, so the whole command runs under `sudo`. Juju
  API calls inside it still run as `$SUDO_USER`, not root.

## Watch one model

```bash
sudo juju-lens watch cos-lite
```

This is equivalent to passing `--model cos-lite`. The recording is written
to `./recordings/<timestamp>--watch-cos-lite` unless you set `--output`.

## Watch an entire controller

```bash
sudo juju-lens watch --controller kub
```

This follows every model on the `kub` controller.

## What happens when you quit

Pressing `q` in the viewer stops the underlying recorder (a clean
`SIGTERM`, same as [`juju-lens stop`](stop-a-recording.md)), which finalises
`manifest.json` and rebuilds `index.db`. The recording directory is kept, so
you can reopen it later:

```bash
juju-lens view ./recordings/<timestamp>--watch-cos-lite
```

## If the viewer never opens

`watch` waits up to 30 seconds for the recording to become ready
(`manifest.json` and `index.db` both present) before giving up. The
recorder's own progress output doesn't go to your terminal — it's
redirected to `record.log` inside the recording directory so it doesn't
corrupt the TUI — so if `watch` reports a failure, check that file for the
underlying error (typically a missing `CAP_BPF`, an unreachable controller,
or `juju-lens-probe` not found).

## Next steps

See the full flag list in the [CLI reference](../reference/cli.md#watch), or
[how to export a recording for an agent](export-for-an-agent.md) once you
have one worth sharing.
