# Event timeline — hooks, and how each maps to a capture signal

Status: **implemented** (M10 adds failure classification §9 and status
attribution §10). Grounds the rework of `internal/viewer/events.go`
(hook-run events + verbose filter), `internal/viewer/timeline.go` (two-line
rendering), and the `.` binding in `internal/viewer/tui.go`. The hook→signal
mapping (§4.1) and the workload/pebble and storage/secret parameter extraction
(§4, detail column) are the durable reference for extending
`index.HookOp`/`parseUniterState` as those hook families gain synth coverage.

## 1. The question this answers

The Events pane today derives one event per interesting *RPC* — a
`CommitHookChanges`, a `SetStatus`, an `EnterScope`, a `SecretsManager` call
(`internal/viewer/events.go:classify`). That is the wrong altitude. It shows
the *side effects* an RPC had, not the *hook* the charm was running, and it
mixes two questions the user asks at different times:

- **"What happened?"** — the sequence of Juju **hooks** (`install`,
  `config-changed`, `grafana-source-relation-joined`, …). This is the default
  reading of the model and should be all the timeline shows.
- **"How exactly did it happen?"** — the raw RPC/jujuc traffic behind a hook
  (status setters, scope changes, secret management, databag commits). This is
  a drill-down, not a navigator row.

This spec defines the **default** view as *exactly the hooks from the Juju hook
reference* (`https://canonical.com/juju/docs/juju-cli/3.6/reference/hook/`),
each shown as a **start** line and an **end** line, and a **verbose** view
(toggled with `.`) that additionally surfaces the raw transitions.

## 2. Source of truth: the profiling data is enough

**We do not need to cross-correlate with `juju debug-log` to build the hook
timeline.** Every hook is bracketed, on the wire we already capture, by two
`Uniter.SetState` RPCs the uniter writes around its `RunHook` operation:

```
Uniter.SetState   uniter-state:  op: run-hook   hook: {kind: config-changed, …}   ← HOOK START
   … the hook body's RPCs (CommitHookChanges, jujuc tools, status setters) …
Uniter.SetState   uniter-state:  op: continue                                     ← HOOK END
```

The indexer already parses these markers (`internal/index/hooks.go:parseUniterState`,
`Indexer.hookByUnit`) — but only to *label* the `CommitHookChanges` span with a
hook name. It never emits the markers themselves as events. The rework promotes
them to first-class events.

Why the `run-hook` marker is the richest possible source: the uniter-state blob
is not just `kind`. For relation/storage/secret hooks it carries the hook's full
parameters in the same payload:

| Hook family | Extra fields in the `run-hook` uniter-state `hook:` block |
|---|---|
| relation-* | `relation-id`, `remote-unit`, `remote-application`, `change-version` |
| storage-*  | `storage-id` |
| secret-*   | `secret-uri`, `secret-revision`, `secret-label` |
| workload   | `workload-name` (the pebble container) |

So the exact hook identity **and** its arguments are available from one captured
RPC, before the charm code even runs. `parseUniterState` currently extracts only
`op` and `kind`; the rework extends it to pull these fields (all optional).

**debug-log is corroboration and fallback, never the primary source.** Juju
emits `INFO juju.worker.uniter.operation ran "config-changed" hook` at each
hook's *end* (already present in our synth stream, `synth.go`). We use it for:

1. **Fallback identity** when the recorder attached mid-hook and never saw the
   `run-hook` marker: a `ran "X" hook` line lets us still place an end event.
2. **A confirmation glyph** — an event whose end we saw *both* on the wire and
   in the log is drawn solid; a wire-only end is fine but un-corroborated.

It is never *required*: a recording with `--sources rpc` only (no debug-log)
still produces a complete, correct hook timeline.

## 3. The hook-run model

A **hook run** is the unit of the default timeline. It is derived per unit by
replaying that unit's `SetState` operations in time order (exactly the state
machine `hooks.go:LabelHooks` already walks), producing:

```go
type hookRun struct {
    unit       string        // "grafana/0"
    kind       string        // canonical hook name, e.g. "config-changed"
    endpoint   string        // relation endpoint for relation-* (else "")
    remoteUnit string        // relation-*; from the run-hook payload
    storageID  string        // storage-*
    secretURI  string        // secret-*
    container  string        // workload/pebble-*
    startSpan  string        // span id of the run-hook SetState (start marker)
    endSpan    string        // span id of the continue SetState (end marker)
    startTs    time.Time
    endTs      time.Time     // zero if still running / never closed
    failed     bool          // any RPC in the bracket errored, or hook-error op seen
    subSpanIDs []string      // every span between the markers (the drill-down)
    hasDatabag bool          // a CommitHookChanges in the bracket changed a databag (M7)
}
```

Each `hookRun` emits **two** timeline events sharing one `hookRun`:

- a **start** event at `startTs`, and
- an **end** event at `endTs`.

Both carry the same `hookRun` pointer, so **pressing Enter on either opens the
identical inspector** (§6) — they are two views of one thing. The two lines are
what let the user see, at a glance, how long a hook took and whether anything
happened *inside* the window (e.g. a status flip appears between them in verbose
mode).

## 4. Default view — the canonical hook set

Default mode shows **only** rows whose `kind` is a Juju hook from the reference.
Everything else (bare `SetStatus`, `EnterScope`, `SecretsManager.*`, standalone
`CommitHookChanges` with no enclosing hook) is suppressed to verbose mode.

### 4.1 Hook → capture-signal map

Every hook is detected the **same way**: the `run-hook`/`continue` SetState
bracket. The "relation/storage/secret detail" column shows what extra we pull
from the `run-hook` payload to render the full name and inspector.

| Reference hook | `hook.kind` on the wire | Detail pulled from `run-hook` payload | Timeline label (default) |
|---|---|---|---|
| `install` | `install` | — | `install` |
| `config-changed` | `config-changed` | — | `config-changed` |
| `start` | `start` | — | `start` |
| `stop` | `stop` | — | `stop` |
| `remove` | `remove` | — | `remove` |
| `update-status` | `update-status` | — | `update-status` |
| `upgrade-charm` | `upgrade-charm` | — | `upgrade-charm` |
| `leader-elected` | `leader-elected` | — | `leader-elected` |
| `leader-settings-changed` | `leader-settings-changed` | — | `leader-settings-changed` |
| `pre-series-upgrade` | `pre-series-upgrade` | — | `pre-series-upgrade` |
| `post-series-upgrade` | `post-series-upgrade` | — | `post-series-upgrade` |
| `<endpoint>-relation-created` | `relation-created` | `endpoint`, `relation-id` | `<endpoint>-relation-created` |
| `<endpoint>-relation-joined` | `relation-joined` | `endpoint`, `remote-unit` | `<endpoint>-relation-joined` |
| `<endpoint>-relation-changed` | `relation-changed` | `endpoint`, `remote-unit` | `<endpoint>-relation-changed` |
| `<endpoint>-relation-departed` | `relation-departed` | `endpoint`, `remote-unit` | `<endpoint>-relation-departed` |
| `<endpoint>-relation-broken` | `relation-broken` | `endpoint` | `<endpoint>-relation-broken` |
| `<storage>-storage-attached` | `storage-attached` | `storage-id` | `<storage>-storage-attached` |
| `<storage>-storage-detaching` | `storage-detaching` | `storage-id` | `<storage>-storage-detaching` |
| `secret-changed` | `secret-changed` | `secret-uri`, `revision` | `secret-changed` |
| `secret-expired` | `secret-expired` | `secret-uri`, `revision` | `secret-expired` |
| `secret-rotate` | `secret-rotate` | `secret-uri` | `secret-rotate` |
| `secret-remove` | `secret-remove` | `secret-uri`, `revision` | `secret-remove` |
| `<container>-pebble-ready` | `pebble-ready` | `workload-name` | `<container>-pebble-ready` |
| `<container>-pebble-custom-notice` | `pebble-custom-notice` | `workload-name`, `notice-*` | `<container>-pebble-custom-notice` |
| `<container>-pebble-check-failed` | `pebble-check-failed` | `workload-name`, `check-name` | `<container>-pebble-check-failed` |
| `<container>-pebble-check-recovered` | `pebble-check-recovered` | `workload-name`, `check-name` | `<container>-pebble-check-recovered` |

Notes:

- On the wire, Juju sends the **bare** hook kind (`relation-joined`), not the
  endpoint-prefixed form; the uniter reconstructs the charm-visible name
  (`grafana-source-relation-joined`) from `kind` + `endpoint`. We do the same:
  the label is `endpoint + "-" + kind` for relation hooks, `storage + "-" + kind`
  for storage hooks, `container + "-" + kind` for workload hooks. This replaces
  the string-munging in `events.go:shortHook`, which currently goes the other
  way (trimming the prefix off an already-joined name).
- **Deprecated hooks** (`leader-settings-changed`, and the long-gone
  `collect-metrics`) are still shown when they occur — the tool records history
  faithfully — but tagged `deprecated` in the inspector.

### 4.2 What leaves the default timeline

These are RPC-derived rows the current `classify` emits; in the reworked model
they are **not hooks** and move to verbose mode (§5):

- `SetStatus` / `SetUnitStatus` / `SetApplicationStatus` → status changes. As
  standalone rows they stay in verbose mode and the **Status** pane. But because
  a status set inside a hook is caused by that hook, M10 also folds it onto the
  hook's own row as an inline suffix (§10) — so the default timeline still shows
  which hook drove the charm's status without a separate row.
- `EnterScope` / `LeaveScope` → these are the RPCs that *accompany* the
  `relation-joined` / `relation-departed` **hooks**; the hook is the event, the
  scope RPC is drill-down.
- `SecretsManager.*` → these are the charm *managing* secrets via jujuc
  (`secret-add`, `secret-grant`), not the secret **hooks** firing. Drill-down.
- Leadership-facade calls → the `leader-elected` **hook** is the event.

## 5. Verbose view (`.`)

`.` toggles verbose mode. It is a **display filter**, not a reload — the
underlying event list is unchanged; verbose mode simply stops suppressing the
non-hook rows and annotates the hook rows with more inline detail.

In verbose mode:

1. **Raw transitions reappear** interleaved by timestamp: status changes
   (`→ active`), `EnterScope`/`LeaveScope`, secret-management RPCs, and any
   `CommitHookChanges` that fell outside a hook bracket.
2. **Hook rows gain inline detail** after the name: duration on the end line,
   remote unit / relation id for relation hooks, exit status, and a `db` pip
   when the enclosed commit changed a databag.
3. **Sub-span count** is shown on the start line (`+7 rpc`) so the user sees how
   much traffic the hook generated without opening the inspector.

The `.` binding is added to `keymap` in `tui.go` (suggested field `Verbose`,
`key.WithHelp(".", "verbose")`) and rendered in the help footer.

## 6. Rendering

Default row shape — `timestamp`, `unit`, and the hook, tab/space-aligned so the
columns line up (the pane already pads `unit` to 12 via `eventRow`):

```
DEFAULT
15:04:12.104  grafana/0     ┌ install
15:04:12.584  grafana/0     └ install                     (480ms)
15:04:14.882  grafana/0     ┌ config-changed
15:04:15.082  grafana/0     └ config-changed              ✓ (200ms)
15:04:17.010  grafana/0     ┌ grafana-source-relation-joined
15:04:17.430  grafana/0     └ grafana-source-relation-joined  ✎db (420ms)
15:04:18.220  prometheus/0  ┌ grafana-source-relation-changed
15:04:18.560  prometheus/0  ✗ grafana-source-relation-changed  error
```

Note the trailing word on a failed hook is one of three (§9): `error` (red — the
charm actually broke and the uniter retried it), `interrupted` (dim — a later
hook opened before this one finished, usually a truncated recording), or
`rpc error` (amber — the hook completed but one of its RPCs returned an error,
which the charm often caught). Only `error` colours the hook glyph red.

A hook also carries, inline, the status it drove the charm into (§10) — because
every status a charm reports is set from inside a running hook:

```
15:04:15.082  grafana/0     └ config-changed              (200ms)  → blocked "waiting for db"
```


```
VERBOSE  (. pressed) — same window, raw transitions folded back in
15:04:14.882  grafana/0     ┌ config-changed              +3 rpc
15:04:14.902  grafana/0     ·   → maintenance  "reconciling config"
15:04:15.082  grafana/0     └ config-changed              ✓ (200ms)
15:04:16.552  grafana/0     ·   EnterScope  rel 3
15:04:17.010  grafana/0     ┌ grafana-source-relation-joined  remote prometheus/0  +5 rpc
15:04:17.244  grafana/0     ·   CommitHookChanges  ✎db rel 3
15:04:17.430  grafana/0     └ grafana-source-relation-joined  ✎db (420ms)
```

Glyphs (extend `event.glyph`):

| Glyph | Meaning |
|---|---|
| `┌` | hook start |
| `└` | hook end (succeeded) |
| `✗` | hook end (errored — the uniter retried it; see §9 for the three failure states) |
| `·` | verbose-only raw transition (indented under its hook) |
| `✎db` | the hook's commit changed a databag (M7; existing marker) |
| `⧖` | hook still open (start seen, no `continue` yet — live tail or truncated recording) |

Alignment: keep the existing `time · unit(12) · glyph · label` layout from
`eventRow`; the two-line model only changes which glyph and which suffix each
line gets. The end line repeats the full hook name (not a bare `└`) so a
scrolled viewport where the start line is off-screen is still readable.

## 7. Inspector (Enter) — identical for start and end

Because both events point at the same `hookRun`, Enter opens the same overlay
regardless of which line the cursor is on. A hook is inspectable when it changed
a databag, is a `config-changed`, **failed in any of the three ways (§9), or set
a status (§10)** — so a plain failed hook is no longer a dead Enter. It shows
(reusing `details.go`):

- **Header**: full hook name, unit, and the facade.method behind the
  representative span.
- **Failure line** (§9): a plain-language explanation of how the run went wrong —
  "hook errored — the unit went to error state and the uniter retried it",
  "hook interrupted — another hook opened before this one finished (often a
  truncated recording)", or "an RPC in this hook returned an error (the hook
  still completed): <message>".
- **Status set by this hook** (§10): each workload/application status the charm
  reported from inside the hook, `unit → blocked "message"`, coloured by value.
- **Databags**: the delta the enclosed `CommitHookChanges` wrote (M7), diffable
  with `d`. Unchanged.
- **Logs**: correlated debug-log lines live in the Logs pane, which locks to the
  selected event by default; span-id exact matches are marked `»`.

## 8. Edge cases

- **Recorder started mid-hook** (no `run-hook` marker seen): we never saw a
  start, so synthesise a start event at the first captured RPC for that unit and
  mark it `initial` (chevron), mirroring the mid-life snapshot handling in
  VISION §5.5. If a `ran "X" hook` debug-log line names the hook, use it for the
  label; otherwise label the run `hook?` and rely on the enclosed
  `CommitHookChanges` for identity.
- **Hook still running at tail** (`--follow`): start seen, no `continue`. Emit
  only the start event, glyph `⧖`; the end event appears once the `continue`
  marker is indexed on a later `Sync`. An open hook at the live tail is
  **running, not failed** — unless it was already retried, in which case it is
  in error state now (§9).
- **Failed hook**: see §9. The four failure states are distinguished from one
  another and from a live-tail open hook, so the timeline no longer conflates a
  unit stuck in error now with one that recovered after a retry, nor a real
  crash with a truncated recording.
- **Hook with no `CommitHookChanges`** (e.g. an `update-status` that touched
  nothing): still a full hook run with start+end from the markers — the two-line
  event is drawn, `hasDatabag=false`, and the Network drill-down simply lists
  fewer sub-spans. This is the key correctness win over the current model, which
  can only see hooks that happened to commit.

## 9. Failure classification (M10)

The original model had a single boolean `failed` that was set when *any* RPC in
a hook's bracket returned an error, or when a hook's bracket was interrupted.
That conflated several very different situations and, worse, was disconnected
from whether the charm actually broke — a hook could read "failed" while the
unit sat `active/idle`. M10 replaces it with a `failState` derived from the
uniter's own operation log.

The uniter records its progress through an operation in the same
`Uniter.SetState` `uniter-state` blob we already parse (§2): `op: run-hook` with
an `opstep:` that walks `pending → done` on success, then `op: continue`. When a
hook **errors**, the uniter does *not* advance to `continue`; it re-enters the
operation, re-writing `op: run-hook` with `opstep: pending` for the *same* hook —
this is its retry loop. `ParseHookMarker` now also extracts `opstep`, and
`buildHookRuns` counts these retries.

The key subtlety: a retried hook usually **recovers**. The uniter retries a
failed hook until it succeeds, then writes `continue`. So a hook that was retried
*and reached `continue`* is history — it errored, Juju retried, and the charm
recovered (which is exactly why such a hook can sit right next to a `→ active`
status). The only hook whose unit is in error state *right now* is one that was
retried and has **not** reached `continue`. The distinguisher is therefore
whether the bracket closed, not merely whether it was retried. Conversely, an
errored *RPC* inside a completed hook never changes charm status (Juju's `error`
status comes only from the hook process exiting non-zero, not from an API call
erroring), so it stays a soft warning.

The four states (`internal/viewer/events.go`, `failState`):

| State | Detected by | Timeline | Meaning |
|---|---|---|---|
| `failErrored` | retried (`run-hook`/`opstep: pending` reappeared) **and** never reached `continue` | red glyph, `error` | the unit is in error state *now* |
| `failRetried` | retried **but** then reached `continue` | dim, `retried` | it errored, Juju retried, and it then succeeded — history, not a current problem |
| `failInterrupted` | a **different** hook opened before this one's `continue` | dim, `interrupted` | almost always a truncated recording, not a charm bug |
| `failRPCWarn` | the hook **completed** but one of its RPCs returned an error | amber, `rpc error` | often a call the charm caught; does not affect charm status |

Precedence: retried-and-open (`failErrored`) beats the plain live-tail "running"
state, so a unit stuck retrying is never hidden as merely "running". A never-
retried open hook at the tail is still just "running".

Why this is trustworthy where the old boolean was not: `failErrored` is sourced
from the uniter's retry loop *without* a recovering `continue` — the same state
that leaves the unit in `error` status — so it corroborates the Status pane (§10)
instead of contradicting it. `failRetried` explains the otherwise-confusing
"errored hook, active unit" pairing rather than mislabelling it red.

## 10. Status attribution (M10)

A charm only ever reports status from inside a running hook — that is how charm
execution works: Juju wakes the charm with an event (hook), the charm runs, and
calls `status-set` before it returns. So every status change *can* be attributed
to the hook that caused it, and the timeline should show it there.

The join is already in the index and needs no new capture:

1. A status snapshot records `ProducingSpanID` = the `SetStatus` /
   `SetUnitStatus` / `SetApplicationStatus` span (`internal/index/extract.go`).
2. That span was made *inside* a hook bracket, so `buildHookRuns` already folds
   its id into the hook run's `spanIDs`.
3. `DB.StatusBySpan` returns `producing-span → status`, and
   `buildEventsWithStatus` attaches to each hook every status whose producing
   span it owns.

Rendered:

- **Timeline**: an inline suffix on the hook row, `→ blocked "waiting for db"`
  (app statuses read `app → …`), coloured by status value. This is what lets the
  user scrub the timeline and see *which hook* drove the charm into
  blocked/active/error without opening anything.
- **Inspector**: a "status set by this hook" section listing each transition.

Two origins of status, two mechanisms, deliberately complementary:

- **Charm-driven** status (`status-set` from hook code) → the `ProducingSpanID`
  join above. Exact, by span id.
- **Juju-driven** `error` status (set by the controller when a hook crashes, not
  by charm code) → surfaced by §9's `failErrored`, not by a `status-set` span.

Together they cover both ways a unit's status can move.

## 11. Implementation notes

Touches, roughly:

- `internal/index/hooks.go` — `HookMarker.Opstep` and its regex, so the viewer
  can tell the uniter's retry loop from normal opstep progression.
- `internal/index/db.go` — `DB.StatusBySpan(modelID, kind)` returning
  `producing-span → status snapshot`.
- `internal/viewer/events.go` — `failState` + `hookRun.classify`, retry/supersede
  tracking in `buildHookRuns`, `spanIDs` collection, and
  `buildEventsWithStatus` + `statusMapFromSnapshots` for status attribution.
- `internal/viewer/timeline.go` — three-way failure suffix and the status suffix.
- `internal/viewer/details.go` — `inspectable` now true for failures/statuses;
  `renderFailure` and `renderStatusChanges` sections.
- `internal/viewer/tui.go` — `setActiveModel` loads the status-by-span maps and
  feeds `buildEventsWithStatus`.
