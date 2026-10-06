// Package narrative distils raw captured spans into the causal story of what
// a unit did: which hooks ran, whether they failed/retried/recovered, and
// what statuses/databags/config they changed. It is the TUI-agnostic core
// that both the viewer (rendered interactively with bubbletea/lipgloss) and
// the export command (rendered as JSON/Markdown) build on, so there is one
// implementation of "what happened" instead of two that can drift apart.
//
// This package depends only on internal/recording and internal/index; it
// must never import bubbletea/lipgloss/tea — anything that renders to a
// terminal belongs in internal/viewer, anything that formats a document
// belongs in internal/export.
package narrative

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

// Event is a meaningful timeline transition. The default timeline shows one
// thing: the Juju hooks a unit ran (docs/explanation/rpcs-to-hooks.md),
// one row per hook execution — from the run-hook marker to the matching
// continue — carrying the hook's duration.
//
// The raw RPC transitions the hooks are made of (status setters, EnterScope,
// secret management) are also events, but they are VerboseOnly: hidden until
// the caller opts in. Each event keeps a representative span so a consumer
// can correlate to it (logs, the inspector, the export detail gatherer).
type Event struct {
	TS          time.Time
	Kind        EventKind
	Unit        string        // "grafana/0" ("" for controller-side events)
	App         string        // "grafana"
	Summary     string        // charm-visible hook name ("grafana-source-relation-changed") or transition
	Detail      string        // optional trailing context (status message)
	SpanID      string        // representative span (the commit, or the run-hook marker)
	Fail        FailState     // how (if at all) this hook run went wrong (M10)
	Statuses    []HookStatus  // the status this marker sets (EvStatus only, M10/M12)
	HasDatabag  bool          // the hook's commit actually changed a databag (M7)
	Dur         time.Duration // hook duration (run-hook → continue)
	Running     bool          // hook still open at the tail of a live recording
	VerboseOnly bool          // a raw transition: hidden in the default view

	SettleWorkload string // workload status at an EvSettle marker (for colouring)
	Cause          string // the hook that produced a promoted EvStatus marker (M12)
}

// FailState classifies how a hook run ended. A single boolean "failed" would
// conflate three very different things (M10): splitting them lets a consumer
// tell whether the charm actually broke, or whether it merely made a call
// that errored, or whether the recording is just truncated.
type FailState int

const (
	FailNone         FailState = iota
	FailErrored                // retried and still not completed at the tail: the unit is in error state now
	FailRetried                // errored and retried, but then reached `continue`: recovered
	FailInterrupted            // superseded by another hook that never reached `done`: the hook was genuinely abandoned mid-run
	FailLostContinue           // superseded, but this hook itself reached `done` (and may have committed): its own `continue` marker is simply missing — almost always a dropped span, not a charm or uniter event
	FailRPCWarn                // the hook completed, but one of its RPCs returned an error (often caught by the charm)
)

// String names the FailState for diagnostics, the export command, and the
// dump tool.
func (f FailState) String() string {
	switch f {
	case FailErrored:
		return "errored"
	case FailRetried:
		return "retried"
	case FailInterrupted:
		return "interrupted"
	case FailLostContinue:
		return "lost-continue"
	case FailRPCWarn:
		return "rpc-warn"
	default:
		return "none"
	}
}

// HookStatus is a workload/application status the charm set from inside a
// hook, attributed to that hook by its producing span (M10). Every status a
// charm reports is set while a hook runs, so a consumer can show which hook
// drove the charm into blocked/active/error.
type HookStatus struct {
	App       bool   // an application status (vs a per-unit workload status)
	Value     string // "active", "blocked", …
	Message   string // the status message, if any
	Redundant bool   // repeats the scope's previous value+message (hidden unless verbose)
}

type EventKind int

const (
	EvHook EventKind = iota
	EvStatus
	EvRelation
	EvLeader
	EvAction
	EvSecret
	EvSettle // a unit came to rest: agent idle for a while, or idle at the tail
)

// BuildEvents distils a span list (sorted by start) into events: one per
// hook run, plus the verbose-only raw transitions. databagChanged marks the
// commit spans that actually moved a databag (M7) so the row can carry the
// "(databag changes)" tag. The result is sorted by timestamp.
func BuildEvents(spans []recording.SpanRow, databagChanged map[string]bool) []Event {
	return BuildEventsWithStatus(spans, databagChanged, nil)
}

// BuildEventsWithStatus is BuildEvents with the status attribution (M10):
// given a map from a status-setter span id to the status it set, each status
// whose producing span falls inside a hook's bracket becomes its own EvStatus
// marker (M12) — not a suffix riding on the hook row — positioned at that
// setter RPC's own instant rather than the hook's start. A hook's status
// takes effect partway through (or at the end of) its run, so a row claiming
// the outcome at the hook's *start* would be misleading the moment another
// unit's hook is also open across that same window (docs on the Events pane
// non-monotonicity discussion). Positioning the change at its true instant
// instead means the row never has to claim more than it knows.
func BuildEventsWithStatus(spans []recording.SpanRow, databagChanged map[string]bool, statusBySpan map[string]HookStatus) []Event {
	resolver := relationEndpointMap(spans)
	spanByID := make(map[string]recording.SpanRow, len(spans))
	for _, sp := range spans {
		spanByID[sp.SpanID] = sp
	}
	var out []Event
	for _, r := range BuildHookRuns(spans) {
		ev := Event{
			TS: r.StartTs, Kind: EvHook, Unit: r.Unit, App: r.App,
			Summary: hookDisplayName(r, resolver), SpanID: r.RepSpan,
			Fail: r.Fail, HasDatabag: databagChanged[r.RepSpan],
		}
		if r.Open {
			ev.Running = true
		} else {
			ev.Dur = r.EndTs.Sub(r.StartTs)
		}
		out = append(out, ev)
		for _, spanID := range r.SpanIDs {
			st, ok := statusBySpan[spanID]
			if !ok {
				continue
			}
			// ev.Summary is the hook's already-resolved display name
			// (relation/storage endpoints included), reused as-is for Cause
			// rather than re-deriving it without the resolver in scope.
			out = append(out, statusMarkerEvent(r, spanByID[spanID], st, ev.Summary))
		}
	}
	for _, sp := range spans {
		if ev, ok := rawTransition(sp); ok {
			out = append(out, ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	out = append(out, settleEvents(spans)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	markRedundantStatuses(out)
	hideRedundantMarkers(out)
	return out
}

// statusMarkerEvent builds the standalone EvStatus row for one status a hook
// set: st is the attributed HookStatus, sp the setter RPC span that produced
// it (its Start is the marker's TS), r the hook run that carries it, and
// cause the hook's already-resolved display name, so the row reads
// "→ active  (config-changed)" without requiring the originating hook row to
// still be on screen.
func statusMarkerEvent(r HookRun, sp recording.SpanRow, st HookStatus, cause string) Event {
	ev := Event{
		TS: sp.Start, Kind: EvStatus, Unit: r.Unit, App: r.App,
		SpanID: sp.SpanID, Statuses: []HookStatus{st}, Cause: cause,
	}
	ev.Summary = "→ " + st.Value
	if st.App {
		ev.Summary = "app → " + st.Value
	}
	return ev
}

// settleQuiescence is how long a unit's agent must stay idle — with nothing
// else processed for that charm — for the rest to count as "settled". A
// shorter idle gap is just the queue briefly draining between back-to-back
// hooks and is not surfaced. This is checked the same way whether the next
// thing is another hook or simply the end of the recording: idle at the tail
// only counts once at least settleQuiescence of the recording was actually
// captured after it, not merely because there was nothing left to capture.
const settleQuiescence = 3 * time.Second

// settleEvents emits one EvSettle marker per time a unit comes to rest: its
// agent goes idle and then stays idle for at least settleQuiescence before the
// next executing/hook, or before the recording's own tail. This gives a
// selectable row testifying that a unit reached e.g. "active / idle", which
// otherwise happens in the gap between hooks and so has no row of its own. The
// workload value shown is whatever the unit's latest SetUnitStatus set at that
// instant (juju's agent axis is idle here by definition).
func settleEvents(spans []recording.SpanRow) []Event {
	if len(spans) == 0 {
		return nil
	}
	var end time.Time
	for _, sp := range spans {
		if sp.Start.After(end) {
			end = sp.Start
		}
	}
	// Per unit, replay agent + workload status RPCs in time order.
	type agentSample struct {
		ts    time.Time
		value string
		span  string
	}
	agentByUnit := map[string][]agentSample{}
	workloadByUnit := map[string][]agentSample{} // reuse shape: value is workload status
	for _, sp := range spans {
		if sp.Unit == "" {
			continue
		}
		method := sp.Attrs["method"]
		switch method {
		case "SetAgentStatus":
			val, _ := statusFromParams(sp.Attrs["params"])
			agentByUnit[sp.Unit] = append(agentByUnit[sp.Unit], agentSample{sp.Start, val, sp.SpanID})
		case "SetStatus", "SetUnitStatus":
			val, _ := statusFromParams(sp.Attrs["params"])
			workloadByUnit[sp.Unit] = append(workloadByUnit[sp.Unit], agentSample{sp.Start, val, sp.SpanID})
		}
	}
	// workloadAt returns the workload status in effect for unit at time t.
	workloadAt := func(unit string, t time.Time) string {
		val := ""
		for _, s := range workloadByUnit[unit] {
			if s.ts.After(t) {
				break
			}
			val = s.value
		}
		return val
	}
	var out []Event
	for unit, samples := range agentByUnit {
		for i, s := range samples {
			if s.value != "idle" {
				continue
			}
			// When does this idle end? At the next agent sample (the next
			// executing), else at the recording tail. Either way, the same
			// minimum gap applies — a unit idle for only a few ms before the
			// recording happens to stop is not evidence it settled, just that
			// nothing more was captured.
			restUntil := end
			if i+1 < len(samples) {
				restUntil = samples[i+1].ts
			}
			if restUntil.Sub(s.ts) < settleQuiescence {
				continue // momentary drain between bursts, not a real rest
			}
			wl := workloadAt(unit, s.ts)
			ev := Event{
				TS:     s.ts,
				Kind:   EvSettle,
				Unit:   unit,
				App:    appOf(unit),
				SpanID: s.span,
			}
			if wl == "" {
				wl = "unknown"
			}
			ev.Summary = "→ " + wl + " / idle"
			ev.SettleWorkload = wl
			out = append(out, ev)
		}
	}
	return out
}

// markRedundantStatuses flags each hook status that merely repeats the last
// value+message its scope was already set to, walking events in time order. A
// scope is a unit (workload status) or an application (app status). Redundant
// statuses are hidden in the default view — a charm re-asserting "active" on
// every hook is noise — but stay available in verbose mode.
func markRedundantStatuses(events []Event) {
	last := map[string]string{} // scope -> "value\x00message"
	for i := range events {
		for j := range events[i].Statuses {
			s := &events[i].Statuses[j]
			scope := "unit:" + events[i].Unit
			if s.App {
				scope = "app:" + events[i].App
			}
			cur := s.Value + "\x00" + s.Message
			if prev, ok := last[scope]; ok && prev == cur {
				s.Redundant = true
			}
			last[scope] = cur
		}
	}
}

// hideRedundantMarkers demotes a promoted status-change marker (the EvStatus
// rows statusMarkerEvent builds, M12) to verbose-only once markRedundantStatuses
// has flagged its one status as a repeat of the scope's previous value+message.
// A charm re-asserting "active" every hook then stays available in verbose
// mode without cluttering the default Events list with a marker for a change
// that did not actually happen. Raw per-RPC EvStatus transitions (already
// verbose-only, carrying no Statuses) are untouched by the len check.
func hideRedundantMarkers(events []Event) {
	for i := range events {
		if events[i].Kind == EvStatus && len(events[i].Statuses) == 1 && events[i].Statuses[0].Redundant {
			events[i].VerboseOnly = true
		}
	}
}

// HookRun is one execution of a hook on a unit: the run-hook..continue bracket
// the uniter writes in its SetState blobs (docs/explanation/rpcs-to-hooks.md).
// RepSpan is the span a consumer drills into — the CommitHookChanges when the
// hook committed, else the run-hook marker itself. RemoteApp/StorageID come from
// the run-hook marker and reconstruct the charm-visible hook name.
type HookRun struct {
	Unit       string
	App        string
	Kind       string // bare kind from the marker, e.g. "relation-changed"
	RemoteApp  string // relation hooks: the remote application
	StorageID  string // storage hooks: e.g. "data/0"
	StartTs    time.Time
	EndTs      time.Time
	RepSpan    string
	SpanIDs    []string  // every span folded into this run (for status attribution, M10)
	RPCErr     bool      // an RPC inside the bracket returned an error (M10)
	Retries    int       // times `opstep: pending` reappeared after already being seen once: the uniter's retry loop (M10)
	Superseded bool      // a *different* hook opened before this one's `continue`: interrupted or lost-continue (M10)
	Fail       FailState // how the run went wrong, if at all (M10)
	Open       bool      // start marker seen, end not yet (live tail / truncated recording)

	seenPending bool // opstep:pending already seen once for this bracket — a second one is a real retry, not the normal queued→pending step
	doneSeen    bool // opstep:done already seen for this bracket — distinguishes FailInterrupted (abandoned mid-run) from FailLostContinue (finished, but its own continue marker is missing) when Superseded
}

// classify decides how a hook run went wrong, if at all. The distinguisher for a
// retried hook is whether it reached `continue`: the uniter re-writes the same
// run-hook marker each time it retries a failed hook, then writes `continue`
// once the retry finally succeeds (docs/explanation/rpcs-to-hooks.md).
//
//   - retried and still open at the tail  → FailErrored: the unit is in error
//     state *now*; this is the only red, "the charm is broken" signal.
//   - retried but reached `continue`       → FailRetried: it errored, Juju
//     retried, and the charm recovered — history, not a current problem. This is
//     why a "retried" hook can sit right next to a "→ active" status.
//   - a different hook opened first, and this one never reached `done`
//     → FailInterrupted: genuinely abandoned
//     mid-run — a truncated recording, an agent restart, or the unit being torn
//     down (`stop` firing mid-hook), not a charm bug.
//   - a different hook opened first, but this one already reached `done`
//     → FailLostContinue: the hook actually
//     finished (it may even have committed changes) — only its own `continue`
//     marker is missing from the capture. This is almost always a dropped span
//     (e.g. a capture-side buffer overrun during a burst of activity), not
//     anything the uniter or charm did.
//   - completed with an errored RPC        → FailRPCWarn: usually a call the
//     charm caught; it does not change charm status, so it stays a soft warning.
func (r *HookRun) classify() {
	switch {
	case r.Retries > 0 && r.Open:
		r.Fail = FailErrored
	case r.Retries > 0:
		r.Fail = FailRetried
	case r.Superseded && r.doneSeen:
		r.Fail = FailLostContinue
	case r.Superseded:
		r.Fail = FailInterrupted
	case r.RPCErr:
		r.Fail = FailRPCWarn
	default:
		r.Fail = FailNone
	}
}

// BuildHookRuns pairs each unit's run-hook and continue SetState markers into
// hook runs. The uniter persists run-hook more than once per hook (opstep
// pending, then done); a run-hook for the hook already open is that later
// opstep, not a new hook. When no run-hook marker is present (the recorder
// attached mid-hook, or synthetic test data) it falls back to a maximal run of
// consecutive spans sharing the same labelled Hook (docs/explanation/rpcs-to-hooks.md).
func BuildHookRuns(spans []recording.SpanRow) []HookRun {
	byUnit := map[string][]recording.SpanRow{}
	var order []string
	for _, sp := range spans {
		if sp.Unit == "" {
			continue
		}
		if _, seen := byUnit[sp.Unit]; !seen {
			order = append(order, sp.Unit)
		}
		byUnit[sp.Unit] = append(byUnit[sp.Unit], sp)
	}

	var runs []HookRun
	for _, unit := range order {
		var cur *HookRun
		var lastEnd time.Time // End of the newest span folded into cur
		synthetic := false    // cur was inferred without a run-hook marker

		closeRun := func() {
			if cur == nil {
				return
			}
			if cur.EndTs.IsZero() {
				if synthetic && !lastEnd.IsZero() {
					cur.EndTs = lastEnd
				} else if synthetic {
					cur.EndTs = cur.StartTs
				} else {
					cur.Open = true
				}
			}
			// Classify every closed bracket, and open brackets too: an open hook
			// that was already retried is a unit stuck in error state *now*
			// (FailErrored), distinct from an open hook that is simply still
			// running at the live tail (M10).
			cur.classify()
			runs = append(runs, *cur)
			cur, synthetic, lastEnd = nil, false, time.Time{}
		}

		for _, sp := range byUnit[unit] {
			if sp.Attrs["method"] == "SetState" {
				if hm := index.ParseHookMarker(sp.Attrs["params"]); hm.Op != "" {
					switch {
					case hm.Op == "run-hook" && hm.Kind != "":
						// A run-hook for the hook already open is a later opstep,
						// not a new hook: keep the current run. The uniter's normal,
						// error-free lifecycle for a single hook execution walks
						// opstep queued→pending→done — so the *first* `pending` is
						// routine, not a retry. Only a *second* `pending` (with no
						// intervening `continue`) is the retry loop the uniter
						// enters when the hook actually errored — the high-
						// confidence "unit went to error state" signal (M10). This
						// is why seenPending, not merely "opstep == pending", gates
						// the increment.
						if cur != nil && cur.Kind == hm.Kind {
							switch hm.Opstep {
							case "pending":
								if cur.seenPending {
									cur.Retries++
								}
								cur.seenPending = true
							case "done":
								cur.doneSeen = true
							}
							continue
						}
						if cur != nil { // a different hook opened with no continue: interrupted or lost-continue (classify decides which)
							cur.Superseded = true
							cur.EndTs = sp.Start // the next hook's start bounds this one
							closeRun()
						}
						cur = &HookRun{
							Unit: unit, App: appOf(unit), Kind: hm.Kind,
							RemoteApp: hm.RemoteApp, StorageID: hm.StorageID,
							StartTs: sp.Start, RepSpan: sp.SpanID,
						}
						cur.seenPending = hm.Opstep == "pending"
						cur.doneSeen = hm.Opstep == "done"
						cur.SpanIDs = append(cur.SpanIDs, sp.SpanID)
						continue
					default: // "continue" (or another op) ends the current hook
						if cur != nil {
							cur.EndTs = sp.Start
							closeRun()
						}
						continue
					}
				}
			}
			// A non-marker span. Start a synthetic run when it belongs to a hook we
			// never saw open, or when the labelled hook changed under us.
			if sp.Hook != "" && (cur == nil || sp.Hook != cur.Kind) {
				closeRun()
				cur = &HookRun{Unit: unit, App: appOf(unit), Kind: sp.Hook, StartTs: sp.Start, RepSpan: sp.SpanID}
				synthetic = true
			}
			if cur != nil {
				lastEnd = sp.End
				cur.SpanIDs = append(cur.SpanIDs, sp.SpanID)
				if sp.StatusCode == "ERROR" {
					cur.RPCErr = true
				}
				if sp.Attrs["method"] == "CommitHookChanges" {
					cur.RepSpan = sp.SpanID // prefer the commit for the inspector/databag
				}
			}
		}
		closeRun()
	}
	return runs
}

// snapBody mirrors the JSON shape written by index.ExtractSnapshots. Kept
// separately (not shared with the extract package, and not shared with
// internal/viewer's own copy) so narrative never depends on another
// package's internal types — the same duplication the codebase already
// tolerates for appOf.
type snapBody struct {
	Value   string    `json:"value"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
}

// StatusMapFromSnapshots turns the index's span→status-snapshot maps (unit and
// application) into the span→HookStatus map BuildEventsWithStatus consumes. The
// snapshot body is the {value,message,since} JSON the extractor wrote (M10).
func StatusMapFromSnapshots(unit, app map[string]index.SnapshotRow) map[string]HookStatus {
	out := make(map[string]HookStatus, len(unit)+len(app))
	add := func(m map[string]index.SnapshotRow, isApp bool) {
		for span, r := range m {
			var body snapBody
			if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
				continue
			}
			out[span] = HookStatus{App: isApp, Value: body.Value, Message: body.Message}
		}
	}
	add(unit, false)
	add(app, true)
	return out
}

// hookDisplayName reconstructs the charm-visible hook name. The wire form leaves
// relation and storage hooks as a bare kind ("relation-changed"); Juju names
// them "<endpoint>-relation-changed" / "<storage>-storage-attached". For
// relation hooks the endpoint comes from the marker's remote-application matched
// against the unit's relations (resolver); synthetic runs whose kind already
// carries the endpoint pass through unchanged.
func hookDisplayName(r HookRun, resolver map[string]map[string]string) string {
	switch {
	case isRelationHook(r.Kind):
		if ep := resolver[r.App][r.RemoteApp]; ep != "" {
			return ep + "-" + r.Kind
		}
	case isStorageHook(r.Kind):
		if name := storageName(r.StorageID); name != "" {
			return name + "-" + r.Kind
		}
	}
	return r.Kind
}

func isRelationHook(kind string) bool {
	switch kind {
	case "relation-created", "relation-joined", "relation-changed", "relation-departed", "relation-broken":
		return true
	}
	return false
}

func isStorageHook(kind string) bool {
	return kind == "storage-attached" || kind == "storage-detaching"
}

// storageName turns a storage id ("data/0") into its storage name ("data").
func storageName(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return id
}

// relationEndpointMap builds, per application, a map from a remote application to
// the local endpoint name of the relation between them, derived from the relation
// keys the indexer attached to spans ("<app>.<endpoint>#<app>.<endpoint>", or
// "<app>.<endpoint>" for a peer relation). This is what lets a bare
// "relation-changed" become "grafana-source-relation-changed".
func relationEndpointMap(spans []recording.SpanRow) map[string]map[string]string {
	m := map[string]map[string]string{}
	for _, sp := range spans {
		if sp.Relation == "" || sp.Unit == "" {
			continue
		}
		app := appOf(sp.Unit)
		localEp, remoteApp, ok := parseRelationKey(sp.Relation, app)
		if !ok {
			continue
		}
		if m[app] == nil {
			m[app] = map[string]string{}
		}
		m[app][remoteApp] = localEp
	}
	return m
}

// parseRelationKey pulls the local endpoint and the remote application out of a
// relation key, from the perspective of localApp. For a peer relation (single
// segment) the remote application is localApp itself.
func parseRelationKey(key, localApp string) (localEp, remoteApp string, ok bool) {
	type seg struct{ app, ep string }
	var segs []seg
	for _, s := range strings.Split(key, "#") {
		dot := strings.IndexByte(s, '.')
		if dot < 0 {
			return "", "", false
		}
		segs = append(segs, seg{app: s[:dot], ep: s[dot+1:]})
	}
	localIdx := -1
	for i, s := range segs {
		if s.app == localApp {
			localIdx, localEp = i, s.ep
			break
		}
	}
	if localIdx < 0 {
		return "", "", false
	}
	remoteApp = localApp // peer default
	for i, s := range segs {
		if i != localIdx {
			remoteApp = s.app
			break
		}
	}
	return localEp, remoteApp, true
}

// rawTransition turns a span into a verbose-only event, or reports ok=false to
// drop it. These are the RPC side effects the default timeline suppresses: the
// status setters (which live in the Status pane), scope changes, secret
// management and actions. Spans that belong to a hook (sp.Hook set) are dropped
// here — they surface inside that hook's inspector instead of doubling as their
// own row (docs/explanation/rpcs-to-hooks.md).
func rawTransition(sp recording.SpanRow) (Event, bool) {
	if sp.Hook != "" {
		return Event{}, false
	}
	facade := sp.Attrs["facade"]
	method := sp.Attrs["method"]
	base := Event{
		TS: sp.Start, Unit: sp.Unit, App: appOf(sp.Unit), SpanID: sp.SpanID,
		VerboseOnly: true,
	}
	if sp.StatusCode == "ERROR" {
		base.Fail = FailRPCWarn
	}
	switch {
	case method == "SetApplicationStatus":
		return statusEvent(base, sp, true), true
	case method == "SetStatus" || method == "SetUnitStatus":
		return statusEvent(base, sp, false), true
	case method == "EnterScope":
		base.Kind, base.Summary = EvRelation, "EnterScope"
		return base, true
	case method == "LeaveScope":
		base.Kind, base.Summary = EvRelation, "LeaveScope"
		return base, true
	case strings.Contains(facade, "Leadership") || method == "ClaimLeadership":
		base.Kind, base.Summary = EvLeader, "ClaimLeadership"
		return base, true
	case strings.HasPrefix(facade, "Secrets"):
		base.Kind = EvSecret
		base.Summary = "secret-manage: " + strings.ToLower(strings.TrimPrefix(method, "Secret"))
		return base, true
	case facade == "Action" || strings.Contains(method, "Action"):
		base.Kind, base.Summary = EvAction, "action"
		return base, true
	}
	return Event{}, false
}

// statusEvent fills a status-change event, parsing the new value/message from
// the setter's params. app marks an application status (vs a per-unit status).
func statusEvent(base Event, sp recording.SpanRow, app bool) Event {
	base.Kind = EvStatus
	val, msg := statusFromParams(sp.Attrs["params"])
	base.Summary = "→ " + val
	if app {
		base.Summary = "app → " + val
	}
	base.Detail = msg
	return base
}

// statusFromParams pulls the first entity's status value/message out of a
// Uniter status setter's params.
func statusFromParams(params string) (value, message string) {
	if params == "" {
		return "", ""
	}
	var args struct {
		Entities []struct {
			Status string `json:"status"`
			Info   string `json:"info"`
		} `json:"entities"`
	}
	if err := json.Unmarshal([]byte(params), &args); err != nil || len(args.Entities) == 0 {
		return "", ""
	}
	return args.Entities[0].Status, args.Entities[0].Info
}

// appOf extracts the application name from a unit tag ("grafana/0" ->
// "grafana"). Deliberately duplicated per package (internal/viewer and
// internal/index each keep their own copy too) rather than exported from one
// of them, so no package depends on another's internals for a four-line
// helper.
func appOf(unit string) string {
	if i := strings.IndexByte(unit, '/'); i >= 0 {
		return unit[:i]
	}
	return unit
}
