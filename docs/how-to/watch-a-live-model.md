# How to watch a live model

Use this guide to follow a model's activity as it happens, for example while you deploy or debug a charm. `juju-lens watch` starts a recording in the background, opens the viewer on it, and stops the recording when you quit the viewer.

## Prerequisites

- The same as for [recording a controller](record-a-controller.md#prerequisites). `watch` only supports agents on the local host.

## Start watching

Pass the model to watch:

```bash
sudo juju-lens watch cos
```

This is the same as `--model cos`. To watch every model on a controller instead, pass `--controller`:

```bash
sudo juju-lens watch --controller microk8s
```

Juju commands that `watch` runs internally run as the user who invoked `sudo`, so your own Juju credentials are used. The recording is written to `./recordings/<timestamp>--watch-cos`, or to the directory given with `--output`.

`watch` waits up to 30 seconds for the recording to start before opening the viewer. If it gives up, read `record.log` in the recording directory: the recorder writes its output there instead of the terminal, so it doesn't interfere with the viewer. Common causes are missing root or `CAP_BPF`, a controller that can't be reached, and `juju-lens-probe` not being installed.

## Stop watching

Press `q` in the viewer. `watch` stops the recorder the same way [`juju-lens stop`](stop-a-recording.md) does, and keeps the recording, so you can open it again later:

```bash
juju-lens view ./recordings/<timestamp>--watch-cos
```

For every flag, see the [command line reference](../reference/cli.md#watch).
