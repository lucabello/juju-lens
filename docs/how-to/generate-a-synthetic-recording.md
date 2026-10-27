# How to generate a synthetic recording

Use this guide to generate a recording without a Juju controller, root, or eBPF. Synthetic recordings are useful for trying the viewer, for developing `juju-lens`, and as test data in bug reports. They work on any platform `juju-lens` builds on.

## Generate and open a recording

Generate a recording from the `trivial` scenario:

```bash
juju-lens synth trivial --output /tmp/rec-trivial
```

The `trivial` scenario deploys `grafana` and `prometheus`, relates them, sets a workload status on each unit, and sets an application status from the leader. Running the command again produces identical files, unless you change `--start` or `--model`.

Open the result like any other recording:

```bash
juju-lens view /tmp/rec-trivial
```

## Try the charm-side view

To see what a recording with `--attach unit` shows, generate the `ops` scenario:

```bash
juju-lens synth ops --output /tmp/rec-ops
juju-lens view /tmp/rec-ops
```

It holds only what the units recorded, with no RPCs, as `record --attach unit` does on a Kubernetes model. Select a hook and press `Enter` to see its trace: its events, observers, charm functions and calls along time, with what each cost; press `.` to show everything, `p` for the hook's profile, and `m` for the memory around it.

For every flag, see the [command line reference](../reference/cli.md#synth).
