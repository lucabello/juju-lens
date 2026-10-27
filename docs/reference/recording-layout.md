# Recording layout

The files and directories in a recording. [From wire to viewer](../explanation/from-wire-to-viewer.md) describes how they're written.

```text
recordings/2026-07-03T14-30-12--my-capture/
├── manifest.json
├── recorder.pid
├── recorder.log
├── index.db
├── raw/
│   ├── rpc/<model>/calls-<hour>.jsonl
│   ├── juju/<model>/debug-log-<hour>.log
│   ├── k8s/<model>/<pod>/<container>/logs-<hour>.log
│   ├── machine/<model>/<host>/journal-<hour>.log
│   ├── unit/<model>/<host>/unit-<hour>.jsonl
│   └── status/<model>/
│       ├── bootstrap.json
│       ├── databags.json
│       ├── config.json
│       └── status-log.json
└── derived/
    └── export/
```

## Top-level files

| Path | Contents |
|---|---|
| `manifest.json` | The `juju-lens` version that wrote the recording; the controller's name, UUID, and version; the models recorded; the attach mode and targets; start and end times; why the recording ended; and the status of each data source. |
| `recorder.pid` | The process ID of the running recorder, used by [`juju-lens stop`](../how-to/stop-a-recording.md). Removed when the recorder exits normally. |
| `recorder.log` | The recorder's output, including the probe's. Written only by recorders started with [`record --detach`](../how-to/record-in-ci.md). Appended to, never rotated. |
| `index.db` | A SQLite database in WAL mode, built from `raw/`. The viewer reads only this file. See the [SQLite index schema](sqlite-schema.md). |
| `derived/export/` | The default output directory of [`juju-lens export`](../how-to/export-for-an-agent.md). |

## Raw data

Files under `raw/` that grow during a recording start a new file every hour.

| Path | Contents |
|---|---|
| `rpc/` | Captured Juju API messages, one JSON object per line. Requests and responses are separate lines. |
| `juju/` | `juju debug-log` output for each model, as printed. |
| `k8s/` | Workload-container logs from Kubernetes models, one directory per pod and container. |
| `machine/` | The systemd journal of each machine in machine models. |
| `unit/<model>/<host>/` | Output of the unit probes started by [`record --attach unit`](cli.md#attach-modes), one JSON object per line. `<host>` is the pod (`otelcol-0`) on Kubernetes models and `machine-<id>` on machine models, where one probe covers every unit on the machine. See [Unit probe records](#unit-probe-records). |
| `status/<model>/bootstrap.json` | `juju status --format=json`, taken when the recording started. |
| `status/<model>/databags.json` | `juju show-unit --format=json` for each unit, taken when the recording started. |
| `status/<model>/config.json` | `juju config <app> --format=json` for each application, taken when the recording started. |
| `status/<model>/status-log.json` | With `--attach unit`: a JSON object with, for each unit a unit probe covered, its `juju show-status-log --type juju-unit --format=json` output, as printed, taken when the recording stops. It lists the hooks and actions each unit's agent ran, except `update-status`, and covers at least the whole recording. The index uses it to find [hooks the tap missed](sqlite-schema.md#hooks-the-tap-missed). Missing when the recorder was killed before it could stop. |

## Unit probe records

Each line in `raw/unit/` has a `kind`, and a `ts` with the time the probe wrote it on the unit. A probe session starts with `hello` or `busy`. When a session ends early, for example because the pod was recreated, the recorder starts a new one on the next reconcile, and the new session appends to the same file starting with a new `hello`.

| `kind` | Other fields |
|---|---|
| `hello` | `hello`: `version` of the probe, its `pid`, `substrate` (`kubernetes` or `machine`), the Juju `agent` binary it found, its `agent_pid` and `agent_start` (on Kubernetes, about when the charm container started), the `units` whose agent directories are on the host, and the host's `boot_time`. For resource measurement: the agent's `cgroup` directory, the `workloads` containers reachable over Pebble (Kubernetes only), `idle_hz` and `run_hz`, how often `/proc` is polled while no execution runs and while one does, `probe_in_cgroup`, true when the probe shares the agent's cgroup and its own CPU is taken out of the cgroup figures, `linger_ms`, the `record --linger` time, and `mem_peak_reset`, true when the agent's cgroup has a `memory.peak` the probe can reset, so each execution's cgroup peak is exact. A probe writes another `hello` if the agent restarts. |
| `heartbeat` | None. Written every 5 seconds while the probe runs. |
| `busy` | `busy`: the `pid` of a probe from another recording that already owns the unit, and `heartbeat_age_ms`, how long ago it last wrote a heartbeat. The session ends after this record, and the recorder tries the unit again 2 minutes later. |
| `loader` | `loader`: an `event` about the tap in one charm's venv, with the `unit`, the venv `dir`, and the `loader` file (`_juju_lens_tap.pth` or `sitecustomize.py`). `installed`: hooks of that unit that started earlier ran without the tap; `dir_changed` is when the venv last changed before the probe wrote into it. `reinstalled`: the venv changed at `dir_changed`, for example in a refresh, and hooks between then and this record ran without the tap. `skipped`, with a `reason`: the charm doesn't get the tap, for example because it has its own `sitecustomize.py`, or because it has no venv (`the charm has no venv: it isn't built on ops`, with the charm directory as `dir` and no `loader`). `unavailable`, with a `reason` and no unit: no charm on the host gets the tap. `removed`: the tap was removed as the recording ended. |
| `tap` | `tap`: one record the tap wrote in a hook process, forwarded as soon as it was written: the `host` and `unit` it ran on, the hook process's `pid`, the spool `file` it came from (one per process), and the record itself in `rec`; see [tap records](#tap-records). A line that isn't valid JSON is in `text` instead. |
| `spool_end` | `spool_end`: a hook process exited and every record of it was forwarded. `host`, `unit`, `pid` and `file` as in `tap`, and the number of `lines`. `truncated`: the process wrote no `proc_end`, so it was killed, for example by the out-of-memory killer, or ended without running its exit handlers; its records stop where it was stopped. `partial`: the length of a last line that never ended, which isn't forwarded. A hook still running when the recording ends has no `spool_end`. |
| `execution` | `execution`: one hook, action or `juju exec` ended, and what it cost; see [execution records](#execution-records). For a process the tap recorded, it comes before the process's `spool_end`. |
| `resources` | `resources`: cumulative counters, written when the session starts and then every 5 seconds. `containers`: one entry per cgroup, the agent's (`charm` on Kubernetes, `agent` on machines) and each workload container's, with `cpu_usec`, `cpu_user_usec`, `cpu_sys_usec`, `throttled_usec`, `mem_bytes` (`memory.current`), `mem_peak_bytes` (`memory.peak`, where the kernel has it), `mem_max_bytes` (the limit, absent when unlimited), `io_read_bytes`, `io_write_bytes`, the pressure stall totals `cpu_stall_usec`, `mem_stall_usec` and `io_stall_usec` (the `some` line), `mem_stat` (`anon`, `file`, `kernel` and `shmem` from `memory.stat`, in bytes) and `mem_events` (`high`, `max`, `oom` and `oom_kill` from `memory.events`), each absent when its file couldn't be read, and `error` when the cgroup couldn't be read. `disks`: the `path`, `size_bytes`, `used_bytes` and `avail_bytes` of the root filesystem and every mount under `/var/lib/juju/storage/`, as the agent's host sees them. `probe_cpu_ms` and `probe_rss_bytes`: the probe's own CPU time so far and current memory. With `burst` true, the record holds only the workload containers, and no disks or probe figures: see [workload bursts](#workload-bursts). |
| `pebble_change` | `pebble_change`: a Pebble change in a workload `container` that started or changed status since the session started: its `id`, `kind` (such as `replan` or `exec`), `summary`, `status`, `ready`, `err`, `spawn` and `ready_time`. Kubernetes only. |

Each session is also listed in `manifest.json` as a source of kind `unit-probe`, named `unit <model>/<host>`, with its start and end times, the number of records, and an `error` saying why it ended if it ended before the recording did.

### Execution records

The probe polls `/proc` for the agent's child processes: every 100 ms while none runs, and every 50 ms while one does. The tap also tells it when a hook process starts and ends, with `proc_start` and `proc_end`. A child with no Juju execution environment, such as the agent's `tmux has-session` check for `juju debug-hooks`, isn't an execution and isn't measured. An `execution` has these fields:

| Field | Meaning |
|---|---|
| `unit`, `kind`, `name` | The unit, and what ran: `hook` and the hook name, `action` and the action name, or `exec` and `juju-exec` for `juju exec`. |
| `env` | The execution's `JUJU_*` variables, as in the tap's `proc_start`. |
| `pid`, `command` | The process the agent started, and its command line. |
| `source` | `tap` when the tap recorded the process; `proc` when only the `/proc` poll saw it, as for charms not built on ops, `juju exec`, and hooks that started before the tap was installed. |
| `tap_file` | The process's spool file, with `source` `tap`. |
| `proc_start` | When the process started, from `/proc`, to 10 ms. |
| `start`, `end` | With `source` `tap`: the times of the tap's `proc_start` and `proc_end`. With `proc`: `proc_start`, and the first poll that found the process gone, up to `end_slack_ms` late. |
| `truncated` | The tap recorded the process but it exited without a `proc_end`; `end` is when the probe found it gone. |
| `overlapped` | Another execution ran under the same agent at the same time, as can happen with several units on one machine. The `cgroup` figures then cover both, and `tree` has no CPU time. |
| `tree` | The process tree: `cpu_user_ms` and `cpu_sys_ms` from the agent's counters of its reaped children, exact for every process of the tree; `peak_rss_bytes`, the largest total resident memory a poll saw; `max_procs`, the most processes alive at once. |
| `cgroup` | The agent's cgroup across the execution: the charm container on Kubernetes, the machine agent's service on machines. `container` names it, `window_start` and `window_end` are the readings the deltas are taken between (when the probe noticed the execution, up to 100 ms after it started, and the first reading after it ended), then `cpu_ms`, `cpu_user_ms`, `cpu_sys_ms`, `throttled_ms`, `mem_base_bytes` (at the start), `mem_peak_bytes`, `mem_peak_source` (how the peak was found: `reset`, `high-water` or `polled`, see [resource attributes](sqlite-schema.md#resource-attributes)), `mem_samples` (how many `memory.current` readings were taken), `mem_max_bytes`, `io_read_bytes`, `io_write_bytes`, `cpu_stall_ms`, `mem_stall_ms` and `io_stall_ms`, and `error`. |
| `workloads` | The same figures for each workload container, read through its Pebble socket; the polled peak comes from the [burst](#workload-bursts) readings. Kubernetes only. |
| `processes` | Each process seen in the tree, up to 200: `pid`, `ppid`, `command`, `start`, `last_seen`, `cpu_ms` and `peak_rss_bytes`. `processes_truncated` counts the rest. |
| `probe_cpu_ms` | The probe's own CPU time during the execution. Taken out of `cgroup` when the `hello` says `probe_in_cgroup`. |
| `linger_ms` | How long the probe kept reading the workload containers in [bursts](#workload-bursts) after the execution, or after the last Pebble change it caused was ready: `record --linger`. Absent when `0`. |

A process that starts and ends between two idle polls, less than 100 ms, has no `execution` unless the tap recorded it.

### Workload bursts

While a hook runs, the probe reads each workload container's cgroup about 10 times a second and writes the readings as `resources` records with `burst` true. When the hook ends, it goes on until `record --linger` (30 seconds by default) after the last Pebble change the hook caused is ready, polling Pebble's changes once a second while one isn't, for at most 5 minutes; then it returns to the 5-second snapshots. A hook that caused no Pebble change stops the bursts as it ends. So a workload that restarts after a replan, and grows as it starts again, is seen in detail.

Pebble logs every file it serves through its file API, twice: a security event and the request. To keep that out of the workload's logs, the probe reads a workload's cgroup through one shell it starts in the container with Pebble's `exec` for the whole session, and asks it for each reading on its standard input. Pebble logs that `exec` once, as about four lines, and shows it as a change, `Execute command "sh"`, which the probe leaves out of its `pebble_change` records. The shell reads the files with its builtins, so a reading starts no process in the workload, and exits when the probe stops or goes away. A workload container without `sh` gets no bursts: its 5-second snapshots, and the readings at each hook's start and end, go through the file API, 11 log lines each.

### Tap records

The `rec` of a `tap` record is what the tap module wrote in the hook process. Every one has `t`, the wall-clock time in nanoseconds since the Unix epoch, `m`, the host's monotonic clock in nanoseconds, and `k`, its kind. Databag values, secret contents, config values and Pebble file contents never appear: hook tool arguments and inputs are cut down to their keys and sizes. Exception messages and a failing hook tool's error output are cut to 300 characters.

| `k` | When | Main fields |
|---|---|---|
| `proc_start` | The tap loaded in a hook process. | `pid`, `ppid`, `uid`, `py` (Python version), `exe`, `argv`, `cwd`, `features` (`proc`, `ops`, and `stacks` unless the recording ran with `--profile=false`), `env` (the hook's `JUJU_*` variables, without the prefix and in lower case), `rss`, `hwm` |
| `ops_loaded` | The charm imported ops. | `version`, `path`, `otel` and `ops_tracing` (whether those were loaded), `patched` |
| `dispatch_start`, `dispatch_end` | ops' `main` started and returned. | `sp`, `dur_ns`, `rc`, and `exc` and `exc_msg` if it raised |
| `reemit_start`, `reemit_backlog` | ops re-emits the deferred events. | `sp`, `count`, `dur_ns` |
| `emit`, `emit_end` | An event is emitted. | `sp`, `ev`, `ev_kind`, `ev_class`, `cls` (`juju`, `action`, `lifecycle` or `custom`), `observers`, `depth`, `in_observer`, `dur_ns`, `deferred` |
| `observer_start`, `observer_end` | ops calls an observer. | `sp`, `obs`, `method`, `ev`, `reemit`, `depth`, `dur_ns`, `deferred`, and `exc` and `exc_msg` if it raised |
| `defer` | An observer deferred its event. | `ev`, `obs` |
| `hook_tool` | ops ran a hook tool. | `tool`, `args`, `via`, `dur_ns`, `rc`, `stdin_bytes` and `stdin_keys`, `out_bytes`, `err_bytes`, and `stderr` if it failed |
| `status_set` | ops set the unit's or the application's status. | `status`, `message`, `app`, `rc` |
| `pebble` | ops called Pebble. | `method`, `path`, `query`, `dur_ns`, `status`, `socket`, `req_bytes`, and the `action`, `services`, `checks` or command name from small request bodies |
| `tap_warn` | The tap couldn't patch part of ops; that part isn't recorded. | `what`, `target` or `module`, and `exc` and `exc_msg` |
| `proc_end` | The hook process exits. | `rc`, and `exc` and `exc_msg` for an uncaught exception, `rss`, `hwm` |
| `stacks` | About once a second while the process runs, and as it exits. | What the stack sampler took since the previous `stacks` record; see [stack samples](#stack-samples) |
| `stacks_end` | The process exits, after its last `stacks` record and before `proc_end`. | `samples` taken, `dropped` (samples whose stack wasn't kept), `interval_ns`, `rss_start` and `rss_peak` in bytes, `cpu_clock` (false when the main thread's CPU clock couldn't be read, so every CPU time is 0), `cpu_ns` (the sampler's own CPU time), and `stopped` if sampling stopped early |

`sp` numbers the span a start record opens, unique within the process: `dispatch_start` opens charm init, which lasts until ops re-emits the deferred events or emits the hook's own event; `reemit_start`, `emit` and `observer_start` open the backlog, the event and the observer call. Each closes with its end record. `0` is the process itself.

The records that open and close spans, from `dispatch_start` to `observer_end`, also have `rss` and `hwm`, as `proc_start` and `proc_end` do: the process's resident memory, from `/proc/self/statm`, and its high-water mark, `ru_maxrss`, in bytes. Reading both costs the hook about 2 microseconds a record.

#### Stack samples

With the `stacks` feature, a thread in the hook process reads the main thread's stack every 10 milliseconds, together with the main thread's CPU clock, the process's resident memory, and the innermost open span, by its `sp`. Samples taken before ops' `main` starts, while Python imports the charm, belong to span `0`. A stack is one call stack within one span: samples of the same stack in two observers are two stacks. The tap's own functions are left out.

Each `stacks` record holds what is new since the previous one, so a process that is killed keeps what it wrote up to about a second before:

| Field | Meaning |
|---|---|
| `m0` | The monotonic clock, in nanoseconds, when sampling started. The same in every record of a process. |
| `f0`, `frames` | Functions seen for the first time, as `[file, function, first line]`. The first gets id `f0`, the next `f0 + 1`, and so on. On Python 3.11 and later, the function name is qualified with its class. |
| `s0`, `stacks` | Stacks seen for the first time, as `[sp, [frame id, ...]]`, innermost frame first, at most 128 frames. Ids continue from `s0` as for frames. |
| `totals` | What the samples since the previous record add to each stack, as `[stack id, samples, wall ns, cpu ns, memory growth bytes]`. Wall time is the time since the previous sample; CPU time is the main thread's; memory growth is how much resident memory grew since the previous sample, so it shows where memory was taken, not where it was held. |
| `tl` | The timeline: every sample, flattened as `time, stack id, RSS` triples. `time` is microseconds since `m0`, RSS is in KiB, and stack id `-1` marks a sample whose stack wasn't kept because the process already had 5000 distinct stacks. |

Sampling stops after an hour, with `stopped` set to `max_duration` in `stacks_end`.
