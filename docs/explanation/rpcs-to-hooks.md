# From RPCs to hooks

A recording holds thousands of RPCs, but a charm author thinks in hooks: `install`, `config-changed`, `ingress-relation-joined`. This page describes how `juju-lens` turns spans, the single RPCs described in [from wire to viewer](from-wire-to-viewer.md), into the hooks, state changes, and log lines the viewer shows. The same rules apply to every recording, captured or synthetic; `juju-lens export` uses them too.

## Finding hooks

Before the unit agent runs a hook, it saves its current operation to the controller with a `Uniter.SetState` call. The payload is a short YAML document:

```yaml
op: run-hook
opstep: pending
hook:
  kind: relation-joined
  remote-application: prometheus
```

When the hook finishes, the agent saves its state again, this time with `op: continue`. The `run-hook` and `continue` markers bracket the hook. Any other RPC that the same unit makes between them, such as a status change, a relation-data read, or the final `CommitHookChanges`, belongs to that hook.

`juju-lens` replays each unit's `SetState` calls in time order to find these brackets. Each bracket becomes a *hook run* with the unit, the hook kind, a start and end time, and the list of spans inside it. This works without the charm's help: the agent has written down which hook it's running before the charm process has started.

The agent saves `run-hook` more than once for the same hook, as `opstep` moves from `queued` to `pending` to `done`. A `run-hook` for the hook that is already open is a step, not a new hook.

If the recording started in the middle of a hook, its opening `run-hook` is missing. `juju-lens` then falls back to grouping consecutive spans from the same unit that carry the same hook label.

The `kind` in the marker is generic: `relation-joined`, not `ingress-relation-joined`. To show the name the charm sees, `juju-lens` adds the endpoint:

- **Relation hooks:** the marker names the remote application. Other spans in the recording name the relation, as `<app>.<endpoint>#<app>.<endpoint>`, which gives the local endpoint for that pair of applications.
- **Storage hooks:** the marker carries the storage ID, such as `data/0`, and the storage name is the part before the slash.

## State changes

Some spans change state that's worth tracking over time. The indexer extracts these as *snapshots*, each with a timestamp, a scope (what it describes), and a body (the value). [The SQLite schema reference](../reference/sqlite-schema.md#snapshots) lists the fields.

| Snapshot kind | Scope | Extracted from |
|---|---|---|
| `unit-status` | A unit | `Uniter.SetUnitStatus` or `Uniter.SetStatus`: the workload status (`active`, `blocked`, ...). |
| `agent-status` | A unit | `Uniter.SetAgentStatus`: the agent status (`executing`, `idle`, ...). |
| `app-status` | An application | `Uniter.SetApplicationStatus`. The leader sets this directly; it isn't derived from unit statuses. |
| `databag` | A relation and a unit or application | `Uniter.CommitHookChanges`, which carries every relation-data write a hook made. A unit's own databag and, when the unit is the leader, the application databag. |
| `config` | An application | The response to `Uniter.ConfigSettings`. |
| `leadership` | An application | A `leader-elected` hook starting, or a unit writing the application databag. |

Each snapshot also records the span that produced it, and through that span, the hook it came from. A snapshot whose body is identical to the previous one for the same scope is marked as a repeat, so a charm that sets `active` on every `update-status` doesn't fill the timeline with changes that changed nothing.

The `juju status` taken at the start of the recording provides a first snapshot for each application and unit. Without it, a unit that stays `active` for the whole recording would have no status at all.

To show the state at a given moment, the viewer takes the latest snapshot for each scope with a timestamp at or before that moment. Moving the cursor in the Events pane updates the Status pane this way.

## What the viewer shows

The Events pane lists, by default:

- **Hook runs**, one row each, at the time the hook started. The row shows the hook name, its duration, and whether it wrote relation data. With unit capture, a hook the tap recorded nothing of is marked `not instrumented`, and one whose records stop early is marked `truncated`; see [hooks the tap missed](#hooks-the-tap-missed).
- **Status changes**, one row each, at the time of the RPC that made the change, with the hook that caused it.
- **Settled units**, a row each time a unit's agent goes `idle` and stays there for at least three seconds. This is when a unit's status, such as `active / idle`, can be read as the result of the hooks before it.

Verbose mode (`.`) also shows the individual RPCs that the default view folds into hook rows: each status RPC, relation scope changes (`EnterScope`, `LeaveScope`), leadership claims, secret operations, and actions, plus the repeated status changes described above.

For a hook recorded with `--attach unit`, the inspector opens on a trace of what the charm did, one for each process that ran the hook, so a hook that Juju retried shows each attempt. A recording with only `--attach unit` has no `run-hook` markers: each hook process is a hook run of its own, and the processes of a hook that failed and ran again fold into one run. Each trace shows:

- **The charm's `__init__`**, from `ops.main` to the first event: ops builds the charm. In the recording this is a *charm init* span, which the viewer shows as the charm's `__init__` function when the hook was profiled, because nearly all of it is that function, and as `charm __init__` otherwise. A charm that reconciles in `__init__` spends most of the hook here, so under it the trace shows the functions of the charm's own code that ran and how long each took, from the stack samples. Observers that ran for a while show theirs the same way. A function marked `mostly waiting` used little CPU for the time it took, and the mark says what it was waiting for: a Pebble call, a hook tool, a subprocess, the network, or the function it was blocked in.
- **Deferred events** that ops re-emitted before the hook's own event, marked `re-emitted`.
- **Each event**, in the order ops emitted it, with the observers it reached. An event an observer deferred is marked `deferred`, on the event and on that observer. Events that a handler emits, such as a library's custom events, sit under that handler.
- **Hook tool and Pebble calls**, under the span that made them. The trace lists the calls that took long, as rows under the charm function that made them; `.` shows every call, and `export` writes every span in JSON. The Markdown export folds the calls into one line per span: a hook tool call keeps its own line when it set a status, took 50 ms or more, or failed, the rest of the hook tool calls fold into one line, and all the Pebble calls fold into another, each with their count, their total time and the most frequent names. Pebble reads that fail are folded too, because charms often check for files that don't exist yet. Events that no observer handled fold into one line.
- **Python subprocesses** the charm started, under the observer that was running when they started.

An exception shows on the innermost span it was raised in, usually an observer or charm init; the spans it passed through are marked `✗`. "of it Python code, not calls" is the time a span spent running Python between the hook tool and Pebble calls it made.

Log lines aren't tied to RPCs when they're recorded, so the Logs pane doesn't try to attribute them. It merges events and log lines from every source into one stream ordered by time and scrolls to the selected event, so the lines around it are the ones written while it happened. The exception is a log line that carries the event's span ID, which the pane marks with `»`. This only happens when tracing is enabled in Juju and the log source writes span IDs.

## How a hook run ended

Whether a hook run closed tells you very little on its own. A hook that the agent retried, a hook that the unit abandoned when it was removed, and a hook whose closing marker the probe dropped can all look the same. `juju-lens` uses the agent's own markers to tell them apart, checking each rule in turn:

```mermaid
flowchart TD
    A["hook run"] --> B{"opstep pending\nseen twice?"}
    B -->|yes| C{"still open at the\nend of the recording?"}
    C -->|yes| D["errored"]
    C -->|no| E["retried"]
    B -->|no| F{"another hook started on\nthis unit before continue?"}
    F -->|yes| G{"opstep done\nseen?"}
    G -->|yes| H["capture gap"]
    G -->|no| I["interrupted"]
    F -->|no| J{"an RPC in the hook\nreturned an error?"}
    J -->|yes| K["RPC warning"]
    J -->|no| L["succeeded"]
```

**Errored**
: The agent retried the hook and it hasn't succeeded yet. The unit is in an error state at the end of the recording.

**Retried**
: The hook failed, the agent retried it, and a later attempt succeeded. A retried hook followed by an `active` status is normal.

**Capture gap**
: Another hook started before this one's `continue`, but this one had reached `opstep: done`. The hook finished; the probe lost its closing marker. See [limitations](from-wire-to-viewer.md#limitations).

**Interrupted**
: Another hook started before this one reached `done`. The recording was cut short, the agent restarted, or the unit was removed while the hook ran.

**RPC warning**
: The hook closed normally, but an RPC in it returned an error. Charms often handle these errors themselves, so this doesn't mean the hook failed.

**Succeeded**
: None of the above.

A hook that hasn't reached `continue` yet and matches none of the rules is still running. This only happens at the end of a live recording.

The default view marks errored, retried, and RPC warning hooks. Capture gaps and interrupted hooks describe the recording, not the charm, so they're only marked in verbose mode.

The same classification appears in `juju-lens export`. See [how to export a recording for an agent](../how-to/export-for-an-agent.md).

## Hooks the tap missed

With `record --attach unit`, a module in the charm's venv records each hook from inside the charm process: its events, observers and calls. A hook it records nothing of would simply be missing from the timeline, and a missing hook reads the same as one that never ran. So `juju-lens` looks for hooks in three other places, and marks the ones the tap didn't see as `not instrumented`:

- **The unit probe's view of the processes.** The probe in each unit polls `/proc` for children of the unit agent, ten times a second while nothing runs. A hook or action it finds with no tap records becomes a hook run with its exact start, its end to within the poll, and what it cost, as soon as it ends.
- **The unit agent's status log.** The agent sets its status to `executing` with a message such as `running config-changed hook` or `running action reconcile` each time it runs a hook or an action. When the recording stops, the recorder fetches each covered unit's log with `juju show-status-log --type juju-unit`.
- **The `run-hook` markers**, when the recording also captured the agents' RPCs (`--attach host,unit`).

A hook from any of these with no tapped process of the same unit and hook at the same time becomes a hook run of its own, with the reason when the probe's records give one. The reasons come from what the probe saw: a charm with no venv isn't built on ops, so the tap can't load into it; a hook that ran after a refresh recreated the pod, but before the probe installed the tap in the new venv, was missed for that reason; and `juju exec` runs as the action `juju-exec`, which runs commands, not the charm.

Not every hook can be cross-checked this way:

- **A short hook can slip between two polls.** A process that starts and ends within one poll interval, about 100 milliseconds, isn't seen by the probe.
- **`update-status` isn't in the status log.** Juju doesn't log it, so in a recording with only `--attach unit`, an `update-status` the tap missed appears only if the probe's poll caught it. With `--attach host,unit`, its `run-hook` marker still shows it.
- **The status log is fetched when the recording stops.** A live view shows the hooks the probe found as they end, but the reasons, and the hooks only the status log saw, once the recording has stopped and been indexed. A recorder that was killed before it could stop has no status log.
- **Only units a unit probe covered are checked.** A hook on a unit that never had a probe isn't marked.

A hook the tap did record can still be incomplete. A process that the out-of-memory killer or `SIGKILL` stopped never writes its last record, and on Kubernetes a hook still running when its pod is deleted loses its probe session with it. Such hooks are marked `truncated`: the events and observers shown are the ones that ran before the records stop.
