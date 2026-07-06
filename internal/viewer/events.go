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
	unit        string // "grafana/0" ("" for controller-side events)
	app         string // "grafana"
	summary     string // charm-visible hook name ("grafana-source-relation-changed") or transition
	detail      string // optional trailing context (status message)
	spanID      string // representative span (the commit, or the run-hook marker)
	failed      bool   // the hook errored / the producing RPC errored
	hasDatabag  bool   // the hook's commit actually changed a databag (M7)
	dur         time.Duration // hook duration (run-hook → continue)
	running     bool   // hook still open at the tail of a live recording
	verboseOnly bool   // a raw transition: hidden in the default view
}

type eventKind int

const (
	evHook eventKind = iota
	evStatus
	evRelation
	evLeader
	evAction
	evSecret
)

// ident is a stable key for one event, used to keep the selection pinned across
// a verbose-filter re-slice. Each event has a distinct representative span.
func (e event) ident() string { return e.spanID }

// buildEvents distils a span list (sorted by start) into the timeline's events:
// one per hook run, plus the verbose-only raw transitions. databagChanged marks
// the commit spans that actually moved a databag (M7) so the row can carry the
// ✎db pip. The result is sorted by timestamp.
func buildEvents(spans []recording.SpanRow, databagChanged map[string]bool) []event {
	resolver := relationEndpointMap(spans)
	var out []event
	for _, r := range buildHookRuns(spans) {
		ev := event{
			ts: r.startTs, kind: evHook, unit: r.unit, app: r.app,
			summary: hookDisplayName(r, resolver), spanID: r.repSpan,
			failed: r.failed, hasDatabag: databagChanged[r.repSpan],
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
	return out
}

// hookRun is one execution of a hook on a unit: the run-hook..continue bracket
// the uniter writes in its SetState blobs (docs/event-timeline.md §2). repSpan is
// the span the inspector drills into — the CommitHookChanges when the hook
// committed, else the run-hook marker itself. remoteApp/storageID come from the
// run-hook marker and reconstruct the charm-visible hook name.
type hookRun struct {
	unit      string
	app       string
	kind      string // bare kind from the marker, e.g. "relation-changed"
	remoteApp string // relation hooks: the remote application
	storageID string // storage hooks: e.g. "data/0"
	startTs   time.Time
	endTs     time.Time
	repSpan   string
	failed    bool
	open      bool // start marker seen, end not yet (live tail / truncated recording)
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
			runs = append(runs, *cur)
			cur, synthetic, lastEnd = nil, false, time.Time{}
		}

		for _, sp := range byUnit[unit] {
			if sp.Attrs["method"] == "SetState" {
				if hm := index.ParseHookMarker(sp.Attrs["params"]); hm.Op != "" {
					switch {
					case hm.Op == "run-hook" && hm.Kind != "":
						// A run-hook for the hook already open is a later opstep
						// (pending→done), not a new hook: keep the current run.
						if cur != nil && cur.kind == hm.Kind {
							continue
						}
						if cur != nil { // a different hook opened with no continue: interrupted
							cur.failed = true
							closeRun()
						}
						cur = &hookRun{
							unit: unit, app: appOf(unit), kind: hm.Kind,
							remoteApp: hm.RemoteApp, storageID: hm.StorageID,
							startTs: sp.Start, repSpan: sp.SpanID,
						}
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
				if sp.StatusCode == "ERROR" {
					cur.failed = true
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
		failed: sp.StatusCode == "ERROR", verboseOnly: true,
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
