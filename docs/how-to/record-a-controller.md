# How to record a Juju controller

Use this guide to record the API traffic and logs of a running Juju controller, to browse later or share with someone debugging it. Recording attaches read-only eBPF probes to the Juju agents and doesn't change any Juju configuration.

## Prerequisites

- `juju-lens` installed. See [install](../../README.md#install). It carries the probe it runs.
- Linux 5.8 or newer, and root or `CAP_BPF`, on the host where the agents run.
- A Juju client that can reach the controller, using the usual credentials in `~/.local/share/juju` or `$JUJU_DATA`.
- To record a machine controller remotely: a Juju client that can `juju ssh` into the controller machine, with passwordless `sudo` there. `juju-lens` uploads the probe to the machine.

## Record on the same host

If the agents run on the host you're on, for example a local LXD or MicroK8s controller, run:

```bash
sudo juju-lens record my-capture
```

`my-capture` is a label for the recording. It's used in the directory name, `./recordings/<timestamp>--my-capture`, and stored in the recording's manifest. Use `--output` to choose a different directory.

## Record a machine controller over SSH

Pass the machine to connect to with `--ssh-target`:

```bash
juju-lens record my-capture \
    --controller my-controller \
    --ssh-target controller/0
```

`juju-lens` uploads its probe to that machine over `juju ssh`, runs it there with `sudo`, and streams the captured data back. The probe deletes itself as it starts. Without `--ssh-target`, the probe runs on the host you're on.

## Choose what to record

By default, `juju-lens` records every model on the current controller, including models created during the recording. To record a different controller, pass `--controller`. To record one model, pass `--model`:

```bash
sudo juju-lens record my-capture --model cos
```

To record specific agent processes only, pass their process IDs:

```bash
sudo juju-lens record my-capture --pid 12345 --pid 12346
```

`juju-lens` also collects `juju debug-log`, Kubernetes container logs, and the machine journal. To leave any of them out:

```bash
sudo juju-lens record my-capture --debug-log=false --k8s-log=false --machine-log=false
```

To record only the logs and the status snapshot, without the probe and without root, pass `--attach none`:

```bash
juju-lens record my-capture --model cos --attach none
```

## Stop the recording

Press `Ctrl-C`. If the recorder runs in the background, see [how to stop a running recording](stop-a-recording.md). Either way, `juju-lens` detaches from the agents, writes the manifest, and finishes building the index.

To record in the background from a CI job or script, see [how to record a controller in CI](record-in-ci.md).

For unattended recordings, set a limit when you start, and the recorder stops by itself after a time or once the raw data reaches a size in bytes:

```bash
sudo juju-lens record my-capture --max-duration 30m
sudo juju-lens record my-capture --max-size 2000000000
```

To browse the result, run:

```bash
juju-lens view ./recordings/<timestamp>--my-capture
```

For every flag, see the [command line reference](../reference/cli.md#record).
