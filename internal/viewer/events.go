package viewer

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

// event is a meaningful timeline transition. The default timeline shows one
// thing: the Juju hooks a unit ran (VISION §6.2; docs/event-timeline.md), one
// row per hook execution — from the run-hook marker to the matching continue —
// carrying the hook's duration.
//
// The raw RPC transitions the hooks are made of (status setters, EnterScope,
// secret management) are also events, but they are verboseOnly: hidden until the
// user presses `.` to reveal them. Each event keeps a representative span so the
// inspector and the log stream can correlate to it.
type event struct {
	ts          time.Time
	kind        eventKind
	unit        string        // "grafana/0" ("" for controller-side events)
	app         string        // "grafana"
	summary     string        // charm-visible hook name ("grafana-source-relation-changed") or transition
	detail      string        // optional trailing context (status message)
	spanID      string        // representative span (the commit, or the run-hook marker)
	fail        failState     // how (if at all) this hook run went wrong (M10)
	statuses    []hookStatus  // status changes this hook produced (M10)
	hasDatabag  bool          // the hook's commit actually changed a databag (M7)
	dur         time.Duration // hook duration (run-hook → continue)
	running     bool          // hook still open at the tail of a live recording
	verboseOnly bool          // a raw transition: hidden in the default view

	settleWorkload string // workload status at an evSettle marker (for colouring)
}

// failState classifies how a hook run ended. The old timeline had a single
// boolean "failed" that conflated three very different things (M10); splitting
// them lets the timeline tell the user whether the charm actually broke, or
// whether it merely made a call that errored, or whether the recording is just
// truncated.
type failState int

const (
	failNone        failState = iota
	failErrored               // retried and still not completed at the tail: the unit is in error state now
	failRetried               // errored and retried, but then reached `continue`: recovered
	failInterrupted           // superseded by another hook with no `continue`: usually a truncated recording
	failRPCWarn               // the hook completed, but one of its RPCs returned an error (often caught by the charm)
)

// String names the failState for diagnostics and the dump tool.
func (f failState) String() string {
	switch f {
	case failErrored:
		return "errored"
	case failRetried:
		return "retried"
	case failInterrupted:
		return "interrupted"
	case failRPCWarn:
		return "rpc-warn"
	default:
		return "none"
	}
}

// hookStatus is a workload/application status the charm set from inside a hook,
// attributed to that hook by its producing span (M10). Every status a charm
// reports is set while a hook runs, so the timeline can show which hook drove
// the charm into blocked/active/error without leaving the Events pane.
type hookStatus struct {
	app       bool   // an application status (vs a per-unit workload status)
	value     string // "active", "blocked", …
	message   string // the status message, if any
	redundant bool   // repeats the scope's previous value+message (hidden unless verbose)
}

type eventKind int

const (
	evHook eventKind = iota
	evStatus
	evRelation
	evLeader
	evAction
	evSecret
	evSettle // a unit came to rest: agent idle for a while, or idle at the tail
)

// ident is a stable key for one event, used to keep the selection pinned across
// a verbose-filter re-slice. Each event has a distinct representative span.
func (e event) ident() string { return e.spanID }

// buildEvents distils a span list (sorted by start) into the timeline's events:
// one per hook run, plus the verbose-only raw transitions. databagChanged marks
// the commit spans that actually moved a databag (M7) so the row can carry the
// ✎db pip. The result is sorted by timestamp.
func buildEvents(spans []recording.SpanRow, databagChanged map[string]bool) []event {
	return buildEventsWithStatus(spans, databagChanged, nil)
}

// buildEventsWithStatus is buildEvents with the status attribution (M10): given
// a map from a status-setter span id to the status it set, each hook run carries
// the statuses whose producing span falls inside its bracket. The viewer passes
// this in from the index; buildEvents (used by pure-span tests) passes nil.
func buildEventsWithStatus(spans []recording.SpanRow, databagChanged map[string]bool, statusBySpan map[string]hookStatus) []event {
	resolver := relationEndpointMap(spans)
	var out []event
	for _, r := range buildHookRuns(spans) {
		ev := event{
			ts: r.startTs, kind: evHook, unit: r.unit, app: r.app,
			summary: hookDisplayName(r, resolver), spanID: r.repSpan,
			fail: r.fail, hasDatabag: databagChanged[r.repSpan],
		}
		for _, spanID := range r.spanIDs {
			if st, ok := statusBySpan[spanID]; ok {
				ev.statuses = append(ev.statuses, st)
			}
		}
		if r.open {
			ev.running = true
		} else {
			ev.dur = r.endTs.Sub(r.startTs)
		}
		out = append(out, ev)
	}
	for _, sp := range spans {
		if ev, ok := rawTransition(sp); ok {
			out = append(out, ev)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ts.Before(out[j].ts) })
	out = append(out, settleEvents(spans)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ts.Before(out[j].ts) })
	markRedundantStatuses(out)
	return out
}

// settleQuiescence is how long a unit's agent must stay idle after a hook run
// for the rest to count as "settled". A shorter idle gap is just the queue
// briefly draining between back-to-back hooks and is not surfaced. The OTHER way
// to settle is being idle at the tail of the recording (no next hook at all),
// which is emitted regardless of how much idle time was captured.
const settleQuiescence = 5 * time.Second

// settleEvents emits one evSettle marker per time a unit comes to rest: its
// agent goes idle and then either (a) stays idle for at least settleQuiescence
// before the next executing/hook, OR (b) is still idle at the end of the
// recording. This gives a selectable row testifying that a unit reached e.g.
// "active / idle", which otherwise happens in the gap between hooks and so has no
// row of its own. The workload value shown is whatever the unit's latest
// SetUnitStatus set at that instant (juju's agent axis is idle here by
// definition).
func settleEvents(spans []recording.SpanRow) []event {
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
	var out []event
	for unit, samples := range agentByUnit {
		for i, s := range samples {
			if s.value != "idle" {
				continue
			}
			// When does this idle end? At the next agent sample (the next
			// executing), else at the recording tail.
			restUntil := end
			if i+1 < len(samples) {
				restUntil = samples[i+1].ts
			}
			atTail := i+1 == len(samples)
			if !atTail && restUntil.Sub(s.ts) < settleQuiescence {
				continue // momentary drain between bursts, not a real rest
			}
			wl := workloadAt(unit, s.ts)
			ev := event{
				ts:     s.ts,
				kind:   evSettle,
				unit:   unit,
				app:    appOf(unit),
				spanID: s.span,
			}
			if wl == "" {
				wl = "unknown"
			}
			ev.summary = "→ " + wl + " / idle"
			ev.settleWorkload = wl
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
func markRedundantStatuses(events []event) {
	last := map[string]string{} // scope -> "value\x00message"
	for i := range events {
		for j := range events[i].statuses {
			s := &events[i].statuses[j]
			scope := "unit:" + events[i].unit
			if s.app {
				scope = "app:" + events[i].app
			}
			cur := s.value + "\x00" + s.message
			if prev, ok := last[scope]; ok && prev == cur {
				s.redundant = true
			}
			last[scope] = cur
		}
	}
}

// hookRun is one execution of a hook on a unit: the run-hook..continue bracket
// the uniter writes in its SetState blobs (docs/event-timeline.md §2). repSpan is
// the span the inspector drills into — the CommitHookChanges when the hook
// committed, else the run-hook marker itself. remoteApp/storageID come from the
// run-hook marker and reconstruct the charm-visible hook name.
type hookRun struct {
	unit       string
	app        string
	kind       string // bare kind from the marker, e.g. "relation-changed"
	remoteApp  string // relation hooks: the remote application
	storageID  string // storage hooks: e.g. "data/0"
	startTs    time.Time
	endTs      time.Time
	repSpan    string
	spanIDs    []string  // every span folded into this run (for status attribution, M10)
	rpcErr     bool      // an RPC inside the bracket returned an error (M10)
	retries    int       // times the same run-hook marker reappeared with no `continue`: the uniter's retry loop (M10)
	superseded bool      // a *different* hook opened before this one's `continue`: interrupted (M10)
	fail       failState // how the run went wrong, if at all (M10)
	open       bool      // start marker seen, end not yet (live tail / truncated recording)
}

// classify decides how a hook run went wrong, if at all. The distinguisher for a
// retried hook is whether it reached `continue`: the uniter re-writes the same
// run-hook marker each time it retries a failed hook, then writes `continue`
// once the retry finally succeeds (docs/event-timeline.md §9).
//
//   - retried and still open at the tail  → failErrored: the unit is in error
//     state *now*; this is the only red, "the charm is broken" signal.
//   - retried but reached `continue`       → failRetried: it errored, Juju
//     retried, and the charm recovered — history, not a current problem. This is
//     why a "retried" hook can sit right next to a "→ active" status.
//   - a different hook opened first        → failInterrupted: a truncated
//     recording, not a charm bug.
//   - completed with an errored RPC        → failRPCWarn: usually a call the
//     charm caught; it does not change charm status, so it stays a soft warning.
func (r *hookRun) classify() {
	switch {
	case r.retries > 0 && r.open:
		r.fail = failErrored
	case r.retries > 0:
		r.fail = failRetried
	case r.superseded:
		r.fail = failInterrupted
	case r.rpcErr:
		r.fail = failRPCWarn
	default:
		r.fail = failNone
	}
}

// buildHookRuns pairs each unit's run-hook and continue SetState markers into
// hook runs. The uniter persists run-hook more than once per hook (opstep
// pending, then done); a run-hook for the hook already open is that later
// opstep, not a new hook. When no run-hook marker is present (the recorder
// attached mid-hook, or synthetic test data) it falls back to a maximal run of
// consecutive spans sharing the same labelled Hook (docs/event-timeline.md §8).
func buildHookRuns(spans []recording.SpanRow) []hookRun {
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

	var runs []hookRun
	for _, unit := range order {
		var cur *hookRun
		var lastEnd time.Time // End of the newest span folded into cur
		synthetic := false    // cur was inferred without a run-hook marker

		closeRun := func() {
			if cur == nil {
				return
			}
			if cur.endTs.IsZero() {
				if synthetic && !lastEnd.IsZero() {
					cur.endTs = lastEnd
				} else if synthetic {
					cur.endTs = cur.startTs
				} else {
					cur.open = true
				}
			}
			// Classify every closed bracket, and open brackets too: an open hook
			// that was already retried is a unit stuck in error state *now*
			// (failErrored), distinct from an open hook that is simply still
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
						// not a new hook: keep the current run. The uniter walks
						// opstep pending→done on success; a *second* `pending`
						// (with no intervening `continue`) is the retry loop it
						// enters when the hook errored — the high-confidence
						// "unit went to error state" signal (M10).
						if cur != nil && cur.kind == hm.Kind {
							if hm.Opstep == "pending" {
								cur.retries++
							}
							continue
						}
						if cur != nil { // a different hook opened with no continue: interrupted
							cur.superseded = true
							cur.endTs = sp.Start // the next hook's start bounds this one
							closeRun()
						}
						cur = &hookRun{
							unit: unit, app: appOf(unit), kind: hm.Kind,
							remoteApp: hm.RemoteApp, storageID: hm.StorageID,
							startTs: sp.Start, repSpan: sp.SpanID,
						}
						cur.spanIDs = append(cur.spanIDs, sp.SpanID)
						continue
					default: // "continue" (or another op) ends the current hook
						if cur != nil {
							cur.endTs = sp.Start
							closeRun()
						}
						continue
					}
				}
			}
			// A non-marker span. Start a synthetic run when it belongs to a hook we
			// never saw open, or when the labelled hook changed under us.
			if sp.Hook != "" && (cur == nil || sp.Hook != cur.kind) {
				closeRun()
				cur = &hookRun{unit: unit, app: appOf(unit), kind: sp.Hook, startTs: sp.Start, repSpan: sp.SpanID}
				synthetic = true
			}
			if cur != nil {
				lastEnd = sp.End
				cur.spanIDs = append(cur.spanIDs, sp.SpanID)
				if sp.StatusCode == "ERROR" {
					cur.rpcErr = true
				}
				if sp.Attrs["method"] == "CommitHookChanges" {
					cur.repSpan = sp.SpanID // prefer the commit for the inspector/databag
				}
			}
		}
		closeRun()
	}
	return runs
}

// statusMapFromSnapshots turns the index's span→status-snapshot maps (unit and
// application) into the span→hookStatus map buildEventsWithStatus consumes. The
// snapshot body is the {value,message,since} JSON the extractor wrote (M10).
func statusMapFromSnapshots(unit, app map[string]index.SnapshotRow) map[string]hookStatus {
	out := make(map[string]hookStatus, len(unit)+len(app))
	add := func(m map[string]index.SnapshotRow, isApp bool) {
		for span, r := range m {
			var body snapBody
			if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
				continue
			}
			out[span] = hookStatus{app: isApp, value: body.Value, message: body.Message}
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
func hookDisplayName(r hookRun, resolver map[string]map[string]string) string {
	switch {
	case isRelationHook(r.kind):
		if ep := resolver[r.app][r.remoteApp]; ep != "" {
			return ep + "-" + r.kind
		}
	case isStorageHook(r.kind):
		if name := storageName(r.storageID); name != "" {
			return name + "-" + r.kind
		}
	}
	return r.kind
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
// own row (docs/event-timeline.md §5).
func rawTransition(sp recording.SpanRow) (event, bool) {
	if sp.Hook != "" {
		return event{}, false
	}
	facade := sp.Attrs["facade"]
	method := sp.Attrs["method"]
	base := event{
		ts: sp.Start, unit: sp.Unit, app: appOf(sp.Unit), spanID: sp.SpanID,
		verboseOnly: true,
	}
	if sp.StatusCode == "ERROR" {
		base.fail = failRPCWarn
	}
	switch {
	case method == "SetApplicationStatus":
		return statusEvent(base, sp, true), true
	case method == "SetStatus" || method == "SetUnitStatus":
		return statusEvent(base, sp, false), true
	case method == "EnterScope":
		base.kind, base.summary = evRelation, "EnterScope"
		return base, true
	case method == "LeaveScope":
		base.kind, base.summary = evRelation, "LeaveScope"
		return base, true
	case strings.Contains(facade, "Leadership") || method == "ClaimLeadership":
		base.kind, base.summary = evLeader, "ClaimLeadership"
		return base, true
	case strings.HasPrefix(facade, "Secrets"):
		base.kind = evSecret
		base.summary = "secret-manage: " + strings.ToLower(strings.TrimPrefix(method, "Secret"))
		return base, true
	case facade == "Action" || strings.Contains(method, "Action"):
		base.kind, base.summary = evAction, "action"
		return base, true
	}
	return event{}, false
}

// statusEvent fills a status-change event, parsing the new value/message from
// the setter's params. app marks an application status (vs a per-unit status).
func statusEvent(base event, sp recording.SpanRow, app bool) event {
	base.kind = evStatus
	val, msg := statusFromParams(sp.Attrs["params"])
	base.summary = "→ " + val
	if app {
		base.summary = "app → " + val
	}
	base.detail = msg
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

// glyph returns the one-rune marker shown next to an event. Hooks carry none —
// the name and duration speak for themselves; raw transitions get a dim mid-dot
// so they read as subordinate. Colour (not a distinct glyph) carries failure,
// keeping the alphabet deliberately small.
func (e event) glyph() string {
	if e.verboseOnly {
		return "·"
	}
	return " "
}

// isErrored reports whether the hook is in error state now (retried and never
// completed): the only failure worth colouring the glyph red on the timeline. A
// recovered "retried" hook is deliberately not red (M10).
func (e event) isErrored() bool { return e.fail == failErrored }

// failLabel is the trailing word describing how a hook run went wrong, or "" for
// a clean run. Each state reads differently so the user can tell a unit stuck in
// error now from one that recovered after a retry, from a truncated recording,
// from a caught RPC error (M10).
func (e event) failLabel() string {
	switch e.fail {
	case failErrored:
		return "error"
	case failRetried:
		return "retried"
	case failInterrupted:
		return "interrupted"
	case failRPCWarn:
		return "rpc error"
	default:
		return ""
	}
}

// statusSummary renders the status changes a hook produced as a compact suffix,
// e.g. `→ blocked` (M10). Each part is coloured by its own status so a hook that
// steps through several (e.g. maintenance → active) does not paint the earlier
// ones with the last one's colour. The status message is intentionally omitted
// here to keep the timeline compact — the full message is in the inspector; the
// Status pane also shows the current one. When verbose is false, statuses that
// merely repeat the scope's current value+message are dropped, so a charm
// re-asserting "active" every hook does not clutter the default view. Empty when
// the hook set no (visible) status.
func (e event) statusSummary(verbose bool) string {
	if len(e.statuses) == 0 {
		return ""
	}
	parts := make([]string, 0, len(e.statuses))
	for _, s := range e.statuses {
		if s.redundant && !verbose {
			continue
		}
		p := "→ " + s.value
		if s.app {
			p = "app → " + s.value
		}
		parts = append(parts, statusStyleFor(s.value).Render(p))
	}
	return strings.Join(parts, "  ")
}
