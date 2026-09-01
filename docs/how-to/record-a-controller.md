# How to record a live Juju controller

Use this guide to capture RPC traffic and logs from a real Juju controller
into a `juju-lens` recording you can later browse with `juju-lens view`.
Recording attaches read-only eBPF uprobes to the target `jujud` (or
`containeragent`) process — it never reads, sets, or restores any Juju
configuration, so it has no side effects on the controller or its models.

## Prerequisites

- `juju-lens` and `juju-lens-probe` built or installed (see the [README's
  installing section](../../README.md#installing)).
- Linux kernel ≥ 5.8 on the host running the target `jujud`/`containeragent`
  process, and `CAP_BPF` (or root) on whichever process attaches the probe.
- Existing Juju credentials for the controller you want to record, reachable
  the normal way (`~/.local/share/juju`, or `JUJU_DATA`).
- For SSH attach: a machine you can reach with `juju ssh` that has
  `juju-lens-probe` already on its `$PATH` — `record` does not stage or
  `scp` the binary there for you.

## Choose an attach mode

`--attach` accepts two values:

- `local` — the target `jujud`/`containeragent` runs on the same host as
  `juju-lens record`. Requires root (or `CAP_BPF`) on this host.
- `ssh` — the target runs on a machine controller reached through `juju ssh`.
  Requires `juju-lens-probe` pre-installed on that machine, and root/`CAP_BPF`
  there.

If you omit `--attach`, `juju-lens` auto-detects based on the controller.

## Record over SSH

```bash
juju-lens record my-capture \
    --output ./recordings/my-capture \
    --attach ssh \
    --controller my-juju-controller \
    --ssh-target controller/0 \
    --max-duration 30m
```

`my-capture` is a label used for the recording directory name and stored in
`manifest.json`; it is not passed to `juju` itself. `--max-duration` (or
`--max-size`) stops the recording cleanly once the limit is hit — useful for
unattended captures.

## Record on this host

If a `jujud` is installed locally (for example, a snap-installed
single-machine controller):

```bash
sudo juju-lens record local-capture --attach local
```

## Scope to specific models or PIDs

By default `record` captures every model on the controller. Narrow this with:

```bash
juju-lens record my-capture --model my-model
juju-lens record my-capture --pid 12345 --pid 12346
```

## Turn off a log source

`record` streams `juju debug-log`, Kubernetes workload-container logs, and
machine journald by default. Disable any of them individually:

```bash
juju-lens record my-capture --debug-log=false --k8s-log=false --machine-log=false
```

## Stop the recording

Press `Ctrl-C` in the terminal running `record`, or — if it's running in the
background — see [how to stop a running recording](stop-a-recording.md).
Either way, `record` detaches its uprobes cleanly, finalises
`manifest.json`, and builds `index.db` before exiting.

## Next steps

Open the finished recording with `juju-lens view ./recordings/my-capture`,
or see the full flag list in the [CLI reference](../reference/cli.md#record).
For a variant that opens the viewer automatically while recording, see [how
to watch a live model](watch-a-live-model.md).
