# Why the wire, not the charm or the API

`juju-lens` reconstructs "what happened" by reading Juju API traffic at the
TLS boundary inside `jujud`/`containeragent` (see [how capture
works](how-capture-works.md)). This page explains why that boundary was
chosen over two plausible alternatives: attaching to the charm's own hook
process, or watching the Juju API as a client.

## The three candidate vantage points

| Vantage point | Verdict |
|---|---|
| **The Juju API wire** (what `juju-lens` uses) | The producer of all the state that matters, seen at the moment it becomes real. |
| The charm's own hook process | A *consumer* of state, not its producer — see below. |
| The Juju API, as a client (e.g. the `AllWatcher` model-events stream) | Broken on the versions this project targets — see below. |

## Why not the charm process?

A charm's hook process doesn't independently know the relation id, the
remote unit, the databag contents, or the model's status — it asks for all
of that through `jujuc` hook tools (`relation-get`, `status-set`, …), which
are themselves RPCs to the unit agent. Watching the hook process from
outside would show *that* it made those calls, not their structured
content — decoding that content still means understanding the same
protocol, just from a worse vantage point, and for one that's also:

- **Not uniform.** `jujud`/`containeragent` is the same Go build regardless
  of the charm installed. The hook process can be anything — Python, a
  shell script, any future charming framework — so instrumenting it means
  re-solving the problem per charm language, indefinitely.
- **Short-lived.** A hook process lives for one hook, typically hundreds of
  milliseconds — attaching per-process is a race. `jujud`/`containeragent`
  lives for the unit's entire life, so attaching once at record start
  misses nothing.

This is intentional project scope, not an oversight: `juju-lens` is not a
charm profiler, and doesn't instrument charm code in any language.

## Why not the Juju API as a client?

The natural-sounding alternative — watch the Juju API's own model-events
stream (`AllWatcher`) as an admin client — was tried and rejected, because
it's broken on both versions this project targets:

- **Juju 4.x removed it.** The relevant facades return "not implemented,"
  and its documented replacement isn't built yet.
- **Even on 3.6, it doesn't carry relation databag contents.** Databags live
  behind the Uniter facade, which authenticates unit agents, not admin
  clients — a one-shot inspection (`juju show-unit`), not a stream.

`juju-lens` still uses the Juju API for what it does well and works on both
versions: `Status()` to bootstrap the inventory, and `WatchDebugLog` for the
log stream. It just isn't the primary source.

## The one thing worth naming: TLS means "from outside" isn't an option either

A true external capture — a network tap, a sidecar packet capture — sees
only ciphertext, since the traffic is TLS-encrypted end to end. Getting the
plaintext that way would mean either pulling it out of the process anyway
(the same uprobe idea, described differently) or terminating TLS in the
middle, which requires installing trust material and would violate the
project's core promise: **`juju-lens` never mutates the target.**

## See also

[How capture works](how-capture-works.md) for the mechanics, and [the
correlation model](correlation-model.md) for how the captured data is
stitched back together.
