# How to make a recording cheaper

Use this guide to lower what a recording made with the default `--attach unit`, or with `host,unit`, costs: the memory and processes on the machine that runs `juju-lens`, the load on the controller, and the size of the recording on disk. The defaults suit a model you're investigating on a workstation. Change them for a long recording, a CI job whose artifacts are kept, or a model with many units. For what each cost is and how it was measured, see [what it costs](../explanation/how-unit-capture-works.md#what-it-costs).

## Record only what you need

Every unit in scope on Kubernetes, and every machine on a machine model, holds a `juju ssh` session: a connection through the controller and, on the machine running `juju-lens`, a `juju` process, which took about 110 MiB on Kubernetes. Without `--model`, every unit of every model on the controller is in scope. Pass `--model`, and `--app` or `--unit` for the part you're debugging:

```bash
juju-lens record my-capture --model cos --app otelcol
```

On a machine model, a probe covers every unit on its machine, so scoping to one unit doesn't reduce the work on that machine.

## Turn off stack sampling

Stack samples were a third of the raw data of a busy recording and almost half of its index. `--profile=false` leaves them out, and keeps the events, observers, hook tool and Pebble calls, and each hook's CPU and memory:

```bash
juju-lens record my-capture --model cos --profile=false
```

A recording of a settled model with the samples off was a quarter smaller on disk. Without them the viewer's profile and `juju-lens inspect profile` have nothing to show: use this when you want to know which hook did something, not which code.

## Stop earlier

Set a limit, so that a recording nobody stops can't grow without bound:

```bash
juju-lens record my-capture --model cos --max-duration 30m --max-size 500000000
```

`--max-size` counts every source under `raw/`: RPCs, the unit probes' records, logs and status snapshots. The index is built from `raw/`, so the recording ends up about as large again.

## Record fewer logs

`--logs=false` turns off `juju debug-log`, workload logs and machine logs. In the recording of a six-unit deploy, logs were under 1% of the raw data, but in a host recording of a CI run they were a third. `--debug-log=false` also ends one `juju` process of about 110 MiB, and `--machine-log` runs `juju ssh … journalctl` for each machine. See the [command line reference](../reference/cli.md#record) for each switch.

## Upload the packed recording

The index is up to as large as the raw data, and compresses worse: equal in the deploys, 60% to 75% in a settled model, where `raw/` compressed to 1.1 MB and `index.db` to 1.7 MB. `juju-lens export` packs a recording without its index, which is rebuilt where you read it:

```bash
juju-lens export ./recording --output recording.tar.gz
```

[How to share a recording](share-a-recording.md) describes reading it again.

## Check what it saved

Compare sizes before and after with `du -sh <recording> <recording>/raw`. In `recorder.log`, the recorder lists the probes it started and the units it couldn't reach.
