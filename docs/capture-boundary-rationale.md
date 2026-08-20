# Why the wire, not the charm process: capture-boundary rationale

This document records the reasoning behind juju-lens's core capture choice —
eBPF uprobes on the Juju API TLS boundary inside `jujud`/`containeragent` —
against the alternative of attaching to the charm's own process or container
from outside. It also records a proof-of-concept that explored a second,
complementary capture boundary (Pebble/workload traffic) and why it was
removed. Nothing here changes behavior; it's the "why" that isn't otherwise
written down next to the AllWatcher-API rejection in
[VISION.md](../VISION.md) §2.2.

## 1. The question

Juju's runtime has (at least) three places you could plausibly attach an
observer to reconstruct "what happened":

1. **The Juju API wire** — the TLS connection between an agent (`jujud`
   unit/machine agent, or a k8s unit sidecar's `containeragent`) and the
   controller's apiserver. This is what juju-lens captures today
   (VISION.md §2.1, §5.1).
2. **The charm's own hook process** — the short-lived process Juju execs to
   run `install`, `config-changed`, `<relation>-relation-changed`, etc.
   (typically Python via `ops`, but Juju doesn't constrain the language).
3. **The workload container**, from outside — e.g. watching a sidecar
   charm's Pebble daemon traffic, or generic syscall/network activity on the
   container, without touching the charm's language or runtime at all.

The question this document answers: why (1), and not (2) or (3)?

## 2. Why not the charm process (2)?

The charm's hook process is a *consumer* of state, not the producer of it.
It doesn't independently know the relation ID, the remote unit, the
databag contents, or the model's status — it asks for all of that through
`jujuc` hook tools (`relation-get`, `status-set`, …), which are themselves
RPCs to the unit agent. Watching the hook process from outside (syscalls,
exec events, env vars — see §5) would show *that* it made those calls, not
their structured content; decoding that content still means understanding
the same protocol, just from a worse vantage point:

- **Non-uniformity.** `jujud`/`containeragent` is the same Go build
  regardless of what charm is installed. The hook process can be anything —
  Python, a shell script, any future charming framework. One uprobe
  technique on one binary covers an entire model; instrumenting hook
  processes means re-solving symbol/protocol resolution per charm
  language/runtime, indefinitely.
- **Lifetime.** A hook process lives for one hook — hundreds of
  milliseconds, typically. Attaching per-process is a scan/attach race
  (see §4's documented race window). `jujud`/`containeragent` lives for the
  unit's entire life; attaching once at record start misses nothing.
- **Authority.** The state that matters (hook kind, relation membership,
  databag diffs, status) is committed through the agent to the controller.
  The wire capture sees it at the point it becomes real and causally
  ordered (`request-id`/`trace-id`), not inferred from a side effect.

This is also **already the project's stated scope**: VISION.md §11 is
explicit — "**Not a charm profiler.** We don't instrument charm Python
code. Charm-side visibility comes from captured jujuc RPCs and from logs."
The `jujuc.(*Jujuc).Main` uprobe (§4 below) is how that visibility is
obtained without ever touching the charm process itself.

## 3. Why not the container/workload, from outside (3)?

This is possible in principle — and was prototyped (§6) — but for the
*primary* signal (hooks, relations, status, databags) it doesn't apply,
because that traffic is the Juju API traffic described in (1), and it's
TLS-encrypted. A true external capture (a host network tap, a sidecar
packet capture) sees only ciphertext unless it also has key material, which
means either:

- pulling the plaintext out of the process anyway — i.e. the same uprobe
  idea, just described differently, or
- MITM'ing the connection, which requires installing trust material and
  touches Juju's TLS configuration — a hard violation of the project's
  "never mutate the target" principle (VISION.md §8.8, README "Status").

So for Juju API traffic specifically, "attach from outside the container"
collapses back into "read the plaintext inside the process that has it,"
which is exactly the TLS-boundary uprobe technique already in place.

## 4. What "the wire" actually is — two boundaries, not one

juju-lens captures at two uprobe points, and **both live inside the same
long-lived agent binary** (`jujud` or `containeragent`) — never inside the
charm's hook process or the small hook-tool client binaries
(`relation-get`, `status-set`, …) it execs:

1. **`crypto/tls.(*Conn).Write` / `.Read`** — the Juju API connection. One
   end is an **agent process** (`jujud` as unit/machine agent, or
   `containeragent`) acting as an API client; the other end is the
   **controller's apiserver**, itself a `jujud` process, in its
   API-serving role. This carries the JSON-over-websocket RPC envelope
   (`rpc/jsoncodec`) for facades like `Uniter.*`, `SecretsManager.*`,
   leadership, model config. Attaching this same uprobe on the controller's
   own `jujud` and on a unit's agent captures the identical logical
   exchange from both ends.
2. **`jujuc.(*Jujuc).Main`** — a *separate, local-only* channel. One end is
   a hook-tool client process (`relation-get`, `status-set`, …), invoked by
   the charm's hook process with the tool name as `argv[0]`; the other end
   is the `jujuc` RPC server **embedded in the same unit agent process**,
   reached over a Unix domain socket. It never reaches the controller
   directly. The symbol resolves to a method — `Jujuc.Main` — because the
   real Juju source (`internal/worker/uniter/runner/jujuc/server.go`)
   registers it as a Go `net/rpc` handler (`Type.Method` naming
   convention); the receiver runs inside the long-lived agent, which is
   exactly why uprobing it doesn't require touching the ephemeral charm
   process at all.

## 5. Env vars: what they'd add, and what was verified

Juju sets a number of `JUJU_*` environment variables on the hook process
(`JUJU_RELATION`, `JUJU_RELATION_ID`, `JUJU_REMOTE_UNIT`,
`JUJU_REMOTE_APPLICATION`, `JUJU_DISPATCH_PATH`, `JUJU_CONTEXT_ID`,
`JUJU_AGENT_SOCKET_ADDRESS`, …). The natural question: doesn't that carry
hook metadata we could just read, instead of deducing it from the wire?

Two things were checked, not assumed:

- **juju-lens doesn't read them today.** No `/proc/<pid>/environ` reads, no
  exec tracing anywhere in the codebase. Getting them would need a *third*
  capture technique (an `execve`/`sched_process_exec` tracepoint aimed at
  the charm's ephemeral hook process) — reintroducing exactly the
  per-language, per-process, race-at-start problems from §2.
- **The RPC that would be the natural place for them doesn't carry them
  either.** Checked directly against the upstream `juju/juju` source
  (`internal/worker/uniter/runner/jujuc/server.go`, `Request` struct): the
  hook-tool-client → agent RPC carries `{ContextId, Dir, CommandName,
  Args, StdinSet, Stdin}` — **no environment**. The client only reads
  `JUJU_CONTEXT_ID` (which hook context this call belongs to) and
  `JUJU_AGENT_SOCKET_ADDRESS` (where to dial); the actual semantic content
  — relation ID, remote unit, remote app, storage ID, secret URI — already
  lives server-side, in a `HookContext` the agent built *before*
  dispatching the hook at all.

That `HookContext` construction is itself something juju-lens already
captures: `docs/event-timeline.md` documents that the `Uniter.SetState`
"run-hook" marker written to the controller carries the full hook identity
and arguments (`relation-id`, `remote-unit`, `remote-application`,
`change-version`, `storage-id`, `secret-uri`, `workload-name`, …) as
structured JSON — arriving *before* the charm process is even exec'd.
So nothing is missing by skipping the charm process's environment; the
same content arrives earlier and in a more reliable form.

## 6. What the wire gives that env vars never could

Even a hypothetical env-var capture would only ever carry *identifiers* —
which relation, which remote unit, which hook kind. The wire gives content
and correlation on top of that:

- **Values, not just identifiers**: full databag contents, status
  messages, secret content, `relation-get` results, action
  params/results — none of this is ever in an env var.
- **Timing**: write/read timestamps per RPC, the basis for every
  synthesised span's start/end and duration. Env vars carry no timing
  signal.
- **The full ordered sequence** of every jujuc tool call within a hook,
  each with its actual arguments and result.
- **Both peers of the exchange**, and every unit's exchange, from one
  probe technique — because it attaches identically to every agent,
  controller included.

## 7. The Pebble proof of concept (removed)

A second boundary was prototyped in `examples/pebble-central-watcher/`:
capturing Pebble API traffic (charm code talking to a workload container's
`pebble` daemon over a Unix socket). This traffic never touches
`jujud`/`containeragent`, so it's outside what the TLS-boundary technique
can see by construction — not a gap in how that technique was applied, but
a genuinely different transport (plain HTTP over a Unix socket, no TLS, no
fixed process, not necessarily even a Go binary).

The prototype's answer was: yes, capturable, but only with a *different*
eBPF mechanism — kernel syscall tracepoints (`sys_enter_connect/write/read`,
`sys_exit_read`, `sys_enter_close`) attached once, system-wide, with
userspace deciding per-connection whether a socket path looked like a
Pebble socket. That's structurally distinct from the symbol-resolving
uprobe technique used everywhere else in this project, with its own
tradeoffs: a documented race window between `connect()` and userspace
deciding to watch a given fd (data written in that window is missed), the
need to attach broadly across an entire host/node rather than to one known
binary, and (per its own status note) it was never actually validated
against a live kernel.

**This has since been judged unfeasible and removed** (`examples/` deleted
from the tree). It was a proof of capture only — it never fed into the
narrative/index/viewer pipeline — so removing it has no effect on anything
implemented. If workload/Pebble-traffic visibility is revisited later, the
reasoning above (why it needs a second, different capture engine, and what
that engine's known limitations were) is worth reading before re-attempting
it, but it is **not** the plan going forward.

## 8. Summary

- **Primary capture stays exactly as designed**: TLS-boundary uprobes on
  `jujud`/`containeragent` for the Juju API, plus a `jujuc.(*Jujuc).Main`
  uprobe on the same process for hook-tool calls. Both endpoints of both
  boundaries live inside agent processes; the charm's own hook process and
  its hook-tool client binaries are never touched.
- **Charm/container-external attach was considered and is out of scope**,
  consistent with VISION.md §11's "not a charm profiler" — confirmed here
  with the concrete reasons (uniformity, process lifetime, TLS
  decryption, authority of the signal) rather than left as an assertion.
- **Environment variables add nothing juju-lens doesn't already have**,
  verified against the real `jujuc` RPC contract, not assumed.
- **The Pebble/workload capture prototype is removed** as unfeasible; it
  remains documented here as a decision record, not as a live design.
