# How capture works

`juju-lens` records Juju API traffic by reading it out of the memory of the Juju agents themselves, at the moment it passes through Go's TLS library. This page describes that mechanism. It applies to Juju 3.6 and 4.x, and to machine and Kubernetes controllers alike.

Every Juju agent (`jujud` on machines and controllers, `containeragent` in Kubernetes units) talks to the controller over a TLS-encrypted websocket. All the activity `juju-lens` cares about travels over that connection: a unit setting its status, a hook committing relation data, a leadership claim, a secret lookup. On the network this traffic is ciphertext. Inside the agent process it exists as plaintext for a short time: just before Go's `crypto/tls` package encrypts an outgoing write, and just after it decrypts an incoming read.

`juju-lens` reads the plaintext at those two points. It doesn't use the Juju API to watch the model, and it doesn't touch the charm processes. [Why the wire](why-the-wire.md) compares this with the alternatives.

## Attaching to the agent

eBPF is a Linux kernel feature for running small, verified programs inside the kernel in response to events. The kernel checks each program before loading it, so a program can't crash the kernel or loop forever, and it can't write to the memory of the process it observes.

A *uprobe* is one kind of event an eBPF program can attach to: "this process is about to execute the instruction at this address". `juju-lens-probe` places uprobes on two functions inside the agent binary:

- `crypto/tls.(*Conn).Write`, on entry. The buffer argument holds the plaintext about to be encrypted.
- `crypto/tls.(*Conn).Read`, on entry and on return. The entry probe remembers where the buffer is, and the return probe copies the bytes that were decrypted into it.

The usual way to hook a function's return is a *uretprobe*, which works by rewriting the return address on the stack. That breaks the Go runtime, which moves goroutine stacks around. Instead, `juju-lens-probe` finds every `RET` instruction in `Read` and attaches an ordinary uprobe to each one.

To place a probe, `juju-lens-probe` needs the address of each function. Production `jujud` binaries are stripped of debug symbols, so the usual symbol table doesn't list them. Every Go binary also carries `.gopclntab`, a table the Go runtime needs for stack traces and garbage collection, which stripping leaves in place, and `juju-lens-probe` reads the addresses from there. [`gojue/ecapture`](https://github.com/gojue/ecapture) uses the same technique.

Each time a probe fires, its eBPF program copies the buffer into a ring buffer shared with userspace. The agent isn't paused or reconfigured, and nothing in it changes. Probes need Linux 5.8 or newer, and `CAP_BPF` or root on the host where the agent runs.

Probes exist only while a `juju-lens record` or `juju-lens watch` command runs. They attach when the recorder starts and detach when it stops, whether by `Ctrl-C`, `juju-lens stop`, or a `--max-duration` or `--max-size` limit. There is no background daemon.

## What the plaintext contains

Once the websocket framing is removed, each message is a JSON object in the format of Juju's `rpc/jsoncodec` package. A request and its response share a `request-id`:

```json
{
  "request-id": 42,
  "type": "Uniter",
  "version": 19,
  "request": "SetUnitStatus",
  "params": {"entities": [{"tag": "unit-grafana-0", "status": "active", "info": ""}]}
}
```

```json
{
  "request-id": 42,
  "response": {"results": [{}]}
}
```

| Field | Present in | Meaning |
|---|---|---|
| `request-id` | both | Pairs a response with its request on the same connection. |
| `type` | request | The API facade, such as `Uniter` or `SecretsManager`. |
| `version` | request | The facade version. |
| `request` | request | The method, such as `CommitHookChanges`. |
| `params` | request | The method arguments. |
| `response` | response | The result, when the call succeeded. |
| `error`, `error-code` | response | The failure, when it didn't. |
| `trace-id`, `span-id` | either | Only present when tracing is enabled in Juju. |

Every request names its facade and method, so `juju-lens` can tell a status change from a relation write without knowing anything about the charm.

This is also why capture works the same on every supported Juju version. It depends on two things: the `crypto/tls` function names, which belong to the Go standard library, and the `rpc/jsoncodec` message format. Neither differs between Juju 3.6 and 4.x. Differences between versions show up in *which* facades and methods appear, and that's handled later, when RPCs are turned into hooks.

[From wire to viewer](from-wire-to-viewer.md) follows the captured bytes from the ring buffer to the recording on disk.
