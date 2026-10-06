# How to generate a synthetic recording

Use this guide to generate a recording without a Juju controller, root, or eBPF. Synthetic recordings are useful for trying the viewer, for developing `juju-lens`, and as test data in bug reports. They work on any platform `juju-lens` builds on.

## Generate and open a recording

Generate a recording from the `trivial` scenario:

```bash
juju-lens synth trivial --output /tmp/rec-trivial
```

`trivial` is the only scenario. It deploys `grafana` and `prometheus`, relates them, sets a workload status on each unit, and sets an application status from the leader. Running the command again produces identical files, unless you change `--start` or `--model`.

Open the result like any other recording:

```bash
juju-lens view /tmp/rec-trivial
```

For every flag, see the [command line reference](../reference/cli.md#synth).
