# Why the wire

With `--attach host`, `juju-lens` reads Juju API traffic inside the agent processes, as described in [how capture works](how-capture-works.md). Two other sources look simpler for what that records, the state of the model: the charm's own hook processes, and the Juju API used as a client. This page explains why neither replaces the wire on Juju 3.6 or 4.x, and what the charm side is used for instead.

A third option, capturing packets on the network, doesn't work at all: agent traffic is TLS-encrypted, so a packet capture sees only ciphertext. Decrypting it would mean either reading keys out of the agent process, which is a harder version of what `juju-lens` already does, or putting a TLS proxy between the agent and the controller, which means installing certificates on the controller. Host capture doesn't change anything on the machines it records.

## The charm's hook process

A hook process doesn't hold the state the wire carries. It asks the unit agent for the relation ID, the remote unit, the databag contents, and the status through hook tools like `relation-get` and `status-set`, and the agent makes the actual API calls. Watching the hook process shows that it ran `relation-get`, but not the answer, and not what the agent did around the hook: the `run-hook` markers it saved before and after, what it committed, how its status changed.

The hook process also has two practical problems:

- It can be written in anything: Python with `ops`, a shell script, a future framework. Instrumenting it means supporting each language separately. The agent is the same Go binary for every charm.
- It lives for one hook, often a few hundred milliseconds. Attaching to each one from outside as it starts is a race. The agent runs for the life of the unit, so attaching once when the recording starts is enough.

What the hook process does have is what the wire can't show: which events ops emitted inside one hook, which observers ran and how long each took, what the hook cost in CPU and memory, and where its Python spent that time. `--attach unit` records that side. It avoids both problems by narrowing the scope rather than solving them: it records events and stacks only for charms built on ops, and instead of attaching to each process it installs a module that the charm's Python loads as it starts. That means writing into the charm's venv for the duration of the recording; [how unit capture works](how-unit-capture-works.md) describes what it writes and how it's removed.

So each mode sees one side of a hook:

| Mode | Sees | Doesn't see |
|---|---|---|
| `host` | Juju's side: the agents' Juju API calls, and from them the `run-hook` markers, agent and workload status, relation data both ways, leadership and secrets, for any charm. | Anything inside the hook process. |
| `unit` | The charm's side: events, observers, deferrals, hook tool and Pebble calls, exit code and exception, CPU and memory per hook and per observer, Python stacks. | Juju's side: the `run-hook` markers, agent status, relation data changes the charm never reads. |
| `host,unit` | Both, with each hook's charm-side tree joined to its RPCs. | Events, observers and stacks of charms not built on ops. |

## The Juju API as a client

Juju has a stream of model changes for clients, the `AllWatcher`. It isn't usable for this:

- Juju 4.x removed it. Its facades return "not implemented", and the planned replacement doesn't exist yet.
- On Juju 3.6 it doesn't include relation databag contents. Databags are only available through the `Uniter` facade, which authenticates unit agents, not clients. `juju show-unit` can read a databag once, but there's no stream of changes.

`juju-lens` does use the Juju API, through the `juju` client, for things that work on both versions: `juju status` at the start of a recording, to know the initial state of the model, and `juju debug-log`, for logs. Unit capture also uses `juju ssh` to reach the units, and `juju show-status-log` when the recording stops, to find hooks its module missed.
