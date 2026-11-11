# How to make a recording cheaper

Use this guide to lower what a unit recording costs: the memory and processes on the machine that runs `juju-lens`, the load on the controller, and the size of the recording on disk. The defaults suit a model you're investigating on a workstation. Change them for a long recording, a CI job whose artifacts are kept, or a model with many units. For what each cost is and how it was measured, see [what it costs](../explanation/how-unit-capture-works.md#what-it-costs).

## Prerequisites

- A recording you take with the default `--attach unit`, or with `host,unit`.

## Record only what you need

Every unit in scope holds a `juju ssh` session, which is a `juju` process of about 110 MB on the machine running `juju-lens`, and a connection through the controller. Without `--model`, every unit of every model on the controller is in scope. Pass `--model`, and `--app` or `--unit` for the part you're debugging:

```bash
juju-lens record my-capture --model cos --app otelcol
```

On a machine model, a probe covers every unit on its machine, so scoping to one unit doesn't reduce the work on that machine.

## Turn off stack sampling

Stack samples are about a third of the raw data of a busy recording and almost half of its index. `--profile=false` leaves them out, and keeps the events, observers, hook tool and Pebble calls, and each hook's CPU and memory:

```bash
juju-lens record my-capture --model cos --profile=false
```

A recording of a settled model with the samples off was a quarter smaller on disk. Without them the viewer's flame graphs and the export's `top_stacks` are empty: use this when you want to know which hook did something, not which code. Each hook reads the setting as it starts, so it takes effect from the next hook.

## Stop earlier

Set a limit, so that a recording nobody stops can't grow without bound:

```bash
juju-lens record my-capture --model cos --max-duration 30m --max-size 500000000
```

`--max-size` counts every source under `raw/`: RPCs, the unit probes' records, logs and status snapshots. The index is built from `raw/`, so the recording ends up about as large again.

## Record fewer logs

`--logs=false` turns off `juju debug-log`, workload logs and machine logs. In a recording of a busy Kubernetes model, logs were a small part of what the probes wrote, but in a host recording they were a third of the raw data, and some log sources each run a `juju` or `kubectl` process. See the [command line reference](../reference/cli.md#record) for each switch.

## Upload the raw data, rebuild the index

The index is as large as the raw data, and compresses worse. In a settled model, `raw/` compressed to 1.1 MB and `index.db` to 1.7 MB. To keep an artifact small, leave `index.db` out and rebuild it where you read the recording:

```bash
tar -czf recording.tar.gz --exclude=index.db -C ./recording .
```

Then, after unpacking it:

```bash
juju-lens index ./recording
```

[How to rebuild a recording's index](rebuild-the-index.md) describes when to rebuild it. The viewer and the export need the index.

## Check what it saved

Compare sizes before and after with `du -sh <recording> <recording>/raw`. In `recorder.log`, the recorder lists the probes it started and the units it couldn't reach.
