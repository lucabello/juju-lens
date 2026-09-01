# How capture works

This page explains where `juju-lens` gets its data from: what it reads,
where it reads it from inside a running process, and why that's enough to
see every Juju API call without talking to the Juju API for it. It applies
to both Juju 3.6 and 4.x, and to machine and Kubernetes controllers alike.

## The short version

Every `jujud` process — controller or unit agent, machine or Kubernetes —
talks to its peers over TLS, using Go's `crypto/tls` package. `juju-lens`
attaches an eBPF probe to that process and reads the plaintext at the two
points where it briefly exists unencrypted inside the process: right before
`crypto/tls` encrypts an outgoing write, and right after it decrypts an
incoming read. The process itself is never paused, modified, or
reconfigured to make this possible — the probe only reads memory the
process already produces as part of doing its normal work.

## What the wire carries

Every Juju API call — `Uniter.CommitHookChanges`, `SecretsManager.*`,
status setters, relation scope changes — goes through this same connection
as a JSON-over-websocket envelope, self-describing with a request id, a
facade name, a method name, and (when tracing is configured upstream) a
trace id. Reading it means seeing every RPC an agent makes or serves,
verbatim, in order — there's no sampling and nothing needs to be inferred
from a side effect.

This is also the reason `juju-lens` doesn't use the Juju API itself as its
main source: the client-facing model-events watcher it would need is
removed in Juju 4.x, and even on 3.6 it doesn't carry relation databag
contents. See [why the wire, not the charm or the API](why-the-wire.md) for
the comparison in full.

## Why the technique survives version upgrades

`juju-lens` depends on two things: Go's `crypto/tls` (standard library) and
the JSON wire format Juju's own `rpc/jsoncodec` uses. Neither changes across
Juju minor versions, so the same uprobe addresses and the same envelope
decoder work on 3.6 and 4.x without version-specific logic in the capture
path itself.

## How the probe finds its target on a stripped binary

`jujud` binaries ship stripped in production — no debug symbols. The probe
still finds `crypto/tls.(*Conn).Write`/`.Read` by reading `.gopclntab`, a
symbol table every Go binary carries regardless of stripping, and resolving
the addresses to attach to. This is the same technique used by
[`gojue/ecapture`](https://github.com/gojue/ecapture), the reference
implementation for uprobing Go TLS on stripped binaries.

## When capture runs

Only for the lifetime of one `juju-lens record` (or `watch`) invocation.
Probes attach when the recorder starts and detach cleanly on `Ctrl-C`,
`juju-lens stop`, or a configured `--max-duration`/`--max-size` limit —
there's no always-on daemon, and nothing is captured outside that window.

## What's next

Once captured, plaintext bytes still need to become the rows the viewer
shows. See [from wire to viewer](from-wire-to-viewer.md) for that path.
