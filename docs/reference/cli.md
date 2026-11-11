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

Writes a recording's hooks, state changes, and logs as a folder of plain-text files, or as one document on standard output. To ask a recording questions instead of reading files, see [`inspect`](#inspect). For hooks recorded with `--attach unit`, it also writes what the charm did in each process that ran the hook: every span in JSON, and in Markdown a tree where most hook tool and Pebble calls are folded into one line per span. When the hook was profiled, it adds the CPU and memory of charm init and each observer, and the five stacks with the most wall time and the five with the most memory growth. See [how to export a recording for an AI agent](../how-to/export-for-an-agent.md) for the files.

| Flag | Default | Meaning |
|---|---|---|
| `--format` | none | Write a single merged document to stdout instead of a folder: `json` or `md`. |
| `--out` | `<recording>/derived/export` | Folder to write the export into. |
| `--parts` | all | Comma-separated files to write, from `summary`, `timeline`, `events`, `details`, and `report`. |
| `--model` | none | Export only this model. Required when the recording has more than one. |
| `--unit` | none | Export only this unit or application. Repeatable. |
| `--since` | none | Export only events at or after this time, in RFC 3339 format. |
| `--until` | none | Export only events at or before this time, in RFC 3339 format. |
| `--errors-only` | `false` | Only hooks that errored, were retried, or had an RPC error, with the hook before and after each on the same unit. |

## `inspect`

```text
juju-lens inspect events <recording> [flags]
juju-lens inspect event <recording> <span-id> [flags]
juju-lens inspect profile <recording> [top|peek|tree|stacks] [function] [flags]
```

Answers questions about a recording as text, or as JSON with `--json`. Most views end with a command to go on with. Every list says how many rows it shows out of how many, and how to see more. It reads the recording's index, and builds it first, with a notice on standard error, when the recording has none or has one of a different schema version. It leaves alone an existing index of a recording that a recorder is still writing. The text isn't stable; the JSON fields are the ones listed here.

These flags pick events, and are shared by `events` and `profile`:

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--unit` | all | Only this unit or application. Repeatable. |
| `--name` | all | Only events whose name contains this, ignoring case, such as `config-changed`. Repeatable. |
| `--kind` | all | Only this kind of event: `hook`, `action`, `status`, `relation`, `leader`, `secret` or `settle`. Repeatable. |
| `--fail` | all | Only events that ended this way: `none`, `errored`, `retried`, `rpc-warn`, `interrupted`, `lost-continue`, or `problem` for any but `none`. Repeatable. |
| `--since`, `--until` | none | RFC 3339 time bounds, which include events at the given time. |

Repeated values of one flag are alternatives. An event must match every flag you give.

### `inspect events`

Lists events, one a line, most of them hooks. Hooks recorded with `--attach unit` and profiled also show the CPU the charm's init and observers used, and the memory their stacks saw taken.

| Flag | Default | Meaning |
|---|---|---|
| `--sort` | `time` | `time` (oldest first), `duration`, `cpu` or `mem` (largest first). Events without a profile come last when sorting by `cpu` or `mem`. |
| `--limit` | `20` | Show at most this many events. `0` shows all. |
| `--skip` | `0` | Leave out this many events first. |
| `--json` | `false` | Print an object with `recording`, `model`, `sort`, `total`, `skip` (only when above 0) and `events`. Each event has `ts`, `kind`, `name`, `fail`, `span_id`, `profiled`, and, when set, `unit` and `duration_ms` (milliseconds). Profiled hooks also have `cpu_ms` (milliseconds), `mem_growth_bytes`, `rss_peak_bytes` (bytes), `top_frame` and `top_memory_frame`. |

### `inspect event`

Prints one event as the [`report.md` of the export](../how-to/export-for-an-agent.md) narrates it. `<span-id>` is the `SPAN` of an `events` line, or the ID of a span inside the hook, such as an observer's in the `costs` section of this command's output or in the `stacks` of `profile`. For a span inside a hook, the command prints the whole hook and says so.

| Flag | Default | Meaning |
|---|---|---|
| `--model` | the only model | The model to read. Required when the recording has more than one. |
| `--section` | all | Only these parts: `statuses`, `ops`, `stacks`, `costs`, `databags` or `config`. Comma-separated, or repeat the flag. The heading and the failure explanation are always printed. |
| `--json` | `false` | Print the event's details, as in `details/<span_id>.json` of the export, without the parts `--section` leaves out. |

### `inspect profile`

Shows the Python stacks that hooks recorded with `--attach unit` sampled every 10 milliseconds, with the views of [pprof](https://github.com/google/pprof). The first argument after the recording is the view; without one, `top`.

| View | Shows |
|---|---|
| `top` | The functions with the most in their own code (self), and what they called (total). |
| `peek <function>` | What called one function and what it called. `<function>` is part of its name; a name that matches several functions is refused, with the candidates. |
| `tree` | The call tree, outermost function first, without the branches under 1% of the total. |
| `stacks` | The heaviest stacks, outermost frame first and joined with `;`, each with the span it ran in and that span's ID. The opposite order from `top_stacks` in the export. |

| Flag | Default | Meaning |
|---|---|---|
| `--span` | none | One hook, by the span ID of its event, or one span in it, such as an observer or charm init. Replaces the flags that pick events. |
| `--metric` | `wall` | What to weigh by: `wall` time, `cpu` time of the main thread, or `mem`, the memory the process took. |
| `--detail` | `callee` | How much code to show: `charm`, the charm's own; `callee`, the charm's and what it calls directly; `ops`, the charm's and ops'; or `all`. What the hidden code cost is added to the function that called it. |
| `--order` | `self` | For `top`: order by `self` or `total`. |
| `--limit` | `15` for `top`, `10` for each list of `peek`, `40` lines for `tree`, `8` for `stacks` | Show at most this many rows (lines for `tree`). `0` uses the default. |
| `--json` | `false` | Print `recording`, `model`, `scope`, `view`, `metric`, `detail`, `samples`, and the view's data: `functions` (`top`), `peek`, `tree` with `tree_hidden` (the count of functions left out), or `stacks`. Values in the view's data are nanoseconds for `wall` and `cpu`, bytes for `mem`. `functions` entries have `function`, `file`, `line`, `self`, `self_pct`, `total`, `total_pct` and `class` (`charm`, `ops` or `other`). `stacks` entries have `span`, `span_id`, `value`, `pct`, `samples` and `frames`, outermost first. The fields of `peek` and `tree` are not stable. |

The memory metric adds up how much resident memory grew between two samples. It shows where memory was taken, not where it was held.

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
