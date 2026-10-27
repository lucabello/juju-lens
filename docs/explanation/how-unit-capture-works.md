# How unit capture works

`juju-lens record --attach unit` records what happens inside each charm while it runs a hook: which events ops emitted, which observers ran and for how long, the hook tools and Pebble calls they made, what the hook cost in CPU and memory, and where its Python spent that time. This page describes how that works, what it changes on the units, what it costs them, and what it can't see. [How to trace a charm's memory spike to its code](../how-to/trace-a-memory-spike.md) shows how to use it.

## Why capture from inside the unit

[Host capture](how-capture-works.md) reads the agents' API traffic with eBPF, which needs root or `CAP_BPF` on the host where the agents run. A user of a remote Kubernetes cluster, or of a cloud they don't administer, has neither. And the agent's traffic shows a hook as one unit of work: `juju-lens` sees that `config-changed` ran and what it wrote, not what the charm did in between.

What a model's administrator does have is `juju ssh`. On Kubernetes it opens a shell in the unit's charm container, and on machines a shell with `sudo` next to the machine agent. From there, a program can see the hook processes, read the container's cgroup, talk to the workload containers' Pebble, which also serves their logs, and write files in the charm's directory. Unit capture is built from those four things alone.

Sampling profilers such as `py-spy` attach to another process with `ptrace`, and eBPF or `perf` need capabilities beyond the container's defaults. Unit capture uses none of them. Instead of observing the charm's Python from outside, it puts a small module into it.

## The probe in each unit

Only one recording can own a unit at a time. The probe takes a lock file, `/tmp/juju-lens-unit.lock`, and refreshes its heartbeat every 5 seconds. A second recording finds it held and leaves the unit alone, trying again 2 minutes later. A holder whose heartbeat is more than 30 seconds old was left by a recorder that went away without its probe noticing: the next probe stops it and takes the lock over.

The probe is the same program as `juju-lens`'s own probe for host capture, built static for `amd64` and `arm64` and carried inside `juju-lens`. The recorder asks each host its architecture over the session, then uploads the matching build to `/tmp`, where the probe deletes its own file as it starts. The probe writes JSON lines to its standard output, which is the `juju ssh` session, and the recorder stores them under `raw/unit/`. When the recording ends, the session closes, the probe's next write fails, and it cleans up and exits. The same happens if the recorder is killed: in the lab, a unit was clean about 22 seconds after its recorder got `SIGKILL`.

## Getting into the charm's Python

Charms built with `charmcraft` run from a virtualenv in the charm directory, and their `dispatch` script starts that virtualenv's Python. When Python starts, it runs every `.pth` file in its `site-packages`. The probe installs two files there in every charm venv on the host:

- `_juju_lens_tap.py`, the *tap*. It uses only the standard library, and runs on Python 3.8 to 3.14.
- `_juju_lens_tap.pth`, one line that imports the tap and ignores any error in doing so.

Charms built with an older `charmcraft` keep their packages directly in the venv directory and put it on `PYTHONPATH`, where Python doesn't read `.pth` files. For those, the probe writes a `sitecustomize.py` that loads the tap and then the `sitecustomize` the charm would otherwise have loaded. If the charm already ships its own `sitecustomize.py` there, the probe leaves it alone and the charm isn't tapped. A charm with no venv isn't built on ops, and gets no tap either; the probe reports why in a `loader` record.

The charm's own files aren't changed. Every second, the probe checks that its files are still in every venv and writes them back if they aren't. On machines, a refresh that replaced the venv is therefore tapped again within about a second; on Kubernetes, a refresh recreates the pod, and the new pod gets a new probe (see [limitations](#limitations)). Hooks that ran before the tap was back were not tapped, and the probe records when the venv changed, so the viewer can say why they're missing.

## The gate

Installing files into a charm is only safe if they can't do anything when they shouldn't. The tap checks four things as it's imported, and does nothing at all unless all four hold:

- The process has a Juju hook environment: `JUJU_DISPATCH_PATH`, `JUJU_HOOK_NAME`, `JUJU_ACTION_NAME` or `JUJU_CONTEXT_ID`. A charm's tests, or `python3` run by hand in the container, are left alone.
- The spool directory, `/tmp/juju-lens-tap`, exists, is a real directory and not a symlink, is owned by the process's user, and has mode `0700`. The probe creates it owned by the agent's user, so only hook processes can write to it.
- The spool's heartbeat file was touched in the last 30 seconds. The probe touches it every second, so the files a dead probe left behind go inert by themselves.
- The spool's `config.json` turns on at least one feature: `proc`, `ops`, or `stacks`.

Each hook reads the config once, as its Python starts, so a hook's features never change half-way, and the probe can change them from one hook to the next. `record --profile=false` writes a config without `stacks`.

Once active, the tap never raises into the charm, never writes to standard output or standard error, and never calls `juju-log`. Every patch it applies is wrapped, so a failure in it leaves that part of ops unrecorded, writes a `tap_warn` record, and lets the hook go on as it would have.

## What the tap records

The tap patches a few internals of ops as their modules are imported, before the charm can bind their names: the framework's event emission and re-emission, observer registration and `defer`, Pebble's request function, the module-level `subprocess` that ops uses for hook tools, and `main`. ops' own tracing (`ops[tracing]`) isn't used: it records nothing unless the charm installs `ops_tracing`, its spans don't say when an observer deferred, and installing a tracer provider of its own would replace the charm's. A charm that sets up its own ops tracing keeps exporting the same spans with the tap active.

Each record is a JSON line, written with one `write(2)` to the hook process's own file in the spool, opened for appending. A reader therefore never sees half a record. The kinds, grouped by feature:

- **`proc`**: `proc_start` when the tap loads, with the process, its Python version and its `JUJU_*` environment; `proc_end` from Python's exit handlers, with the exit code and any uncaught exception. A process killed with `SIGKILL`, including by the out-of-memory killer, never writes `proc_end`.
- **`ops`**: which ops version loaded; `ops.main` starting and ending, with its exit code and exception; deferred events re-emitted before the hook's own event; each event emitted, with its class and the observers it reached; each observer call, its duration, and whether it deferred or raised; every hook tool call; every `status-set`; every Pebble call.
- **`stacks`**: stack samples, described [below](#profiling).

The records that open and close spans also carry the process's resident memory, from `/proc/self/statm`, and its high-water mark, `ru_maxrss`, so each span has its memory at start, at end, and at peak. Reading both took about 2 microseconds on Python 3.12 in an `opentelemetry-collector-k8s` charm container.

The recorder's index turns each process's records into a tree of spans: the process; *charm init*, from `ops.main` to the first event, which is the charm's `__init__` and a little ops setup around it, and which the viewer shows as the `__init__` function when the hook was profiled; the re-emitted backlog; each event, with its observers under it; and each hook tool and Pebble call under the span that made it. A Python subprocess that loads the tap joins its parent's tree. [From RPCs to hooks](rpcs-to-hooks.md#what-the-viewer-shows) describes how the viewer shows these trees, and the [recording layout](../reference/recording-layout.md#tap-records) lists every field.

### Redaction

Hook tools carry relation data, secrets and configuration, and Pebble calls carry files and service commands. Databag values, secret contents, configuration values and Pebble file contents never reach a record. A hook tool's `key=value` arguments and its input are cut down to their keys and byte sizes, its output and a `juju-log` message to their byte sizes; other arguments, such as keys, unit names and secret IDs, are kept, and `status-set` and `action-fail` keep their messages. Pebble requests keep their method, path and query, and from small JSON bodies only the action, the service and check names, and an `exec`'s command name. Exception messages, and the error output of a hook tool that failed, are kept, cut to 300 characters, because they're what explains a failed hook; a charm that puts a secret in an exception message puts it in the recording too.

## From the spool to the recording

The probe watches the spool with `inotify`, and forwards each complete line as soon as a hook writes it, with the unit and the hook's process. It never forwards a partial line. Once a hook process has exited and its file has been read to the end, the probe deletes the file. If the process wrote no `proc_end`, the probe reports it as *truncated*: it was killed, and its records stop where it stopped.

On a Kubernetes unit and an LXD machine, a line reached the recorder's disk a median of about 0.5 ms after the hook wrote it, and at most 2.8 ms (`opentelemetry-collector-k8s` units and a machine running an `ubuntu` charm with an `opentelemetry-collector` subordinate, 1167 records).

## Workload logs on Kubernetes

With `--k8s-log`, which is on by default, the probe in a Kubernetes unit also follows the logs of every workload container. Each workload container's Pebble keeps the output of its services, and serves it with `GET /v1/logs` on the same socket that Juju shares with the charm container, so no `kubectl` or cluster access is involved. The probe follows each container's stream, takes up a dropped stream where it left off without repeating lines, and writes each line as a `pebble_log` record. The recorder doesn't keep those records in the probe's file: it writes each line under `raw/k8s/<model>/<pod>/<container>/`, in the shape `kubectl logs --timestamps` gives for a container that runs under Pebble, so the viewer and the index treat both alike.

A recording of a Kubernetes model without `--attach unit` gets the same logs from `kubectl logs`, which needs `kubectl` pointing at the cluster that runs the model. The two differ in what they hold: Pebble's log is the services' output only, while `kubectl logs` also has Pebble's own lines, such as its access log. The Pebble calls the charm itself makes are in the trace instead.

## Measuring what a hook costs

The probe doesn't rely on the tap alone to find hooks. It polls `/proc` for children of the unit agent, every 100 ms while nothing runs, reading only the agent's `cgroup.procs`, and every 50 ms while something does. When the tap is active, its `proc_start` and `proc_end` give each hook's exact start and end; for what the tap can't see, such as charms not built on ops, `juju exec`, and hooks that ran before the tap was installed, the poll does, and each execution records which source found it.

For each execution, the probe measures:

- **The process tree.** When the agent reaps a finished child, the kernel adds the child's CPU time, and that of everything it waited for, to the agent's counters of its children. Their change across the execution is the tree's exact CPU time, including hook tools and subprocesses too short for any poll to see.
- **The agent's cgroup.** On Kubernetes this is the charm container, which runs one hook at a time. Its cgroup v2 counters give CPU time, throttling, memory, IO and pressure stalls across the hook, and `memory.stat` splits the memory into anonymous memory (the heap), page cache, kernel memory and shared memory, so a page cache that grew can be told from a heap that did. On Linux 6.12 and later, where the cgroup filesystem is writable, the probe resets `memory.peak` as each hook starts, so the hook's peak is exact. Kubernetes mounts the cgroup filesystem read-only: there the peak is exact only when the hook set a new high for the container, and otherwise the highest of the probe's readings. On machines, the agent's cgroup is the machine agent's service, shared by every unit on the machine, so hooks of different units that overlap share their figures and are marked as overlapped.
- **The workload containers.** A workload container's cgroup isn't visible from the charm container, but its Pebble is. The probe starts one `sh` in each workload container through Pebble's `exec`, keeps it for the session, and reads the container's cgroup files through it. It doesn't use Pebble's file API for this because Pebble logs every file it serves, twice, into the workload's own logs; the shell costs about four log lines per session instead. While a hook runs, and for `record --linger` (30 seconds by default) after the last Pebble change it caused is ready, the probe reads each workload's memory about 10 times a second. A workload that restarts after a replan and grows as it starts again is therefore seen in detail, after the hook that caused it has ended. Workloads on machines run as their own services, outside the agent's cgroup, and aren't measured.

Every 5 seconds the probe also records each cgroup's counters, the disk usage of the root filesystem and Juju storage, its own CPU and memory, and any new Pebble changes. When the probe runs in the agent's cgroup, as it does in a Kubernetes charm container, it takes its own CPU time out of the agent's figures.

## Profiling

With the `stacks` feature, which is on unless the recording runs with `--profile=false`, the tap starts one background thread in each hook process. Every 10 milliseconds the thread reads the main thread's Python stack, the main thread's CPU clock, the process's resident memory, and the innermost span open at that moment. It attributes three things to the stack:

- **Wall time**, the time since the previous sample. This includes waiting: on Pebble, on hook tools, on the network.
- **CPU time**, the main thread's own. The sampler thread isn't included.
- **Memory growth**, how much resident memory grew since the previous sample. Memory freed doesn't count, so this shows where memory was taken, not where it was held.

Because each sample carries its span, a profile belongs to a span rather than to a time window: the profile of an observer has only that observer's samples, and samples taken before the first event belong to charm init, where a charm that reconciles in `__init__` spends most of its hook. Samples taken while Python imports the charm, before ops starts, belong to the process.

Samples with the same span and stack add up as the process runs, so the size of a profile depends on how many distinct stacks there are, more than on how long the hook ran. The thread also keeps a timeline of every sample's time, stack and resident memory, which lets the viewer go from a point on a memory curve to the stack running at that moment. About once a second, and at exit, it writes what's new, so a hook that's killed keeps what it wrote up to about a second before. To keep profiles bounded, stacks are cut at 128 frames, a process keeps at most 5000 distinct stacks (it still counts the samples of the rest), and sampling stops after an hour.

A stack sample says what was running at that instant, not every call made. A function that runs for less than 10 milliseconds at a time can be missed, or charged a whole interval it only partly used. Over a hook that runs for seconds, the totals are accurate to a few samples.

## What it costs

Each figure below was measured on the deployment named, by comparing hooks with and without the part being measured. Run-to-run variation of hook durations is large, because hook tools go to the controller, so small differences aren't significant.

| What | Cost | Measured on |
|---|---|---|
| The tap, without stacks | CPU of an `update-status` hook, including its hook tools, +14.9 ± 8.8 ms (95% interval) on a median of 894 ms (+1.7%); wall time within variation; peak memory +0.3 MB. One `update-status` wrote 111 records, 24 KB. | `opentelemetry-collector-k8s` (ops 3.7.1, Python 3.12) on MicroK8s, `update-status` dispatched 40 times with the tap and 40 times inert, alternating. |
| Stack sampling | `config-changed` took a median of 2170 ms with sampling and 2112 ms without (+58 ms, +2.8%), within run-to-run variation. The sampler used a median of 38 ms of CPU per hook and wrote about 54 KB. | `opentelemetry-collector-k8s` on MicroK8s, 30 hooks with sampling and 18 without. |
| The probe, between hooks | 0.55-0.65% of one CPU. | `opentelemetry-collector-k8s` and a Bash charm on MicroK8s, over 71-91 s with no hooks. |
| The probe, during hooks | 17.4-17.9 ms of CPU per second of hook, 2.5% of the hooks' own CPU; 12-14 MB of memory. | Four COS charms on MicroK8s, `update-status` every 15 s. |

On the recorder's side, each probed unit, or machine, keeps one `juju ssh` session open for the whole recording. On Kubernetes, `juju ssh` goes through the controller and the Kubernetes API, so a recording of many units holds that many sessions through the controller. In the workload containers' logs, the probe adds about four lines for its shell per session, and Pebble's own log of the probe listing changes every 5 seconds.

## What's left behind

When the recording ends, each probe removes the tap, its loader, the bytecode Python cached for them, the spool, and its lock. In the lab, a listing of every file in each venv, with its checksum, was identical before the recording and after it. On Kubernetes, removal can take up to about 30 seconds, until `juju ssh` tears the session down; a new recording of the same unit started in that time finds it busy.

If the probe itself dies before it can clean up, for example because it was killed with `SIGKILL`, the tap and its loader stay in the venvs, and the spool and lock may stay in `/tmp`. They do nothing: without a heartbeat less than 30 seconds old the tap is inert, and the hook runs exactly as it would without `juju-lens`. The next `--attach unit` recording on that unit removes them, and on Kubernetes so does the next refresh, which recreates the pod. `juju ssh` itself leaves a small `/tmp/<random>.pid` file in the charm container for each session on Kubernetes.

## Failure modes

- **A patch target is missing**, because ops changed: the tap writes a `tap_warn` and records the rest. `just ops-matrix` runs the tap against ops 1.5.5 to 3.9.0 to catch this before a release.
- **A hook process is killed** (`SIGKILL`, out of memory): no `proc_end`, so the hook is shown as truncated, with the events and observers it reached.
- **A session is lost**, for example because the pod was recreated: the recorder reports it and starts a new one; a hook that was running on that unit is shown as truncated.
- **A hook isn't tapped** (charm not built on ops, `juju exec`, a hook before the tap was installed): the probe's poll still measures it unless it's shorter than one poll, the unit's status log is checked when the recording stops, and the hook is shown as not instrumented, with the reason. See [hooks the tap missed](rpcs-to-hooks.md#hooks-the-tap-missed).
- **Another recording owns the unit**: it's skipped and retried every 2 minutes.

## Limitations

- **Only charms built on ops get the event tree.** Others, and `juju exec`, get hook boundaries and resource figures from the probe's poll, and no events, observers or stacks.
- **The first hooks after a Kubernetes refresh are lost.** A refresh recreates the pod, and with it the venv. The new pod's first hooks, often `upgrade-charm`, run before the probe in the new pod has installed the tap. In one lab run, `upgrade-charm`, `config-changed` and `start` ran untapped, and the tap was installed 9.6 seconds after the new agent started. These hooks are marked as not instrumented, with the exceptions listed in [hooks the tap missed](rpcs-to-hooks.md#hooks-the-tap-missed).
- **Not visible by design:** Juju's side of the hook, such as the agent's `run-hook` markers, which show which hook it runs next, and the agent's status (use `--attach host`); relation data changes on the other side that the charm never reads; and what a subprocess that isn't the charm's Python does, beyond its CPU and memory in the process tree.
- **Killed hooks are truncated.** A hook killed by `SIGKILL` or the out-of-memory killer has no `proc_end`; it's marked truncated rather than guessed at.
- **ops internals, not a public API.** The tap patches ops' internals, which have been stable from 1.5.5 to 3.9.0. On a newer ops, a part of the tree may be missing, with a `tap_warn` record saying which; the [project README](../../README.md#develop) describes how maintainers check each new release.
- **Machine models share one cgroup.** Several units on one machine share the machine agent's cgroup, and their workloads aren't measured.
