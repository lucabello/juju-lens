# How to trace a charm's memory spike to its code

Use this guide when a charm's memory jumps after a known event and you want to know which hook, which observer, and which code path took it. It records the charm from inside its units with `--attach unit`, which needs only `juju ssh`, so it works on a remote Kubernetes cluster and on charms you can't change.

The example is an `opentelemetry-collector-k8s` aggregator (`otelcol`) in a COS model, sending metrics to Prometheus and logs to Loki, and scraping both. The model is `lens-t14` on the `k8s` controller; a configuration change makes the charm restart its workload.

## Prerequisites

- `juju-lens`. See [install](../../README.md#install). It carries a static probe for `amd64` and `arm64`, and `record` uploads the one that matches each unit; a `go install` build doesn't carry it, so use a release or `just build`.
- A Juju client that can `juju ssh` into the units of the model. On machine models, the `juju ssh` user needs passwordless `sudo`.
- `jq`, for the last step.
- A charm built on [ops](https://github.com/canonical/operator). Other charms get each hook's duration and cost, but not its events, observers or stacks.

`--attach unit` writes a module into every charm's venv in the model for as long as the recording runs, and removes it when the recording ends. [How unit capture works](../explanation/how-unit-capture-works.md) describes what it changes and what it costs.

## Record the model

Start a recording of the model before the event happens. Every unit in the model gets a probe:

```bash
juju-lens record otelcol-spike --model k8s:lens-t14 --attach unit -o /tmp/jl-t14
```

Wait until each unit reports `tap installed`:

```text
juju-lens: recording "otelcol-spike" with --attach unit (pid 433472)
juju-lens: stop this recording with `juju-lens stop /tmp/jl-t14` (or SIGINT/SIGTERM)
juju-lens: starting unit probe on lens-t14 loki/0
juju-lens: starting unit probe on lens-t14 otelcol/0
juju-lens: starting unit probe on lens-t14 prometheus/0
juju-lens: unit probe running on lens-t14 otelcol/0
juju-lens: unit probe running on lens-t14 loki/0
juju-lens: unit probe running on lens-t14 prometheus/0
juju-lens: lens-t14 prometheus/0: tap installed for prometheus/0
juju-lens: lens-t14 otelcol/0: tap installed for otelcol/0
juju-lens: lens-t14 loki/0: tap installed for loki/0
```

If a unit reports that another recording owns it, stop that recording first, or wait: `record` tries the unit again 2 minutes later.

To record in the background instead, for example from a script, add `--detach`; it returns once every unit has a probe. See [how to stop a running recording](stop-a-recording.md).

## Make the event happen

From another terminal, trigger the event, or wait for it. Here, a configuration change makes the charm rewrite the collector's configuration and replan its workload:

```bash
juju config otelcol debug_exporter_for_metrics=true
```

After the event, leave the recording running for a little longer than `--linger`, 30 seconds by default, so the probe can follow the workload's memory after the restart the hook caused. If your workload takes longer to settle, start the recording with a longer `--linger`.

## Stop the recording

Press `Ctrl-C` where `record` runs, or from another terminal run:

```bash
juju-lens stop /tmp/jl-t14
```

The recorder indexes the recording and prints a summary in its terminal:

```text
juju-lens: indexed 3151 spans (3151 from the charms, 0 hooks the tap missed), 34 profiles, 0 snapshots (18 bootstrap), 2544 log records
juju-lens: recording saved to /tmp/jl-t14 (0 RPC messages, 2544 log lines, 7125 unit probe records captured)
```

A number of `hooks the tap missed` above 0 means some hooks ran without the module, for example right after a Kubernetes refresh; the viewer marks them `not instrumented`. See [hooks the tap missed](../explanation/rpcs-to-hooks.md#hooks-the-tap-missed).

## Find the hook

Open the recording:

```bash
juju-lens view /tmp/jl-t14
```

Press `/` and type the event's name, `config-changed`. The Events pane keeps the hooks that match, including hooks that emitted that event or ran an observer of that name:

```text
╭─ Events · /config-changed ──────────────────────
│ 00:01:27 otelcol/0   config-changed  (3.7s)
```

Press `Enter` to apply the filter, then `Enter` again to open the inspector. For a hook recorded with `--attach unit`, it opens on the trace of the hook: a bar for each span and charm function on a common time axis, with the hook process's memory over the same axis on top, and the time and memory (`mem Δ`) each row took on the right. Where the memory line rises, the bars below it are what ran then (excerpt):

```text
trace of config-changed  3.671s in all
  showing the charm's code and the calls it makes · hiding short calls and functions, and 657ms of importing Python modules before ops started (. shows more)
  open: all levels (1-9 set it, + and - change it by one)
                                                          0            +500ms        +1.0s         +1.5s         +2.0s         +2.5s         +3.0s         +3.5s     time   mem Δ
                                                          ┬────────────┬─────────────┬─────────────┬─────────────┬─────────────┬─────────────┬─────────────┬────
  memory of the hook process                              ▁▁▂▂▂▃▃▃▄▄▄▄▄▄▄▄▅▅▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▇▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆▆█
  ▾ config-changed                                        ██████████████████████████████████████████████████████████████████████████████████████████████████████   3.7 s    +75MB
    ▾ OpenTelemetryCollectorK8sCharm.__init__                              █████████████████████████████████████████████████████████████████████████████████      2.9 s    +20MB
      ▾ OpenTelemetryCollectorK8sCharm._reconcile                          █████████████████████████████████████████████████████████████████████████████████      2.9 s    +19MB
        ▾ refresh_certs  charm.py:46                                           ██████████████████████████████                                                     1.1 s
          ▾ ExecProcess.wait  pebble.py:1769                                    █████████████████████████████                                                     1.1 s
              GET /v1/changes/58/wait                                           █████████████████████████████                                                     1.0 s
        ▾ scrape_metrics  integrations.py:192                                                                 ██████                                              232 ms
        ▾ send_remote_write  integrations.py:222                                                                    ███████
        ▾ Container.replan  model.py:2607                                                                                    ███████████████
            GET /v1/changes/59/wait                                                                                           ████████████████
    ▾ config_changed                                                                                                                                        ██     64 ms
```

Read it from the top down. The process grew by 75 MB, and most of the hook is the charm's `__init__`, where `otelcol` reconciles. The two long waits are Pebble calls: one under `ExecProcess.wait`, which `refresh_certs` called, and one under `Container.replan`, with the charm's own code in between. By default the trace shows the charm's code and the calls it makes, which are ops and library functions such as `Container.replan`; they explain the time between a charm function and the Pebble call it caused. `.` shows everything, including short calls, ops internals and imports.

Use `↑` and `↓` to select a row. The details of the selected row are shown under the rows, including what a function was `mostly waiting` for when it used little CPU for the time it took, such as `waiting for Pebble (a command it ran)`. To find the interesting part quickly, fold and unfold: `Enter` or `←` and `→` fold one row; `1` to `9` open that many levels of the whole trace, so `2` shows the process and its direct children and `3` the next level; and `-` and `+` close and open one level under the selected row only, so you can open one branch deeper while the rest stays folded. The [viewer keybindings](../reference/viewer-keybindings.md#trace) list the rest. For a hook that changed a relation databag or the config, `d` shows the change as a diff, from the trace or straight from the Events pane.

Ops builds the charm by calling its `__init__`, and that call is what the trace shows at the start of the hook; the time ops takes around it is small.

## See what took the memory

Press `m` for the memory view; `Esc` goes back to the trace. It starts by saying what happened, in words (excerpt):

```text
memory on otelcol-0  the hook ran 00:01:27.3 → 00:01:31.0 UTC (3.7 s); readings from 2 s before it until its workloads settled
  What happened
  • The hook process grew from 13MB to 82MB (+70MB).
  • The charm container stayed around 114MB, and was as high as 180MB in between.
  • The otelcol container grew from 37MB to 39MB (+1.8MB).
                               before  highest    after
  hook process                   13MB     82MB     82MB
  charm container               114MB    180MB    115MB
  otelcol container              37MB     41MB     39MB
```

The hook process is the charm's Python process. A container is everything running in it, with file cache, which the kernel keeps and can take back; the view says how much of each container is file cache. Below the table, each container is drawn over time, with the hook and the Pebble changes in the workload containers underneath (`▲`). Under them, the biggest jumps of the hook process say when each happened and what the charm was doing:

```text
What took memory in the hook process
the biggest jumps; ← means "called by"
   +52MB  13MB → 64MB  0.6 s into the hook
          during the process starting up, before ops (importing Python modules)
   +17MB  65MB → 82MB  2.4 s into the hook
          during the charm's __init__
          code: compress ← CharmedDashboard.load_dashboards_from_dir ← GrafanaDashboardProvider._update_all_dashboards_from_dir ← GrafanaDashboardProvider.reload_dashboards ← forward_dashboards
   +16MB  66MB → 82MB  3.6 s into the hook
          during observer GrafanaDashboardProvider._update_all_dashboards_from_dir
          code: compress ← CharmedDashboard.load_dashboards_from_dir ← GrafanaDashboardProvider._update_all_dashboards_from_dir
```

Most of the first 52 MB is Python importing the charm and its libraries, before ops starts. Twice, 16 MB to 17 MB were taken while the charm compressed its Grafana dashboards with LZMA: once in the charm's `__init__`, where `otelcol` calls `forward_dashboards`, and once in the observer.

If the spike is in a workload container rather than in the charm, the jumps listed under `When the <container> container grew` say when it rose and which Pebble change was running then. In the trace, press `.` to see the Pebble call that started that change, such as `POST /v1/services replan`, under the charm function that made it.

## Confirm the code path in the profile

Press `p` for the profile of the hook, which adds up what each function took over the whole hook. It opens as a call tree of the charm's code and the calls it makes, weighed by time; `.` shows all code. Press `s` until it shows memory taken, and `Tab` to go through the parts of the hook: the whole hook, the charm's `__init__`, and each observer that has samples. For the observer:

```text
profile of observer GrafanaDashboardProvider._update_all_dashboards_from_dir  part 4 of 4: tab for the next
  20 ms sampled · cpu 10 ms · memory taken 16 MB
  showing memory taken (s) · the charm's code and the calls it makes (. shows more) · call tree (v)
  with calls             own               function
       16 MB 100.0%      0 B  ████████████ (everything)
       16 MB 100.0%      0 B  ████████████ GrafanaDashboardProvider._update_all_dashboards_from_dir  lib/charms/grafana_k8s/v0/grafana_dashboard.py:1257
       16 MB 100.0%      0 B  ████████████ CharmedDashboard.load_dashboards_from_dir  lib/charms/grafana_k8s/v0/grafana_dashboard.py:992
       16 MB  99.3%    16 MB  ███████████▉ LZMABase64.compress  cosl/utils.py:16
```

The 16 MB is in `LZMABase64.compress`, the call `load_dashboards_from_dir` makes, or below it, in code the view doesn't show. Press `.` to show all code, down to where the memory was taken:

```text
       16 MB 100.0%      0 B  ████████████ GrafanaDashboardProvider._update_all_dashboards_from_dir  lib/charms/grafana_k8s/v0/grafana_dashboard.py:1257
       16 MB 100.0%      0 B  ████████████ CharmedDashboard.load_dashboards_from_dir  lib/charms/grafana_k8s/v0/grafana_dashboard.py:992
       16 MB  99.3%      0 B  ███████████▉ LZMABase64.compress  cosl/utils.py:16
       16 MB  99.3%    16 MB  ███████████▉ compress  lzma.py:319
```

Press `v` for a table of functions, where `Enter` shows what called the selected function and what it called, with each one's share. To follow a function through the whole hook, press `/`, type part of its name, and press `Enter`; then `Tab` through the parts and `n` for the next match. On the whole hook, the profile also names what isn't the charm's code: `(importing Python modules)` is the time before ops starts, and `(ops framework)` is ops work with no charm function on the stack.

Memory taken counts how much resident memory grew between two samples, 10 ms apart, so it shows where memory was taken, not where it was held. Here one sample took 16 MB: the process took that memory within 10 ms, while `compress` ran. Because memory freed later doesn't count, a profile's total can be more than the process's `rss Δ`: 88 MB for the whole hook here, against +75 MB.

Back on the whole hook (`Tab`), the same path appears twice, as in the memory view.

## Get the same answer from an export

To hand the hook to someone else, or to an AI agent, export it. `--unit`, `--since` and `--until` narrow the export to the hook. Take its start and end from the inspector, whose times are in UTC, on the recording's date:

```bash
juju-lens export /tmp/jl-t14 --format md --unit otelcol --since 2026-10-23T00:01:27Z --until 2026-10-23T00:01:32Z
```

The Markdown has the same tree and cost columns as the inspector, then the five stacks with the most wall time and the five with the most memory growth:

```text
top stacks by memory growth
  1. 17MB (19%) · cpu 9.0ms · 1 sample · in charm __init__
       compress (lzma.py:319)
       LZMABase64.compress (cosl/utils.py:16)
       CharmedDashboard.load_dashboards_from_dir (lib/charms/grafana_k8s/v0/grafana_dashboard.py:992)
       GrafanaDashboardProvider._update_all_dashboards_from_dir (lib/charms/grafana_k8s/v0/grafana_dashboard.py:1257)
       GrafanaDashboardProvider.reload_dashboards (lib/charms/grafana_k8s/v0/grafana_dashboard.py:1253)
       forward_dashboards (src/integrations.py:419)
       OpenTelemetryCollectorK8sCharm._reconcile (src/charm.py:101)
       OpenTelemetryCollectorK8sCharm.__init__ (src/charm.py:93)
       … 4 outer frames
  2. 16MB (19%) · cpu 7.8ms · 1 sample · in GrafanaDashboardProvider._update_all_dashboards_from_dir
       compress (lzma.py:319)
       LZMABase64.compress (cosl/utils.py:16)
       CharmedDashboard.load_dashboards_from_dir (lib/charms/grafana_k8s/v0/grafana_dashboard.py:992)
       GrafanaDashboardProvider._update_all_dashboards_from_dir (lib/charms/grafana_k8s/v0/grafana_dashboard.py:1257)
       … 8 frames
       <module> (src/charm.py:1)
```

In JSON, each hook event has `costs`, per charm `__init__` and observer, and `top_stacks` and `top_memory_stacks`:

```bash
juju-lens export /tmp/jl-t14 --format json --unit otelcol --since 2026-10-23T00:01:27Z --until 2026-10-23T00:01:32Z \
  | jq -c '.items[] | select(.kind == "event") | .event.top_memory_stacks[] | {span, mem_growth_bytes, caller: .frames[1]}'
```

```text
{"span":"charm __init__","mem_growth_bytes":17330176,"caller":"LZMABase64.compress (cosl/utils.py:16)"}
{"span":"GrafanaDashboardProvider._update_all_dashboards_from_dir","mem_growth_bytes":17125376,"caller":"LZMABase64.compress (cosl/utils.py:16)"}
{"span":"config-changed process","mem_growth_bytes":6234112,"caller":"SourceFileLoader.path_stats (<frozen importlib._bootstrap_external>:1202)"}
{"span":"config-changed process","mem_growth_bytes":4919296,"caller":"SourceLoader.get_code (<frozen importlib._bootstrap_external>:1062)"}
{"span":"config-changed process","mem_growth_bytes":3371008,"caller":"_type_check (typing.py:175)"}
```

[How to export a recording for an AI agent](export-for-an-agent.md) describes the other files.

## If the hook is missing or cut short

- **`not instrumented`**: the hook ran without the module, so it has a duration and cost but no tree or stacks. The inspector gives the reason. After a Kubernetes refresh, the first hooks of the new pod are often missed this way; trigger the event again once the unit reports `tap installed`.
- **`truncated`**: the hook process was killed before it exited, for example by the out-of-memory killer. The tree and the stacks stop where it was killed, up to about a second before. The charm container's memory curve still shows what happened, and the `mem_oom_kill_events` counter in the index's [`resource_samples`](../reference/sqlite-schema.md#resource_samples) table says whether the out-of-memory killer ran.
