# How to record a Juju controller

Use this guide to record what happens in the models of a running Juju controller, to browse later or share with someone debugging it. By default, `juju-lens` records from inside each unit, over `juju ssh`; to record Juju's API traffic from the agents' host instead, see [record the Juju API traffic](#record-the-juju-api-traffic).

## Prerequisites

- `juju-lens` installed. See [install](../../README.md#install). It carries the probe it runs.
- A Juju client that can reach the controller, using the usual credentials in `~/.local/share/juju` or `$JUJU_DATA`, and that can `juju ssh` into the units. On machine models, passwordless `sudo` on the machines.

## Record a model

Pass the model to record:

```bash
juju-lens record my-capture --model cos
```

`my-capture` is a label for the recording. It's used in the directory name, `./recordings/<timestamp>--my-capture`, and stored in the recording's manifest. Use `--output` to choose a different directory.

Every unit of the model gets a probe, which writes a module into the charm's Python environment while the recording runs and removes it when the recording stops. [How unit capture works](../explanation/how-unit-capture-works.md) describes what changes on the units and what it costs.

Without `--model`, `juju-lens` records every model on the current controller, including models created during the recording, and starts a probe for every unit of each. To record a different controller, pass `--controller`. To record only some of a model's applications or units, pass `--app` or `--unit`:

```bash
juju-lens record my-capture --model cos --app otelcol --unit grafana/0
```

`juju-lens` also collects `juju debug-log`, Kubernetes container logs, and the machine journal. To leave any of them out:

```bash
juju-lens record my-capture --model cos --debug-log=false --k8s-log=false --machine-log=false
```

To record only the logs and the status snapshot, without a probe in the units, pass `--attach none`:

```bash
juju-lens record my-capture --model cos --attach none
```

## Record the Juju API traffic

A unit recording doesn't contain the values of relation data: the probe records which keys a hook tool touched, never the values. It also lacks the agent's status and the markers that show which hook the agent runs next, because those are on Juju's side. To record them, attach to the agents' host with `--attach host`, which needs root or `CAP_BPF` and Linux 5.8 or newer, on the host where the agents run. It attaches read-only eBPF probes to the Juju agents and doesn't change any Juju configuration. If the agents run on the host you're on, for example a local LXD or MicroK8s controller, run:

```bash
sudo juju-lens record my-capture --attach host
```

To record both sides, with each hook's charm-side tree joined to its RPCs, pass both:

```bash
sudo juju-lens record my-capture --model cos --attach host,unit
```

To record a machine controller remotely, pass the machine to connect to with `--ssh-target`. It needs a Juju client that can `juju ssh` into the controller machine, with passwordless `sudo` there:

```bash
juju-lens record my-capture \
    --controller my-controller \
    --attach host \
    --ssh-target controller/0
```

`juju-lens` uploads its probe to that machine over `juju ssh`, runs it there with `sudo`, and streams the captured data back. The probe deletes itself as it starts. Without `--ssh-target`, the host probe runs on the host you're on.

To record specific agent processes only, pass their process IDs:

```bash
sudo juju-lens record my-capture --attach host --pid 12345 --pid 12346
```

## Stop the recording

Press `Ctrl-C`. If the recorder runs in the background, see [how to stop a running recording](stop-a-recording.md). Either way, `juju-lens` detaches from the agents and units, writes the manifest, and finishes building the index. On Kubernetes, removing the probes from the units can take up to about 30 seconds.

To record in the background from a CI job or script, see [how to record a controller in CI](record-in-ci.md).

For unattended recordings, set a limit when you start, and the recorder stops by itself after a time or once the raw data reaches a size in bytes:

```bash
juju-lens record my-capture --model cos --max-duration 30m
juju-lens record my-capture --model cos --max-size 2000000000
```

To make a recording smaller or cheaper to take, see [how to make a recording cheaper](make-a-recording-cheaper.md).

To browse the result, run:

```bash
juju-lens view ./recordings/<timestamp>--my-capture
```

For every flag, see the [command line reference](../reference/cli.md#record).
