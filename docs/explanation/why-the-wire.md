# Why the wire

`juju-lens` reads Juju API traffic inside the agent processes, as described in [how capture works](how-capture-works.md). Two other sources look simpler: the charm's own hook processes, and the Juju API used as a client. This page explains why neither works for `juju-lens` on Juju 3.6 or 4.x.

A third option, capturing packets on the network, doesn't work at all: agent traffic is TLS-encrypted, so a packet capture sees only ciphertext. Decrypting it would mean either reading keys out of the agent process, which is a harder version of what `juju-lens` already does, or putting a TLS proxy between the agent and the controller, which means installing certificates on the controller. `juju-lens` doesn't change anything on the machines it records.

## The charm's hook process

A hook process doesn't hold the state `juju-lens` records. It asks the unit agent for the relation ID, the remote unit, the databag contents, and the status through hook tools like `relation-get` and `status-set`, and the agent makes the actual API calls. Watching the hook process would show that it ran `relation-get`, but the answer would still have to be read from the agent's traffic.

The hook process also has two practical problems:

- It can be written in anything: Python with `ops`, a shell script, a future framework. Instrumenting it would mean supporting each language separately. The agent is the same Go binary for every charm.
- It lives for one hook, often a few hundred milliseconds. Attaching to each one as it starts is a race. The agent runs for the life of the unit, so attaching once when the recording starts is enough.

`juju-lens` isn't a charm profiler, and doesn't instrument charm code.

## The Juju API as a client

Juju has a stream of model changes for clients, the `AllWatcher`. It isn't usable for this:

- Juju 4.x removed it. Its facades return "not implemented", and the planned replacement doesn't exist yet.
- On Juju 3.6 it doesn't include relation databag contents. Databags are only available through the `Uniter` facade, which authenticates unit agents, not clients. `juju show-unit` can read a databag once, but there's no stream of changes.

`juju-lens` does use the Juju API for two things that work on both versions: `juju status` at the start of a recording, to know the initial state of the model, and `juju debug-log`, for logs.
