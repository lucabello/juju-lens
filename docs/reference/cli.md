# Command line

All `juju-lens` commands and their flags. For tasks, see the [how-to guides](../README.md#how-to-guides).

## `record`

```text
juju-lens record <name> [flags]
```

Records a controller and its models into a new recording directory. `<name>` labels the recording.

| Flag | Default | Meaning |
|---|---|---|
| `--output`, `-o` | `./recordings/<ts>--<name>` | Recording directory. |
| `--attach` | `unit` | What to capture, as a comma-separated set of `unit` and `host`, or `none`. See [attach modes](#attach-modes). |
| `--controller` | current controller | Record every model on this controller. |
| `--model` | all models | Record only this model, written as `model`, `owner/model` or `controller:model`. A model that another user or service account owns, such as one on a JAAS controller, is written `controller:owner/model`, for example `jaas:ci@serviceaccount/dev`; `juju models` lists the names. A short name that two owners share is ambiguous and needs the owner. |
| `--app` | all | With `--model`, record only the units of this application, such as `grafana`. Repeat it or separate names with commas to choose several. Combines with `--unit`. |
| `--unit` | all | With `--model`, record only this unit, such as `grafana/0`. Repeatable. With `--attach unit`, only these units get a probe on Kubernetes, and only the machines that host them on a machine model, where the probe still covers every unit on the machine. `juju debug-log` is limited to them too. `--attach host` still records every agent's traffic, and the status snapshot covers the whole model. |
| `--ssh-target` | none | With `--attach host`, run the probe on this machine over `juju ssh`, such as `controller/0`. The target is resolved in the current Juju model (`juju switch`), not in `--model`. Without it, the probe runs on this host. Setting it is an error unless `--attach` includes `host`. |
| `--pid` | all agent processes | Record only these agent process IDs. Repeatable. Setting it is an error unless `--attach` includes `host`. |
| `--logs` | `true` | Set to `false` to record no logs: it turns off `--debug-log`, `--k8s-log` and `--machine-log`, and wins over them. |
| `--debug-log` | `true` | Stream `juju debug-log` for the recorded models. |
| `--k8s-log` | `true` | Record workload-container logs for Kubernetes models. With `--attach unit` they are read through each unit probe's Pebble connection, which needs no `kubectl` or cluster access; otherwise they are streamed with `kubectl logs`, which needs `kubectl` pointing at the cluster of the model, and the recorder says so if it can't list the pods. |
| `--machine-log` | `true` | Stream the systemd journal for machine models. |
| `--max-duration` | no limit | Stop recording after this duration, such as `30m` or `2h`. |
| `--max-size` | no limit | Stop recording after this many bytes of `raw/`, counting every source: RPCs, unit probe records, logs and status snapshots. `index.db` isn't counted, and is built from `raw/`, so a recording ends up about as large again. |
| `--profile` | `true` | With `--attach unit`, sample each hook's Python stacks every 10 milliseconds, tagged with the event or observer running. `--profile=false` turns sampling off; the rest of the unit capture stays. Each hook reads the setting as it starts. Requires `unit` in `--attach`. |
| `--linger` | `30s` | With `--attach unit`, keep reading the workload containers' cgroups about 10 times a second for this long after the last Pebble change a hook caused is ready, so a workload that restarts and grows afterwards is seen in detail. `0` stops when the hook ends. Requires `unit` in `--attach`. |
| `--detach` | `false` | Run the recorder in the background. Return once it's capturing: with `--attach host`, once the probe has attached to at least one agent process; with `--attach unit`, once every unit in scope has a running probe. |
| `--detach-timeout` | `30s` | With `--detach`, fail if the recorder isn't capturing within this time. Requires `--detach`. |

### Attach modes

`--attach` takes `unit`, `host`, or both as `host,unit`, or `none` on its own. Without it, `record` uses `unit`.

| Value | Captures | Needs |
|---|---|---|
| `unit` (default) | Runs the probe inside every unit in scope, plus logs and the status snapshot, with the workload containers' logs on Kubernetes. On Kubernetes models it runs in each unit's charm container; on machine models, once per machine, next to the machine agent, with `sudo`. Units added during the recording get a probe too, and a lost session is started again. It doesn't record relation data. | `juju ssh` to the units. Nothing installed beforehand. |
| `host` | Every Juju API RPC of the agents, read at their TLS boundary with eBPF, plus logs and the status snapshot. The probe runs on this host, as `juju-lens probe`, or on `--ssh-target`, to which `juju-lens` uploads it through `juju ssh` and runs it with `sudo`. | Root or `CAP_BPF` on the host where the agents run. |
| `none` | Logs and the status snapshot only. No probe runs. | The `juju` client. |

`local` and `ssh`, the values of earlier releases, are both `--attach host` now: with `--ssh-target` set, the probe runs over `juju ssh`, otherwise locally. Earlier releases defaulted to `host`; a script that needs the Juju API traffic passes `--attach host`.

[How unit capture works](../explanation/how-unit-capture-works.md) explains the design of `--attach unit` and what it costs. [How to make a recording cheaper](../how-to/make-a-recording-cheaper.md) lists the switches that lower it.

With no `--model`, every model of the controller is in scope, and every unit of each of them gets a probe, so a controller with many models holds many `juju ssh` sessions. Pass `--model`, and `--app` or `--unit`, to record only what you need.

With `--attach unit`, `record` uploads the probe through each `juju ssh` session into `/tmp` on the unit, and the probe deletes the file as it starts. The probe then installs a Python module, `_juju_lens_tap.py`, into every charm venv on the host, with a loader that imports it when a hook's Python starts: `_juju_lens_tap.pth` in the venv's `site-packages`, or `sitecustomize.py` in the venv directory for charms that put it on `PYTHONPATH`. A charm with its own `sitecustomize.py` doesn't get the module. Hooks write what the module records to a spool directory, `/tmp/juju-lens-tap`, readable only by the agent's user, one file per hook process. The probe sends each record to the recorder as soon as the hook writes it, and deletes a hook's file once the hook has exited; `record` warns about a hook process that was killed before it finished. The probe puts the module back if a refresh replaces a venv. On Kubernetes, a refresh recreates the pod, and the first hook in the new pod, often `upgrade-charm`, runs before the new probe has installed the module.

The probe also measures what each hook, action and `juju exec` costs, with no privileges: CPU time of the process tree, from the agent's counters of its finished children; CPU, CPU throttling, memory (with its split into heap, page cache, kernel memory and shared memory, and the memory limit events), IO and pressure stalls of the agent's cgroup, from cgroup v2; and the same for every workload container, read through its Pebble socket. It takes a hook's start and end from the module's records, and polls `/proc` for what the module doesn't see: charms not built on ops, `juju exec`, and hooks that started before the module was installed. Every 5 seconds it also records the cgroups' counters, the disk usage of the root filesystem and Juju storage, its own CPU and memory, and any new Pebble changes in the workload containers. On Kubernetes the agent's cgroup is the charm container. On machine models it is the machine agent's systemd service, which runs the hooks of every unit on the machine, so hooks of different units that overlap share their cgroup figures and are marked as overlapped; workloads there run as their own services, and the probe doesn't measure them. On Linux 6.12 and later, where the probe can write the cgroup filesystem, the agent's cgroup peak during each hook is exact: the probe resets the cgroup's `memory.peak` for itself as the hook starts. Kubernetes mounts the cgroup filesystem read-only, so there the peak is exact only when the hook set a new high for the container; otherwise it is the highest of about 20 readings a second. The module records the hook process's resident memory as each event, observer and charm init starts and ends.

While a hook runs, and for `--linger` after the last Pebble change it caused is ready, the probe reads each workload container's cgroup about 10 times a second; see [workload bursts](recording-layout.md#workload-bursts). It does so through one `sh` it starts in each workload container with Pebble's `exec` and keeps for the session, because Pebble logs every file read through its file API in the workload's own logs; the `exec` adds about four lines to the workload's log, and a change, `Execute command "sh"`, to its Pebble changes. The shell exits when the probe stops. A workload container with no `sh` gets no bursts.

Unless `--profile=false` is set, the module also starts a thread in each hook process that samples the main thread's Python stack every 10 milliseconds, and records each sample with the event or observer running at that moment; see [stack samples](recording-layout.md#stack-samples). The setting is in a config file in the spool that each hook reads as it starts. What sampling costs a hook is in [how unit capture works](../explanation/how-unit-capture-works.md#what-it-costs).

When the recording ends, the probes remove the module, the loader, the bytecode Python cached for them, the spool and their lock file in `/tmp`, leaving each venv as they found it; on Kubernetes this can take up to about 30 seconds, until the session is torn down. If a probe dies before it can clean up, the module and loader stay in the venvs, but they do nothing: the module is active only while a probe refreshes the spool's heartbeat, and a hook with the spool gone or more than 30 seconds stale runs as it would without juju-lens. The next `--attach unit` recording on the unit removes them. `juju ssh` itself leaves a small `/tmp/<random>.pid` file in the charm container for each session on Kubernetes.

A unit that already runs a probe for another recording is skipped and tried again 2 minutes later; a probe left behind by a recorder that died is stopped by the next one. What the probes write is described in the [recording layout](recording-layout.md#unit-probe-records).

As the recording stops, `record` saves the status log of every unit a probe covered, from `juju show-status-log --type juju-unit`, to [`raw/status/<model>/status-log.json`](recording-layout.md#raw-data), and the index marks the hooks it lists that the tap recorded nothing of. See [hooks the tap missed](../explanation/rpcs-to-hooks.md#hooks-the-tap-missed).

With `--detach`, the recorder writes its output to `recorder.log` in the recording directory. When it's ready, `record` exits 0 and prints two lines to standard output:

```text
dir=<absolute recording directory>
pid=<recorder process ID>
```

If the recorder exits or doesn't become ready within `--detach-timeout`, `record` stops it, removes `recorder.pid`, and exits 1 with the cause and the end of `recorder.log` on standard error. A probe that never reports its attach status is too old for `--detach`; the error says so, and upgrading `juju-lens-probe` on the target fixes it. With `--attach none`, the recorder is ready as soon as it has started its log sources. See [how to record a controller in CI](../how-to/record-in-ci.md).

## `watch`

```text
juju-lens watch [model] [flags]
```

Records a model or controller and opens the viewer on the recording. Quitting the viewer stops the recording. With `--attach host` and no `--ssh-target`, run it with `sudo`.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | none | Watch only this model. Same as the `[model]` argument. |
| `--app`, `--unit` | all | Watch only these applications or units of the model. Same as for [`record`](#record). |
| `--controller` | none | Watch every model on this controller. |
| `--output`, `-o` | `./recordings/<ts>--watch-<scope>` | Recording directory. |
| `--attach` | `unit` | What to capture. Same values as for [`record`](#attach-modes). |
| `--ssh-target` | none | With `--attach host`, run the probe on this machine over `juju ssh`. |
| `--logs` | `true` | Set to `false` to record no logs. |
| `--debug-log` | `true` | Stream `juju debug-log` for the recorded models. |

## `stop`

```text
juju-lens stop <recording> [flags]
```

Stops a running recorder, found through `recorder.pid`, with `SIGTERM`.

| Flag | Default | Meaning |
|---|---|---|
| `--timeout` | `30s` | How long to wait for the recorder to exit. |
| `--force` | `false` | Send `SIGKILL` if the recorder is still alive after `--timeout`. |

## `index`

```text
juju-lens index <recording> [flags]
```

Rebuilds `index.db` from a recording's `raw/` directory.

| Flag | Default | Meaning |
|---|---|---|
| `--rename` | none | Merge model `OLD` into model `NEW`, written as `OLD=NEW`. Repeatable. |

## `view`

```text
juju-lens view <recording> [flags]
```

Opens a recording in the viewer. See [viewer keybindings](viewer-keybindings.md).

| Flag | Default | Meaning |
|---|---|---|
| `--follow`, `-f` | `false` | Follow a recording that is still being written, and refresh as it grows. |

## `export`

```text
juju-lens export <recording> [flags]
```

Packs a recording, or part of it, into one gzipped tar file: its `manifest.json`, everything under `raw/`, and `recorder.log`. It leaves out `index.db`, which is rebuilt from `raw/` the first time anything reads the recording. The files are under one directory named after the recording, so extracting the file gives a recording directory. A recorder must not be writing to the recording: stop it first. See [how to share a recording](../how-to/share-a-recording.md).

| Flag | Default | Meaning |
|---|---|---|
| `--output`, `-o` | `<recording directory name>.tar.gz` | The file to write, in the current directory by default. |
| `--model` | all | Only the files of this model. Repeatable. An error if the recording has no such model. |
| `--unit` | all | Only this unit or application. Repeatable. It keeps: the unit probe's files of the unit, on a machine those of the machine's probe if it covers the unit; the workload logs of the unit's pod; the lines of that unit in the debug log and the RPC capture; and the unit's entries in `status-log.json`. |
| `--without-values` | `false` | Leave out `raw/status/<model>/databags.json` and `config.json`, the snapshots that hold the values of relation data and configuration. `bootstrap.json` (the output of `juju status`) stays, and logs are unchanged. `inspect` and the viewer read the archive without them. |
| `--since`, `--until` | none | Only lines at or after, or at or before, this time, in RFC 3339 format. Each line's own timestamp decides. A unit probe's `hello` and `loader` records are kept whatever the time. |

With any of these, the archive's `manifest.json` has an `exported` object with the scope, and `inspect summary` says that the recording is a part. Status snapshots of a kept model (`bootstrap.json`, `databags.json`, `config.json`) are kept whole, except with `--without-values`. A line that continues the one before, as in a multi-line log message, follows it. A hook that straddles the window is cut: its records outside the window are gone, and `inspect` shows it as truncated, with the reason, rather than as still running; a hook that began before `--since` is listed. A hook wholly outside the window is not. `export` fails, and writes no file, when no line of `raw/` is in the scope. A time window with no hooks in it can still produce an archive, because a unit probe's `hello` and `loader` records and the status snapshots are kept.

## `inspect`

```text
juju-lens inspect summary <recording> [flags]
juju-lens inspect events <recording> [flags]
juju-lens inspect event <recording> <span-id> [flags]
juju-lens inspect costs <recording> [flags]
juju-lens inspect profile <recording> [top|peek|tree|stacks] [function] [flags]
juju-lens inspect trace <recording> <span-id> [flags]
juju-lens inspect memory <recording> <span-id> [flags]
juju-lens inspect logs <recording> [flags]
juju-lens inspect diff <recording-a> [<recording-b>] [flags]
```

Answers questions about a recording as text, or as JSON with `--json`. Most views end with a command to go on with. Every list, and the `stacks` view, says how many rows it shows out of how many, and how to see more. It reads the recording's index, and builds it first, with a notice on standard error, when the recording has none or has one of a different schema version. It leaves alone an existing index of a recording that a recorder is still writing. The text isn't stable; the JSON fields are the ones listed here.

These flags pick events, and are shared by `events`, `costs`, `profile` and `diff`; `summary` takes `--unit`, `--since` and `--until`:

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--unit` | all | Only this unit or application. Repeatable. |
| `--name` | all | Only events whose name contains this, ignoring case, such as `config-changed`. Repeatable. |
| `--fail` | all | Only events that ended this way: `none`, `errored`, `retried`, `interrupted`, or `problem` for any but `none`. Repeatable. |
| `--since`, `--until` | none | RFC 3339 time bounds, which include events at the given time. |

Where a flag is repeatable here or in the commands below (`--unit`, `--name`, `--fail`, `--grep`), repeated values are alternatives, and an event or line must match every flag you give.

### `inspect summary`

The first command to run on a recording. It prints when the recording was taken and the attach mode, how many events and log lines it holds and how the events ended, the events of each unit and how many were not clean, the five slowest events, and for profiled hooks the five that used the most CPU in the charm's init and observers.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--unit` | all | Only this unit or application. Repeatable. |
| `--since`, `--until` | none | RFC 3339 time bounds. |
| `--json` | `false` | Print `recording`, `model`, `started`, `events`, `log_lines`, `outcomes` (a count for each way an event ended), `units` (`unit`, `events`, `problems`) and `slowest`, and when set `controller`, `attach_mode`, `ended`, `costliest` and, for an exported part of a recording, `partial` (`models`, `units`, `since`, `until`, each only when it was given). `slowest` and `costliest` hold events as `inspect events` prints them in `--json`. |

### `inspect events`

Lists events, one a line, most of them hooks. Hooks recorded with `--attach unit` and profiled also show the CPU the charm's init and observers used, and the memory their stacks saw taken; time importing Python modules before ops starts is not counted in either. Sizes in the text output use 1024-based units, labelled `KB`, `MB` and so on.

| Flag | Default | Meaning |
|---|---|---|
| `--sort` | `time` | `time` (oldest first), `duration`, `cpu` or `mem` (largest first). Events without a profile come last when sorting by `cpu` or `mem`. |
| `--limit` | `20` | Show at most this many events. `0` shows all. |
| `--skip` | `0` | Leave out this many events first. |
| `--json` | `false` | Print an object with `recording`, `model`, `sort`, `total`, `skip` (only when above 0) and `events`. Each event has `ts`, `name`, `fail`, `span_id`, `profiled`, and, when set, `unit` and `duration_ms` (milliseconds). Profiled hooks also have `cpu_ms` (milliseconds), `mem_growth_bytes`, `rss_peak_bytes` (bytes), `top_frame` and `top_memory_frame`. |

### `inspect event`

Prints one event: why it failed, the statuses it set, what the charm did in it, its cost and its heaviest stacks. `<span-id>` is the `SPAN` of an `events` line, or the ID of a span inside the hook, such as an observer's in the `costs` section of this command's output or in the `stacks` of `profile`. For a span inside a hook, the command prints the whole hook and says so.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--section` | all | Only these parts: `statuses`, `ops`, `stacks` or `costs`. Comma-separated, or repeat the flag. The heading and the failure explanation are always printed. |
| `--json` | `false` | Print the event's details, without the parts `--section` leaves out. |

### `inspect costs`

Adds up what charm init and the observers cost over the hooks the scope flags pick, and groups it. The CPU and memory are those of each span's stack samples, with what the span called; time importing Python modules before ops starts is not in them.

| Flag | Default | Meaning |
|---|---|---|
| `--by` | `observer` | Group by `observer` (charm init is one group), `event` (the event name), `unit` or `app`. |
| `--sort` | `cpu` | `cpu`, `mem` (memory taken) or `duration`, largest first. |
| `--limit` | `20` | Show at most this many groups. `0` shows all. |
| `--skip` | `0` | Leave out this many groups first. |
| `--json` | `false` | Print `recording`, `model`, `by`, `sort`, `hooks` (profiled hooks in scope), `total`, `skip` and `costs`. A group has `name`, `hooks`, `calls`, `wall_ms`, `cpu_ms`, `mem_taken_bytes` and, when set, `kind` (`init` or `observer`), `rss_peak_bytes`, `unprofiled_calls` and `costliest_span_id`, the span with the most of what `--sort` ranks, which `profile --span` takes. |

The shared scope flags and `--model` apply.

### `inspect profile`

Shows the Python stacks that hooks recorded with `--attach unit` sampled every 10 milliseconds, with the views of [pprof](https://github.com/google/pprof). The first argument after the recording is the view; without one, `top`.

| View | Shows |
|---|---|
| `top` | The functions with the most in their own code (self), and what they called (total). |
| `peek <function>` | What called one function and what it called. `<function>` is part of its name; a name that matches several functions is refused, with the candidates. |
| `tree` | The call tree, outermost function first, without the branches under 1% of the total. |
| `stacks` | The heaviest stacks, outermost frame first and joined with `;`, each with the span it ran in and that span's ID. The opposite order from `top_stacks` in `inspect event --json`. |

| Flag | Default | Meaning |
|---|---|---|
| `--span` | none | One hook, by the span ID of its event, or one span in it, such as an observer or charm init. Replaces the flags that pick events. |
| `--metric` | `wall` | What to weigh by: `wall` time, `cpu` time of the main thread, or `mem`, the memory the process took. |
| `--detail` | `callee` | How much code to show: `charm`, the charm's own; `callee`, the charm's and what it calls directly; `ops`, the charm's and ops'; or `all`. What the hidden code cost is added to the function that called it. |
| `--order` | `self` | For `top`: order by `self` or `total`. |
| `--limit` | `15` for `top`, `10` for each list of `peek`, `40` lines for `tree`, `8` for `stacks` | Show at most this many rows (lines for `tree`). `0` uses the default; with `--folded`, `0` prints every stack. |
| `--folded` | `false` | With the `stacks` view: print every stack of the scope as [folded stacks](https://github.com/brendangregg/FlameGraph#2-fold-stacks), one per line and nothing else, for flame graph tools and `grep`: `unit;event;span;frame;…;frame value`, outermost frame first. The span is `charm __init__`, an observer, an ops event or `process`. The value is microseconds for `wall` and `cpu`, bytes for `mem`, added up over the hooks of that unit with that event name, largest first. Each frame is `function (file:line)`; the top level of a module is `importing <module>`. All frames are printed unless `--detail` is given, and every stack unless `--limit` is. Can't be combined with `--json`. |
| `--json` | `false` | Print `recording`, `model`, `scope`, `view`, `metric`, `detail`, `samples`, and the view's data: `functions` (`top`), `peek`, `tree` with `tree_hidden` (the count of functions left out), or `stacks`. Values in the view's data are nanoseconds for `wall` and `cpu`, bytes for `mem`. `functions` entries have `function`, `file`, `line`, `self`, `self_pct`, `total`, `total_pct` and `class` (`charm`, `ops` or `other`). `stacks` entries have `span`, `span_id`, `value`, `pct`, `samples` and `frames`, outermost first. The fields of `peek` and `tree` are not stable. |

The memory metric adds up how much resident memory grew between two samples. It shows where memory was taken, not where it was held.

### `inspect memory`

Says what happened to memory around one hook recorded with `--attach unit`: how the hook's process, the charm container and the workload containers moved, the biggest jumps with the observer and the charm code running at each, and the Pebble change a workload's jump followed. `<span-id>` is as for `inspect event`.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--json` | `false` | Print `recording`, `model`, `span_id`, `unit`, `event`, `host`, `hook_start` and `hook_end` (RFC 3339), and `rows` (`name`, `before_bytes`, `highest_bytes`, `after_bytes`, and when not zero `limit_bytes` and `file_cache_bytes`). Also, when there are any: `jumps`, each with `series`, `gain_bytes`, `from_bytes`, `to_bytes` and `when` (words, relative to the hook), and when set `during` (the observer or init), `code` (the charm's functions, innermost first) and `pebble`; `charm_container_peak_bytes` and `charm_container_peak_exact`; and `limit_events`, sentences about a container that reached its limit. |

### `inspect trace`

Lays one process of a hook out along time: charm init, events, observers, the hook tool and Pebble calls that took at least 20 milliseconds and 1% of the process, the observers of at least 5 milliseconds, and under them the functions of the charm's code, from the stack samples. Each row has its start from the start of the process, its duration, CPU and memory taken. Rows shorter than that, and functions under 15 milliseconds or 1% of the process, are left out unless `--all`; a `status-set` and a call that failed are always listed.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--attempt` | `1` | Which process ran the hook, from 1; `0` is an error. A hook that was retried ran in more than one. |
| `--detail` | `callee` | How much code the function rows show: `charm`, `callee`, `ops` or `all`. |
| `--all` | `false` | List every span, call and function, however short. |
| `--depth` | `0` | Only this many levels below the process. `0` for all. |
| `--min-duration` | none | Leave out rows shorter than this, such as `50ms`. |
| `--limit` | `80` | Show at most this many rows. `0` shows all. |
| `--json` | `false` | Print `recording`, `model`, `span_id`, `unit`, `event`, `attempt`, `attempts`, `process` (the hook's name), `duration_ms`, `total_rows` and `rows`, and `importing_python_ms` when it is not zero. A row always has `depth`, `kind` (`root`, `init`, `observer`, `event`, `backlog`, `tool`, `pebble` or `function`), `label`, `start_ms` (from the start of the process, which can be negative for the first rows) and `duration_ms`; and when set, `loc`, `detail`, `span_id`, `cpu_ms`, `mem_bytes`, `waiting_on`, `error`, `marks` and `short_rows_left_out`. Times are in milliseconds, memory in bytes. |

### `inspect logs`

Lists log lines in time order: `juju debug-log`, the workload containers' logs, and the machines' journals.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--unit` | all | Only this unit, application or entity such as `machine-0`. Repeatable. |
| `--level` | all | Only these levels, comma-separated, such as `error,warning`: `trace`, `debug`, `info`, `warning`, `error` or `critical`. Ignores case. |
| `--grep` | none | Only lines containing this text, ignoring case. Repeatable. |
| `--source` | all | Only `debug-log`, `k8s` or `journal`. |
| `--since`, `--until` | none | RFC 3339 time bounds. |
| `--around` | none | An RFC 3339 time: the lines within `--window` of it. Can't be combined with `--span`. |
| `--window` | `5s` with `--around`, `2s` with `--span` | How far either side of `--around`, or of a hook. An error without one of them. |
| `--span` | none | The lines about this event: those the index tied to it by its span ID, marked `*`, and the same unit's lines within `--window` of the hook. |
| `--width` | `240` | Cut each line's text at this many characters. `--json` has all of it. |
| `--limit` | `40` | Show at most this many lines. `0` shows all. |
| `--skip` | `0` | Leave out this many lines first. |
| `--json` | `false` | Print `recording`, `model`, `total` and `logs`; `event` with `--span`, and `skip` when above 0. A line always has `ts`, `source` and `body`; and when set, `unit`, `entity`, `level`, `module`, `span_id` and, with `--span`, `matched`: `span` for a line tied to the event, `window` for one near it. |

### `inspect diff`

Compares the stack samples of two sides, and lists the functions whose cost changed the most, per hook, so that sides with different numbers of hooks compare. A side is one hook, or the hooks that the scope flags pick. With one recording, the sides are two hooks of it, given by `--span-a` and `--span-b`. With two, each side is taken from its own recording, which compares the same hook before and after a change to the charm. Functions are matched by name and file, not by line.

| Flag | Default | Meaning |
|---|---|---|
| `--span-a` | none | Side `a` is this hook or span of the first recording. |
| `--span-b` | none | Side `b` is this hook or span of the second recording, or of the first when only one is given. |
| `--unit`, `--name`, `--fail`, `--since`, `--until`, `--model` | as for `events` | Pick the hooks of each side, applied to both. |
| `--metric` | `wall` | `wall`, `cpu` or `mem`, as for `profile`. |
| `--detail` | `callee` | `charm`, `callee`, `ops` or `all`. |
| `--order` | `self` | Compare each function's `self` value, or its `total`. |
| `--limit` | `15` | Show at most this many functions. `0` uses the default. |
| `--json` | `false` | Print `metric`, `detail`, `order`, `a` and `b` (`label`, `scope`, `profiled_hooks`, `samples`, `wall_ns_per_hook`, `cpu_ns_per_hook`, `mem_taken_bytes_per_hook`), `functions` (cut by `--limit`) and `changed_functions` (how many differ in all). A function has `function`, `a`, `b`, `delta` (b minus a), `class` (`charm`, `ops` or `other`) and, when set, `file`; values are nanoseconds or bytes per hook. |

## `skills`

```text
juju-lens skills list
juju-lens skills show <name>
juju-lens skills install [name...] --dir <directory>
```

Prints or installs the instructions for AI agents that are built into the binary, as `SKILL.md` files with a `name` and a `description` in their frontmatter, so that they describe the commands this version has. There is one skill, `investigate-a-recording`: the path through `juju-lens inspect`, and what its numbers mean.

| Command | Meaning |
|---|---|
| `list` | Each skill's name and description. |
| `show <name>` | Prints the skill's `SKILL.md`. |
| `install` | Writes the named skills, or all of them, as `<dir>/<name>/SKILL.md`. |

| Flag | Default | Meaning |
|---|---|---|
| `--dir` | none | With `install`: the directory to write under. Required; which directory an agent reads skills from depends on the agent, such as `~/.claude/skills` for Claude Code. A leading `~/` is the home directory. |

## `synth`

```text
juju-lens synth <scenario> [flags]
```

Writes a recording that imitates a Juju controller, without needing one. The output is identical on every run for the same flags. The scenarios are:

| Scenario | Recording |
|---|---|
| `trivial` | RPCs of `grafana` and `prometheus` forming one relation. |
| `ops` | What `--attach unit` records in two units of an `otelcol` charm, with no RPCs: an event deferred and re-emitted, a subprocess, a hook that fails twice in an observer before it succeeds, and a hook killed before it exits. It includes stack samples of every hook process, the containers' memory, and Pebble changes, including a replan after which the workload's memory rises after the hook has ended. |

| Flag | Default | Meaning |
|---|---|---|
| `--output`, `-o` | `./recordings/synth-<scenario>` | Recording directory. |
| `--start` | fixed per scenario | Start time of the recording, in RFC 3339 format. |
| `--model` | `default` | Model name to use in the recording. |

## `version`

```text
juju-lens version
```

Prints the version and the commit it was built from.
