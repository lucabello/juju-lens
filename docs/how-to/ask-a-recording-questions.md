# How to ask a recording questions

Use this guide to find the slowest or costliest hooks of a recording, and the code behind them, with `juju-lens inspect`. It answers in short text, so that you, or an AI agent with a shell, can ask for what you need instead of reading every file of an [export](export-for-an-agent.md). `inspect events` lists every kind of event, which are mostly hooks.

## Prerequisites

- `juju-lens` installed. See [install](../../README.md#install).
- A recording made with `--attach unit`, the default, for costs and profiles. Events and failures are in any recording.
- The recording doesn't need an index. `inspect` builds it the first time and says so; it can take a while for a large recording.
- `jq`, for the last step.

The examples use a recording of an `opentelemetry-collector-k8s` unit, at `./rec`. Your span IDs will differ. To try the commands without a controller, [generate a synthetic recording](generate-a-synthetic-recording.md) and use its path instead of `./rec`.

## List the hooks that cost the most

Rank the events by the CPU the charm's code used. Add `--unit` and `--name` to narrow them down, and use `--sort mem` for the hooks that took the most memory, or `--sort duration` for the longest:

```bash
juju-lens inspect events ./rec --name config-changed --sort cpu --limit 5
```

The list shows each event's duration, CPU, and the memory its stacks saw taken, with the ID of its span (excerpt):

```text
TIME (UTC)    UNIT       EVENT           OUTCOME  DURATION  CPU    MEMORY TAKEN  SPAN
23:25:58.352  otelcol/0  config-changed  none     3.9s      137ms  37MB          e39d3d6c60330477

1 event
next: juju-lens inspect event ./rec e39d3d6c60330477
      juju-lens inspect profile ./rec --span e39d3d6c60330477
```

To list only the events that went wrong, pass `--fail problem`. Every list says how many events it shows of how many, and `--skip` shows the next ones.

## Read one hook

Pass the span ID to `event`:

```bash
juju-lens inspect event ./rec e39d3d6c60330477
```

It prints what the export's report has for that hook: why it failed, the statuses it set, what the charm did in it, and its heaviest stacks. Use `--section` to print only some of that, for example `--section statuses,stacks`.

## Find the code behind the time

Open the profile of the hook. Without a view, it shows the functions with the most time in their own code:

```bash
juju-lens inspect profile ./rec --span e39d3d6c60330477 --detail charm --order total
```

`--detail charm` shows only the charm's code, and `--order total` puts first the functions with the most time in them and in what they called (excerpt):

```text
SELF   SELF%  TOTAL  TOTAL%  FUNCTION
1.1s   28%    3.2s   82%     OpenTelemetryCollectorK8sCharm._reconcile (src/charm.py:101)
0.0ms  0%     3.2s   82%     OpenTelemetryCollectorK8sCharm.__init__ (src/charm.py:93)
1.8s   47%    1.8s   47%     refresh_certs (src/charm.py:46)
534ms  14%    534ms  14%     (importing Python modules)
```

To see what one of them called, give `peek` part of its name:

```bash
juju-lens inspect profile ./rec --span e39d3d6c60330477 peek _reconcile
```

For the whole call tree, use `tree`. To scope a profile to charm init or one observer, pass its span ID to `--span`. The `stacks` view prints the span each stack ran in, with its ID, and `juju-lens inspect event ./rec <span-id> --section costs` lists them.

## Find where memory went

Weigh the profile by memory instead of time:

```bash
juju-lens inspect profile ./rec --span e39d3d6c60330477 --metric mem tree
```

The tree shows the memory taken by each function and the ones it called. It counts growth between two samples, so it shows where memory was taken, not where it was held. [How to trace a charm's memory spike to its code](trace-a-memory-spike.md) follows a spike through the viewer.

## If a command finds nothing

A recording with several models needs `--model`. A recording made with `--attach host` alone, or with `--profile=false`, has events but no stack samples, so `profile` stops with an error that says so.

## Use JSON

Every command takes `--json`. For example, to pick the span of the costliest hook in a script:

```bash
juju-lens inspect events ./rec --sort cpu --limit 1 --json | jq -r '.events[0].span_id'
```

For every flag and field, see the [command line reference](../reference/cli.md#inspect).
