# Your first recording

This tutorial walks you through generating a synthetic `juju-lens` recording
and exploring it in the viewer. You'll end up with a working recording on
disk and a feel for the three things the viewer shows: events, status, and
logs. No Juju controller, root access, or Linux-specific tooling is needed —
synthetic recordings are plain data, so this works on any platform with Go.

## Prerequisites

- [Go](https://go.dev) ≥ 1.25.
- [`just`](https://github.com/casey/just), the recipe runner this project uses for common tasks.
- A clone of the `juju-lens` repository.

## Step 1: build the binaries

From the repository root:

```bash
just build
```

This produces `./bin/juju-lens` and `./bin/juju-lens-probe`. You won't need
the probe binary for this tutorial — it's only used for capturing from a real
controller (see [how to record a live controller](../how-to/record-a-controller.md)).

## Step 2: generate a synthetic recording

```bash
./bin/juju-lens synth trivial --output /tmp/rec-trivial
```

`synth` writes a recording directory whose contents mimic what a real Juju
controller would emit, without needing one. The `trivial` scenario simulates
two applications, `grafana` and `prometheus`, forming one relation: it
deploys both, has them join the relation, sets a workload status on each
unit, and sets the leader's application status. The recording is
byte-for-byte reproducible, so you'll see identical output every time you
run this command.

Inspect what got written:

```bash
ls /tmp/rec-trivial
```

You'll see `manifest.json` (what this recording is, when it ran),
`index.db` (a SQLite index built from the captured data), and a `raw/`
directory holding the underlying JSONL. The [recording layout
reference](../reference/recording-layout.md) explains each piece.

## Step 3: open it in the viewer

```bash
./bin/juju-lens view /tmp/rec-trivial
```

You should see a terminal UI with two rows: **Events** and **Status** side
by side on top, and **Logs** spanning the width beneath them. Everything is
synchronised to whichever event is currently selected.

Try this:

1. Press `↓`/`j` a few times to move through the events. Each is a hook
   (`install`, `config-changed`, `grafana-source-relation-joined`, …) fired
   by one of the two units.
2. Press `Enter` on a hook to open its inspector: what status it set, what
   databag it changed, and the raw RPCs behind it.
3. Press `.` to toggle verbose mode — this reveals the raw status changes
   and relation-scope events that are folded into hooks by default.
4. Press `Tab` to move focus to the Status pane, and watch it show each
   application's and unit's status as of the currently selected event.
5. Press `q` to quit.

For the full keybinding list, see the [viewer keybindings
reference](../reference/viewer-keybindings.md).

## What you just did

Everything you saw came from one on-disk recording — no live process, no
network. That's the whole point: `record` (against a real controller) and
`synth` (for a fabricated one) both produce the same kind of recording
directory, and `view` only ever reads it back. To understand how a real
recording captures this data from a running controller, see [how capture
works](../explanation/how-capture-works.md); to actually record one, see
[how to record a live controller](../how-to/record-a-controller.md).
