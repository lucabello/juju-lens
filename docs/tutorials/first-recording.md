# Your first recording

This tutorial captures a real Juju deployment: you'll record a Kubernetes
controller while a charm's own integration test suite deploys and relates
two real charms against it, then explore that recording in the viewer. By
the end you'll have watched `juju-lens` turn real hook activity — the kind
that runs in charm CI on every PR — into a navigable timeline.

This is more setup than a synthetic recording, but it's the real thing:
every event you'll see was actually fired by a real `jujud`, not fabricated
for the tutorial. If you'd rather skip the infrastructure and just see the
viewer, see [how to generate a synthetic recording](../how-to/generate-a-synthetic-recording.md)
instead — you can come back to this once you have a charm dev environment
around anyway.

## Prerequisites

- `juju-lens` built (`just build`, from a clone of this repository).
- Linux ≥ 5.8 and root (or `CAP_BPF`) on this machine — recording needs to
  attach eBPF uprobes here.
- A working charm development environment: [MicroK8s](https://microk8s.io/)
  with a Juju controller bootstrapped onto it, plus `charmcraft` and `tox`
  (or `uv`) installed. If you don't have this yet, follow the [Juju SDK's
  charm development setup](https://documentation.ubuntu.com/juju/3.6/howto/manage-charms/) —
  it's the same environment charm authors use day to day, not anything
  `juju-lens`-specific.
- `git`, to clone a charm repository.

This works specifically because MicroK8s runs its pods as ordinary Linux
processes on this same host, so root here can see the controller's `jujud`
process directly. `juju-lens` doesn't yet support attaching to a controller
running somewhere else in a Kubernetes cluster (see the [project
status](../../README.md#status)) — recording a remote k8s controller isn't
possible today. Recording a *machine* controller from wherever you are
works over SSH instead; see [how to record a controller](../how-to/record-a-controller.md).

## Step 1: get a charm with integration tests

We'll use [`prometheus-k8s-operator`](https://github.com/canonical/prometheus-k8s-operator),
whose `metrics-endpoint` relation exercises exactly the kind of hook
sequence `juju-lens` is built to untangle: a deploy, a relation join, a
config change, and a charm refresh.

```bash
git clone https://github.com/canonical/prometheus-k8s-operator
cd prometheus-k8s-operator
```

Any charm with a `tox -e integration` (or equivalent) target works for this
tutorial — pick one you maintain yourself if you'd rather record something
you already know the internals of.

## Step 2: start recording

In this terminal, confirm your Juju client points at the controller
bootstrapped onto MicroK8s (`juju controllers` should show it as current).
Then, from wherever you built `juju-lens`:

```bash
sudo ./bin/juju-lens record int-test --attach local
```

Leave this running. With no `--controller`/`--model` given, it scopes to
your current controller and *every* model on it — including the throwaway
model the test suite is about to create, which `record` picks up
automatically as soon as it appears (see [how capture
works](../explanation/how-capture-works.md)).

## Step 3: run the integration tests

In a second terminal, from the `prometheus-k8s-operator` checkout:

```bash
tox -e integration -- -k test_charm
```

This builds and deploys `prometheus-k8s` alongside a small tester charm,
relates them, changes the tester's configuration, and refreshes it — all
against the model `record` is now watching. Kubernetes charm deployments
aren't instant; expect this to take several minutes. If the test file name
has moved, look under `tests/integration/` in the checkout for a
comparably small one.

## Step 4: stop the recording

Once the test run finishes (pass or fail — either way, real hooks fired),
go back to the first terminal and press `Ctrl-C`. `record` detaches its
probes, finalises `manifest.json`, and builds `index.db` before exiting.

## Step 5: open it in the viewer

```bash
./bin/juju-lens view ./recordings/<timestamp>--int-test
```

You should see two rows: **Events** and **Status** side by side on top,
**Logs** spanning the width beneath. Unlike a synthetic recording, exactly
what's here depends on what the test suite did on your run — but you should
be able to find, among the events:

1. The install/config-changed/start sequence for at least one unit, from
   the initial deploy.
2. A `metrics-endpoint-relation-joined` (or similarly named) event once the
   tester charm relates to `prometheus-k8s`.
3. A `config-changed` from the tester's alert-rules config change.
4. An `upgrade-charm` from the refresh.

Move through them with `↓`/`j`, press `Enter` on one to open its inspector,
and press `.` to toggle verbose mode and see the raw status/relation
traffic folded into each hook by default. For the full keybinding list, see
the [viewer keybindings reference](../reference/viewer-keybindings.md).

## What you just did

You recorded a real Juju controller for the duration of a real test run,
using nothing but read-only eBPF uprobes — `juju-lens` never touched the
controller's configuration or the charms under test. To do the same
against your own deployments, see [how to record a
controller](../how-to/record-a-controller.md) or [how to watch a live
model](../how-to/watch-a-live-model.md) for a version that opens the viewer
automatically. To understand what you're looking at in more depth, see
[the hook timeline](../explanation/hook-timeline.md) and [the correlation
model](../explanation/correlation-model.md).
