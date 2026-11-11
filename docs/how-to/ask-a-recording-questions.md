# How to ask a recording questions

Use this guide to find a hook of a recording that went wrong or cost a lot, and read the code and logs behind it, with `juju-lens inspect`. It answers in short text, so that you, or an AI agent with a shell, can ask for what you need. `inspect events` lists every kind of event, which are mostly hooks.

## Prerequisites

- `juju-lens` installed. See [install](../../README.md#install).
- A recording made with `--attach unit`, the default, for costs, profiles and traces.
- The recording doesn't need an index. `inspect` builds it the first time and says so; it can take a while for a large recording.
- `jq`, for the last step.

The examples use a recording of an `opentelemetry-collector-k8s` unit, at `./rec`. Your span IDs will differ. To try the commands without a controller, [generate a synthetic recording](generate-a-synthetic-recording.md) and use its path instead of `./rec`.

## Start with the summary

`summary` says when the recording was taken and with what, how many events each unit ran and how they ended, the slowest events, and the hooks that used the most CPU in the charm's code (excerpt):

```bash
juju-lens inspect summary ./rec
```

```text
recording ./rec, model expt, controller k8s
captured 2026-11-11 17:54:31 → 18:03:18 UTC, recorded with --attach unit
135 events, 531 log lines; ended: retried 4, none 131

units:
  alertmanager/0      21 events, 1 not clean
  grafana/0           36 events, 1 not clean
```

Add `--unit`, `--since` or `--until` to summarize part of it.

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

## Add the costs up

To see which observer, event, unit or application costs the most over a whole recording, rather than hook by hook, use `costs`:

```bash
juju-lens inspect costs ./rec --sort mem --limit 5
juju-lens inspect costs ./rec --by app
```

Each group has its CPU, wall time, the memory its stacks saw taken, and how many hooks and calls it adds up. The `next:` line gives the span of the group's costliest call, for `profile --span`.

## Read one hook

Pass the span ID to `event`:

```bash
juju-lens inspect event ./rec e39d3d6c60330477
```

It prints why the hook failed, the statuses it set, what the charm did in it, and its heaviest stacks. Use `--section` to print only some of that, for example `--section statuses,stacks`.

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

## Hand the stacks to a flame graph tool

`stacks --folded` prints every stack of the scope as folded stacks, one per line, which flame graph tools read and `grep` searches:

```bash
juju-lens inspect profile ./rec --unit grafana --metric cpu stacks --folded > grafana.folded
grep compress grafana.folded | head -3
```

Each line is `unit;event;span;frame;…;frame value`, outermost frame first, with the value in microseconds for wall time and CPU, or in bytes for memory taken.

## Find where memory went

Weigh the profile by memory instead of time:

```bash
juju-lens inspect profile ./rec --span e39d3d6c60330477 --metric mem tree
```

The tree shows the memory taken by each function and the ones it called. It counts growth between two samples, so it shows where memory was taken, not where it was held. [How to trace a charm's memory spike to its code](trace-a-memory-spike.md) follows a spike through the viewer.

## See when in the hook it happened

`trace` lays the hook out along time, with the functions of the charm's code under each span, and `memory` says when memory grew and what ran then:

```bash
juju-lens inspect trace ./rec e39d3d6c60330477 --min-duration 50ms
juju-lens inspect memory ./rec e39d3d6c60330477
```

The trace starts like this (excerpt):

```text
   START     TOOK      CPU   MEMORY  SPAN OR FUNCTION
   0.0ms     3.9s             +70MB  config-changed  · exit 0
   543ms     3.2s             +19MB    OpenTelemetryCollectorK8sCharm.__init__ (charm.py:93)  · 82% of the hook
   545ms     3.2s     88ms    +19MB      OpenTelemetryCollectorK8sCharm._reconcile (charm.py:101)
```

A hook that was retried ran in more than one process; `--attempt 2` traces the second.

## Read the logs around it

List the log lines about a hook, with `*` before the source of the ones the index tied to it by its span ID:

```bash
juju-lens inspect logs ./rec --span e39d3d6c60330477 --window 5s
```

Filter logs further with `--unit`, `--level`, `--grep` and the time bounds.

## Compare two hooks or two recordings

`diff` lists the functions whose cost changed the most, per hook. Compare two hooks of a recording, with the span IDs from `events`:

```bash
juju-lens inspect diff ./rec --span-a e39d3d6c60330477 --span-b <other-span-id>
```

Or compare the same hook in a recording from before and after a change to the charm:

```bash
juju-lens inspect diff ./before ./after --unit otelcol --name config-changed --metric mem
```

The table starts with the totals of each side, then lists each function (excerpt):

```text
A      B      CHANGE  FUNCTION
580ms  1.7s   +1.2s   SocketIO.readinto (socket.py)
450ms  1.1s   +630ms  dump (yaml/__init__.py)
```

## If a command finds nothing

A recording with several models needs `--model`. A recording made with `--attach host` alone, or with `--profile=false`, has events but no stack samples, so `profile` stops with an error that says so; `memory` and `trace` need a hook recorded with `--attach unit` and stop the same way. `logs` prints `no log lines match` when the recording holds none.

## Use JSON

Every command takes `--json`. For example, to pick the span of the costliest hook in a script:

```bash
juju-lens inspect events ./rec --sort cpu --limit 1 --json | jq -r '.events[0].span_id'
```

For every flag and field, see the [command line reference](../reference/cli.md#inspect).
