# How to generate a synthetic recording

Use this guide to get a `juju-lens` recording to look at without a Juju
controller, root access, or eBPF — useful for trying the viewer, for
`juju-lens` development, or as fixture data for a bug report. For a
recording captured from a real deployment instead, see the [first-recording
tutorial](../tutorials/first-recording.md).

## Prerequisites

- `juju-lens` built (`just build`) or installed. Works on any platform Go
  targets — synthetic recordings are plain data, so none of the eBPF/Linux
  requirements apply here.

## Generate one

```bash
juju-lens synth trivial --output /tmp/rec-trivial
```

`trivial` is currently the only scenario: two applications, `grafana` and
`prometheus`, forming one relation — it deploys both, has them join the
relation, sets a workload status on each unit, and sets the leader's
application status.

The output is byte-for-byte reproducible: running the same command again
overwrites the recording with identical content, unless you change
`--start` or `--model`. See the [CLI reference](../reference/cli.md#synth)
for both.

## Open it

```bash
juju-lens view /tmp/rec-trivial
```

## Next steps

See the [recording layout reference](../reference/recording-layout.md) for
what's on disk, or [how to export a recording for an
agent](export-for-an-agent.md) to turn it into narrated artifacts.
