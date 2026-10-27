# SQLite index schema

The tables in `index.db`, as of schema version 7, apart from the internal `meta` table. [From wire to viewer](../explanation/from-wire-to-viewer.md#the-index) describes how they're filled.

## `models`

One row per Juju model in the recording.

| Column | Meaning |
|---|---|
| `id` | Primary key, referenced by every other table. |
| `name` | Model name. |
| `uuid` | Model UUID, when known. |

## `spans`

One row per RPC: a request paired with its response. With `record --attach unit`, also one row per step of each hook process the tap recorded in a charm: these are the [ops spans](#ops-spans), with `service` set to `ops`.

| Column | Meaning |
|---|---|
| `span_id` | Primary key. |
| `trace_id`, `parent_span_id` | RPC: from Juju's tracing when enabled, otherwise derived from the request. Ops: one trace per hook process; `parent_span_id` links each span to the one it ran in. |
| `model_id` | Which model. |
| `ts_start`, `ts_end` | RPC: when the request was written and the response read. Ops: when the step started and ended. In Unix nanoseconds. |
| `name` | RPC: `<facade>.<method>`, such as `Uniter.CommitHookChanges`. Ops: see [ops spans](#ops-spans). |
| `service` | The agent that made the call, or `ops`. |
| `unit`, `app` | The unit this span belongs to, and its application. |
| `hook` | The hook this span ran inside, if any. For ops spans, the hook's name as the charm sees it, such as `metrics-endpoint-relation-changed`, or `<action> action`. |
| `relation` | The relation this span concerns, if any. Empty for ops spans. |
| `status`, `status_msg` | RPC: status this span set, if any. Ops: `OK` or `ERROR` and the error, empty when the outcome isn't known. |
| `raw_file`, `raw_offset` | Pointer back to the exact line in `raw/rpc/*.jsonl`, or, for ops spans, the line in `raw/unit/*.jsonl` that opened the span. |
| `attrs_json` | RPC: the request and response, as captured. Ops: `kind`, plus the fields of the tap records the span was built from. |

### Ops spans

Each hook process is one tree. `attrs_json` has a `kind`, and the fields of the [tap records](recording-layout.md#tap-records) the span came from, except their clocks and kind: the tap's `ev`, `obs`, `args`, `rc`, `exc`, `exc_msg` and so on.

| `kind` | `name` | One per | Notable attributes |
|---|---|---|---|
| `root` | The hook's name, `<action> action`, `python <script>`, or `juju-exec` | Process | `exec`: `hook`, `action`, `exec` for `juju exec`, or `process` for any other Python process, such as one a hook started. `hook_kind`: the hook without its endpoint, storage or container prefix, such as `relation-changed`. `env`: the process's `JUJU_*` variables. `rc`, `exc`, `exc_msg`. `ops_version`. `truncated`: the process's records stop before its `proc_end`. `open`: it was still running when the recording ended. `tap`: `truncated` when the records stop before `proc_end`, or `missing` for a [hook the tap missed](#hooks-the-tap-missed). `tap_reason`: why, when known. `tap_warn`: what the tap couldn't patch. The [resource attributes](#resource-attributes), when the unit probe measured the process. |
| `init` | `charm init` | Process | From `ops.main` to the deferred events: the charm's construction. `exc` when the charm raised there. |
| `backlog` | `deferred events` | Process | `count`: deferred events re-emitted. |
| `event` | The event kind, such as `config_changed` | Event | `ev`, `ev_class`, `cls`, `observers`. `deferred`: an observer deferred it. `reemitted`: a deferred event re-emitted from the backlog. |
| `observer` | The observer, such as `charm.MyCharm._on_config_changed` | Observer call | `method`, `ev`, `reemit`, `deferred`, `exc`. |
| `tool` | The hook tool, such as `relation-get` | Call | `args`, `rc`, `out_bytes`. A `status-set` also has `status`, `message`, and `app`. |
| `pebble` | `<method> <path>`, such as `POST /v1/layers` | Call | `status` (HTTP), `socket`, `services`, `action`, `command`. |

A span the process never closed, because it was killed or the recording ended, ends at the process's last record and has `unfinished` set. A Python process a hook started is a `root` in its parent's trace, under the parent's root.

The `root`, `init`, `backlog`, `event` and `observer` spans also have the process's memory, in bytes, as strings:

| Attribute | Meaning |
|---|---|
| `rss_start_bytes`, `rss_end_bytes` | The process's resident memory as the span opened and closed. `rss_end_bytes` is absent for an `unfinished` span. |
| `rss_peak_bytes` | The highest of the start, the end, and the [stack samples](#profile_samples) taken within the span, every 10 ms. |
| `rss_hwm_exact` | `true` when the process's high-water mark (`ru_maxrss`) rose within the span: `rss_peak_bytes` is then that mark, exact however short the peak. Otherwise a peak between two samples is missed. |

So `rss_end_bytes` minus `rss_start_bytes` is how much memory an observer kept, and `rss_peak_bytes` minus `rss_start_bytes` how much it took at most. Recordings made before these attributes existed don't have them.

A process with `tap` set to `truncated` has one of these `tap_reason` values:

| `tap_reason` | Meaning |
|---|---|
| `it exited without proc_end: killed (SIGKILL, out of memory) or ended without running its exit handlers` | Its `spool_end` record says `truncated`. |
| `its unit probe session ended before it did, as when the pod is deleted` | A new probe session started on the host before the process's spool file was complete. |

#### Hooks the tap missed

When the recording is indexed, each hook or action that ran on a unit a unit probe covered, but that the tap recorded nothing of, has a `root` span with no children. Its `attrs_json` has `kind` `root`, `exec` (`hook` or `action`; `juju exec` is the action `juju-exec`), `hook_kind` for a hook, `tap` set to `missing`, `tap_reason` when the reason is known, and `seen_by`. With `seen_by` `proc`, the unit probe's `/proc` poll found the process: the span starts when the process did, ends up to `res_end_slack_ms` after it, and has `env`, `pid`, `command` and the [resource attributes](#resource-attributes) with `res_source` `proc`, but no `rc`; the probe writes it as the process ends. Otherwise `seen_by` is `status-log` when the unit's [status log](recording-layout.md#raw-data) lists it, `rpc` when a `run-hook` marker brackets it, or both, comma-separated, and the span is added when the recording is indexed: it starts when the status log entry or the marker does, and ends at the unit's next status log entry or at the marker's `continue`. `status_code` is `ERROR` when the status log says the hook failed. `raw_file` is the status log, or the RPC file of the marker.

| `tap_reason` | Meaning |
|---|---|
| `juju exec runs commands, not the charm` | It's the action `juju-exec`, which `juju exec` runs. |
| `the charm has no venv: it isn't built on ops` | The probe found no venv in the charm. |
| `the tap can't be installed: <reason>` | The probe skipped the charm, for example because it has its own `sitecustomize.py`. |
| `the tap couldn't be set up on the host: <reason>` | The probe couldn't create the spool. |
| `the tap wasn't installed yet: the pod had just started` | The unit's agent started during the recording, and the hook ran before the probe installed the tap in the new venv: on Kubernetes, the first hooks in a pod a refresh recreated. |
| `the tap wasn't installed yet` | The hook ran before the probe installed the tap. |
| `the tap wasn't back yet in the venv a refresh replaced` | A refresh replaced the venv, and the hook ran before the probe put the tap back. |
| `the unit probe hadn't started yet` | The hook ran before the unit's first probe session. |
| `no unit probe session was running` | The hook ran between two probe sessions. |
| `the unit probe had stopped` | The hook ran after the unit's last probe session. |
| `the tap had been removed` | The hook ran after the probe removed the tap as the recording ended. |

Without a `tap_reason`, the tap was in place and the reason is unknown.
## Profiles

With `record --attach unit`, unless it ran with `--profile=false`, the tap samples each hook process's Python stacks every 10 milliseconds; see [stack samples](recording-layout.md#stack-samples). Four tables hold them, keyed by the process's `root` [ops span](#ops-spans). Each stack belongs to the ops span it ran in: the innermost of the observer, event, deferred-events backlog, charm init, or the process itself, `root`, for what ran before ops started, such as importing the charm.

### `profiles`

One row per hook process that took samples.

| Column | Meaning |
|---|---|
| `root_span_id` | Primary key: the process's `root` span. |
| `model_id`, `unit` | Which model and unit. |
| `ts_start` | When sampling started, as Python started, in Unix nanoseconds. |
| `interval_ns` | Time between samples, 10 ms. `0` when `complete` is `0`. |
| `samples`, `dropped` | Samples taken, and how many of them have no stack because the process already had 5000 distinct stacks. When `complete` is `0`, counted from `profile_samples`. |
| `rss_start`, `rss_peak` | The process's resident memory when sampling started, `0` when `complete` is `0`, and the highest sampled, in bytes. |
| `sampler_cpu_ns` | CPU time the sampler itself used in the process. |
| `complete` | `1` if the process wrote its last samples as it exited. `0` if it was killed, or still ran when the recording ended: the samples stop about a second before its last record. |

### `profile_frames`

The functions a process's samples saw.

| Column | Meaning |
|---|---|
| `root_span_id`, `frame_id` | Primary key: the process, and the frame's id within it. |
| `file`, `func`, `line` | The Python source file, the function, qualified with its class on Python 3.11 and later, and its first line. |

### `profile_stacks`

One row per distinct call stack within one span, with what its samples add up to.

| Column | Meaning |
|---|---|
| `root_span_id`, `stack_id` | Primary key: the process, and the stack's id within it. |
| `span_id` | The ops span the samples ran in. Indexed. |
| `frames_json` | The stack, as a JSON array of `frame_id`s, innermost first. |
| `samples` | How many samples saw this stack. |
| `wall_ns`, `cpu_ns` | Wall time and the main thread's CPU time, each since the sample before. |
| `mem_bytes` | How much resident memory grew between the sample before and each sample of this stack: where memory was taken, not where it was held. |

### `profile_samples`

Every sample, in time order: the timeline that leads from a moment, such as a memory peak, to the stack running then.

| Column | Meaning |
|---|---|
| `root_span_id`, `seq` | Primary key: the process, and the sample's position in it. |
| `ts` | When the sample was taken, in Unix nanoseconds. |
| `stack_id` | The stack in `profile_stacks`; `NULL` for a dropped sample. |
| `rss_bytes` | The process's resident memory, in bytes. |

For example, the observer, event or init span and the innermost function running at each process's memory peak:

```sql
SELECT s.name AS span, f.func, f.file, f.line, p.rss_bytes
FROM profile_samples p
JOIN profile_stacks st USING (root_span_id, stack_id)
JOIN spans s ON s.span_id = st.span_id
JOIN profile_frames f ON f.root_span_id = p.root_span_id
                     AND f.frame_id = json_extract(st.frames_json, '$[0]')
WHERE p.rss_bytes = (SELECT max(rss_bytes) FROM profile_samples WHERE root_span_id = p.root_span_id);
```

### Resource attributes

What one execution cost, from the unit probe's [execution record](recording-layout.md#execution-records), as attributes of its `root` span. Times are in milliseconds, sizes in bytes, all as strings.

| Attribute | Meaning |
|---|---|
| `res_source` | `tap`: the span's start and end are the tap's `proc_start` and `proc_end`. `proc`: only the `/proc` poll saw the process; the start is the process's start time, and the end up to `res_end_slack_ms` late. |
| `res_proc_start` | When the process started, from `/proc`, as RFC 3339. |
| `res_end_slack_ms` | How late the end can be, when it came from a poll. |
| `res_overlapped` | `true` when another execution ran under the same agent at the same time; the `res_cgroup_*` figures cover both, and `res_cpu_*` is 0. |
| `res_truncated` | `true` when the tap recorded the process and it exited without a `proc_end`. |
| `res_cpu_ms`, `res_cpu_user_ms`, `res_cpu_sys_ms` | CPU time of the process and everything it started, exact. |
| `res_peak_rss_bytes`, `res_max_procs` | The largest total resident memory of the process tree, and the most processes alive at once, as polled. |
| `res_cgroup` | The cgroup the next figures are for: `charm` (the charm container) on Kubernetes, `agent` (the machine agent's service, shared by every unit on the machine) on machines. |
| `res_cgroup_cpu_ms`, `res_cgroup_cpu_user_ms`, `res_cgroup_cpu_sys_ms`, `res_cgroup_throttled_ms` | CPU time used in the cgroup, and time it was throttled by its CPU limit, over the window. Includes the agent's own work for the hook; excludes the probe's. |
| `res_cgroup_mem_base_bytes`, `res_cgroup_mem_peak_bytes`, `res_cgroup_mem_max_bytes`, `res_cgroup_mem_samples` | Memory at the start of the window, the peak during it, the limit (absent when unlimited), and how many `memory.current` readings were taken. |
| `res_cgroup_mem_peak_source`, `res_cgroup_mem_peak_exact` | How `res_cgroup_mem_peak_bytes` was found. `reset`: the cgroup's `memory.peak`, reset as the probe noticed the execution (Linux 6.12 and later, where the probe can write the cgroup filesystem: machines, not Kubernetes, which mounts it read-only). `high-water`: the cgroup's lifetime `memory.peak` rose during the window, so its new value is the peak. `polled`: the largest `memory.current` reading, about 20 a second, which misses a shorter peak. `res_cgroup_mem_peak_exact` is `false` only for `polled`. Absent in recordings made before. |
| `res_cgroup_io_read_bytes`, `res_cgroup_io_write_bytes` | Bytes read and written. |
| `res_cgroup_cpu_stall_ms`, `res_cgroup_mem_stall_ms`, `res_cgroup_io_stall_ms` | Pressure stall time (the `some` totals). |
| `res_cgroup_window_start`, `res_cgroup_window_end` | The counter readings the figures are taken between, as RFC 3339: when the probe noticed the process, up to 100 ms after it started, and the first after it ended. |
| `res_cgroup_error` | Why the cgroup couldn't be read. |
| `res_workloads` | JSON: the same figures for each workload container, as in the execution record's `workloads`, with `mem_peak_source` `high-water` or `polled` (from the burst readings, 10 a second). Kubernetes only. |
| `res_processes`, `res_processes_truncated` | JSON: the processes seen in the tree, as in the execution record's `processes`, and how many more there were. |
| `res_probe_cpu_ms` | The probe's own CPU time during the execution. |
| `res_linger_ms` | The execution record's `linger_ms`: how long after the execution, or after the last Pebble change it caused was ready, the probe kept sampling workloads densely. |

## `snapshots`

State at a point in time, extracted from spans or from `raw/status/`.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `ts` | Which model, and when this snapshot was taken. |
| `kind` | `app-status`, `unit-status`, `agent-status`, `databag`, `config`, or `leadership`. |
| `scope` | What the snapshot describes, such as `unit-status:grafana/0`. |
| `body_json` | The snapshot's content. |
| `content_hash` | Hash of `body_json`; deduplicates no-op re-writes. |
| `origin` | `rpc` if extracted from a span, `bootstrap` if from `raw/status/`. |
| `producing_span_id` | The span that produced this snapshot, if any. |

## `log_records`

One row per ingested log line, from any source.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `ts` | Which model, and when. |
| `source` | `debug-log`, `k8s`, or `journal`. |
| `entity`, `unit` | The raw Juju entity tag, and the unit it resolves to. |
| `level`, `module` | Log level and module. |
| `body` | The log line's text. |
| `trace_id`, `span_id` | The trace and span the line belongs to, when the log source wrote them. |
| `raw_file`, `raw_line` | Pointer back to the source file. |

## `resource_samples`

One row per cgroup per [`resources` record](recording-layout.md#unit-probe-records) of a unit probe: every 5 seconds, and for each workload container about 10 times a second while a hook runs and for `record --linger` after the last Pebble change it caused is ready. Counters are cumulative; subtract two rows of the same `host` and `container` to get usage over the interval.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `ts` | Which model, and when, in Unix nanoseconds. |
| `host` | The pod (`otelcol-0`) or machine (`machine-3`) the probe ran on. |
| `unit` | The host's unit, when it has only one: always on Kubernetes. |
| `container` | `charm` (the charm container) on Kubernetes, `agent` (the machine agent's service) on machines, a workload container's name, or `juju-lens-probe` for the probe itself, which fills only `cpu_usec` and `mem_bytes`. |
| `cpu_usec`, `cpu_user_usec`, `cpu_sys_usec` | CPU time, in microseconds. |
| `throttled_usec` | Time the cgroup was throttled by its CPU limit, in microseconds. |
| `mem_bytes` | Current memory (`memory.current`); for the probe, its resident memory. |
| `mem_peak_bytes` | `memory.peak`, the most the cgroup has used; `NULL` on kernels without it. |
| `mem_max_bytes` | The memory limit; `NULL` when unlimited. |
| `io_read_bytes`, `io_write_bytes` | Bytes read and written. |
| `cpu_stall_usec`, `mem_stall_usec`, `io_stall_usec` | Pressure stall time (the `some` totals), in microseconds. |
| `mem_anon_bytes`, `mem_file_bytes`, `mem_kernel_bytes`, `mem_shmem_bytes` | Where the memory is, from `memory.stat`: anonymous memory (heaps and stacks), page cache (files read and written, `shmem` included, which the kernel can reclaim), kernel memory, and tmpfs and shared memory. `NULL` where `memory.stat` couldn't be read, in the probe's own rows, and in recordings made before. |
| `mem_high_events`, `mem_max_events`, `mem_oom_events`, `mem_oom_kill_events` | Cumulative `memory.events` counters: reclaims forced by `memory.high`, times usage hit `memory.max`, allocations that failed there, and processes the OOM killer killed. `NULL` as above. |
| `error` | Why the counters couldn't be read, if they couldn't. |

For example, a workload's memory split into heap and page cache around each Pebble change, from the burst rows:

```sql
SELECT r.ts, r.mem_bytes, r.mem_anon_bytes, r.mem_file_bytes, c.kind, c.summary
FROM resource_samples r
LEFT JOIN pebble_changes c
  ON c.host = r.host AND c.container = r.container AND c.ts_spawn BETWEEN r.ts - 100000000 AND r.ts
WHERE r.container = 'otelcol'
ORDER BY r.ts;
```

## `disk_samples`

One row per filesystem per `resources` record: the root filesystem and Juju storage mounts.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `ts` | Which model, and when. |
| `host`, `unit` | As in `resource_samples`. |
| `path` | Mount point. |
| `size_bytes`, `used_bytes`, `avail_bytes` | Filesystem size, used space, and space available. |

## `pebble_changes`

One row per Pebble change in a workload container, such as a replan, a service restart, or an `exec`, that started or changed status while the probe ran. Kubernetes only.

| Column | Meaning |
|---|---|
| `id` | Primary key. |
| `model_id`, `host`, `unit` | As in `resource_samples`. |
| `container` | The workload container. |
| `change_id`, `kind`, `summary`, `status`, `err` | The change as Pebble last reported it. |
| `ts_spawn`, `ts_ready` | When the change started and finished, in Unix nanoseconds; `ts_ready` is `NULL` until it finishes. |
