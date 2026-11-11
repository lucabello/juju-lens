// Package query reads a recording's index for the inspect commands: its
// events, what each cost, its stack samples, memory, trace and logs. It sits
// on internal/narrative (the derivation) and internal/index (the SQLite query
// layer); it never imports internal/viewer, so it carries none of the TUI's
// rendering concerns.
package query

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// Options narrows the events a Source lists.
type Options struct {
	Units []string  // each may be a unit ("grafana/0") or an application ("grafana") name; empty = all
	Since time.Time // events from this time on
	Until time.Time // events up to this time
}

// RecordingInfo is what the manifest says of a recording.
type RecordingInfo struct {
	Dir        string                 `json:"dir"`
	Controller string                 `json:"controller"`
	AttachMode string                 `json:"attach_mode,omitempty"` // what the recording captured: unit, host, host,unit or none
	Exported   *recording.ExportScope `json:"exported,omitempty"`    // set when the recording is a part of another, made by export
	Started    time.Time              `json:"started"`
	Ended      time.Time              `json:"ended,omitempty"`
}

// StatusDetail is one status a hook set, ready for display: Who is "unit" or
// "app".
type StatusDetail struct {
	Who       string `json:"who"`
	Value     string `json:"value"`
	Message   string `json:"message,omitempty"`
	Redundant bool   `json:"redundant,omitempty"`
}

// DatabagEntryDetail is one side (app, or one unit) of a relation's databag
// as of an event, with its diff against the value just before the event when
// it actually changed.
type DatabagEntryDetail struct {
	Label string               `json:"label"`
	Prev  any                  `json:"prev,omitempty"`
	Cur   any                  `json:"cur,omitempty"`
	Diff  []narrative.DiffLine `json:"diff,omitempty"` // nil when Prev == Cur
}

// DatabagDetail is everything the local application held on one relation at
// an event.
type DatabagDetail struct {
	Relation string               `json:"relation"`
	Entries  []DatabagEntryDetail `json:"entries"`
}

// ConfigDetail is the charm config a config-changed hook read, and its diff
// against the config in effect just before the hook.
type ConfigDetail struct {
	Prev any                  `json:"prev,omitempty"`
	Cur  any                  `json:"cur,omitempty"`
	Diff []narrative.DiffLine `json:"diff,omitempty"` // nil when Prev == Cur
}

// EventDetail is one narrative.Event enriched with everything the TUI's
// inspector overlay would show for it: why it failed (if it did), the
// statuses it set, the databag/config it changed, and what the charm did
// in each process that ran the hook.
type EventDetail struct {
	TS              time.Time              `json:"ts"`
	Kind            string                 `json:"kind"`
	Unit            string                 `json:"unit,omitempty"`
	App             string                 `json:"app,omitempty"`
	Summary         string                 `json:"summary"`
	Detail          string                 `json:"detail,omitempty"`
	Fail            string                 `json:"fail"` // narrative.FailState.String(); "none" when clean
	FailExplanation string                 `json:"fail_explanation,omitempty"`
	Statuses        []StatusDetail         `json:"statuses,omitempty"`
	DurationMS      int64                  `json:"duration_ms,omitempty"`
	Running         bool                   `json:"running,omitempty"`
	Tap             string                 `json:"tap,omitempty"`        // "missing" or "truncated": the tap didn't record the hook in full
	TapReason       string                 `json:"tap_reason,omitempty"` // why, when known
	Databags        []DatabagDetail        `json:"databags,omitempty"`
	Config          *ConfigDetail          `json:"config,omitempty"`
	Ops             []*narrative.OpsNode   `json:"ops,omitempty"`               // one span tree per process (attempt) that ran the hook, every span included
	Costs           []SpanCost             `json:"costs,omitempty"`             // what charm init and each observer cost, in the order they ran
	TopStacks       []StackDetail          `json:"top_stacks,omitempty"`        // the stacks the hook's processes spent the most wall time in
	TopMemStacks    []StackDetail          `json:"top_memory_stacks,omitempty"` // the stacks that saw the most memory growth
	SpanID          string                 `json:"span_id,omitempty"`
	profs           []recording.OpsProfile // the stack samples of the hook's processes, for the folded stacks
}

// SpanCost is what charm init or one observer cost: its wall time, and the
// CPU and memory narrative.AttachCosts measured.
type SpanCost struct {
	Kind       string  `json:"kind"` // recording.OpsInit or OpsObserver
	Name       string  `json:"name"`
	SpanID     string  `json:"span_id"`
	Attempt    int     `json:"attempt,omitempty"` // which process of the hook, from 1, when it ran more than once
	DurationMS float64 `json:"duration_ms"`
	narrative.OpsCost
}

// StackDetail is one call stack the hook's Python was sampled in, with what
// its samples add up to.
type StackDetail struct {
	SpanID         string   `json:"span_id"`
	Span           string   `json:"span"` // the init, event or observer span it ran in
	Samples        int      `json:"samples"`
	WallMS         float64  `json:"wall_ms"`
	CPUMS          float64  `json:"cpu_ms"`
	MemGrowthBytes int64    `json:"mem_growth_bytes"`
	Share          float64  `json:"share"`  // of the hook's total of the metric the list is ordered by
	Frames         []string `json:"frames"` // "function (file:line)", innermost first
	// digest is what the Markdown shows of Frames: the innermost few, then
	// the charm's own frames among the rest, with gaps marked.
	digest []string
	// charmFrame is the innermost frame of the charm's own code, which says
	// more about what the charm was doing than a socket read at the top.
	charmFrame string
}

// Stack digests show this many innermost frames, then up to this many of
// the charm's own.
const (
	digestInner = 3
	digestCharm = 5
)

// topStacks is how many stacks the export lists per hook and metric.
const topStacks = 5

// resolveModel picks the model an export focuses on: the explicitly named
// one, the only one when there's exactly one, or an error listing the
// available names when the choice is ambiguous. There is no interactive
// picker here — export is meant to be scriptable.
func resolveModel(db *index.DB, name string) (index.Model, error) {
	models, err := db.Models()
	if err != nil {
		return index.Model{}, fmt.Errorf("listing models: %w", err)
	}
	if len(models) == 0 {
		return index.Model{}, fmt.Errorf("recording has no indexed models (run `juju-lens index`?)")
	}
	if name == "" {
		if len(models) == 1 {
			return models[0], nil
		}
		names := make([]string, len(models))
		for i, m := range models {
			names[i] = m.Name
		}
		return index.Model{}, fmt.Errorf("recording has multiple models (%s); pass --model", strings.Join(names, ", "))
	}
	for _, m := range models {
		if m.Name == name {
			return m, nil
		}
	}
	return index.Model{}, fmt.Errorf("no model named %q in this recording", name)
}

// unitSet turns --unit values into a lookup set.
func unitSet(units []string) map[string]bool {
	if len(units) == 0 {
		return nil
	}
	out := make(map[string]bool, len(units))
	for _, u := range units {
		out[u] = true
	}
	return out
}

// scopeMatches mirrors the viewer's scope filter: an empty set matches
// everything; otherwise the app or the unit itself must be in it, so scoping
// to an application covers all of its units for free.
func scopeMatches(units map[string]bool, app, unit string) bool {
	if len(units) == 0 {
		return true
	}
	if app != "" && units[app] {
		return true
	}
	return unit != "" && units[unit]
}

// filterEvents keeps default-view events (hooks + settle markers, not the
// verbose-only raw RPC transitions) that match the unit scope and time
// window, sorted by time.
func filterEvents(events []narrative.Event, units map[string]bool, since, until time.Time) []narrative.Event {
	var out []narrative.Event
	for _, ev := range events {
		if ev.VerboseOnly {
			continue
		}
		if !scopeMatches(units, ev.App, ev.Unit) {
			continue
		}
		if !since.IsZero() && ev.TS.Before(since) {
			continue
		}
		if !until.IsZero() && ev.TS.After(until) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// buildEventDetail enriches ev with the same data the TUI's inspector
// overlay gathers for it: the failure explanation, statuses set, the
// databag/config it changed, and the charm-side span trees.
func buildEventDetail(db *index.DB, modelID int64, ev narrative.Event, sp recording.SpanRow, ops *narrative.OpsIndex) EventDetail {
	d := EventDetail{
		TS: ev.TS, Kind: eventKindLabel(ev.Kind), Unit: ev.Unit, App: ev.App,
		Summary: ev.Summary, Detail: ev.Detail, Fail: ev.Fail.String(),
		DurationMS: ev.Dur.Milliseconds(), Running: ev.Running,
		Tap: ev.Tap, TapReason: ev.TapReason,
		Ops: ops.Trees(ev.OpsSpans), SpanID: ev.SpanID,
	}
	addCosts(db, &d, ops)
	if ev.Fail != narrative.FailNone {
		d.FailExplanation = failExplanation(ev, sp)
	}
	for _, s := range ev.Statuses {
		who := "unit"
		if s.App {
			who = "app"
		}
		d.Statuses = append(d.Statuses, StatusDetail{Who: who, Value: s.Value, Message: s.Message, Redundant: s.Redundant})
	}
	if ev.HasDatabag {
		d.Databags = buildDatabagDetail(db, modelID, sp)
	}
	if narrative.IsConfigChanged(ev) {
		d.Config = buildConfigDetail(db, modelID, ev)
	}
	return d
}

// addCosts attaches what each span of d's trees cost, lists init's and the
// observers', and the hook's heaviest stacks.
func addCosts(db *index.DB, d *EventDetail, ops *narrative.OpsIndex) {
	if len(d.Ops) == 0 {
		return
	}
	profs, _ := db.Profiles(narrative.ProcessRoots(d.Ops))
	d.profs = profs
	narrative.AttachCosts(d.Ops, profs)
	var walk func(n *narrative.OpsNode, attempt int)
	walk = func(n *narrative.OpsNode, attempt int) {
		if n.Cost != nil && (n.Kind == recording.OpsInit || n.Kind == recording.OpsObserver) {
			d.Costs = append(d.Costs, SpanCost{
				Kind: n.Kind, Name: n.Name, SpanID: n.SpanID, Attempt: attempt,
				DurationMS: n.DurationMS, OpsCost: *n.Cost,
			})
		}
		for _, c := range n.Children {
			walk(c, attempt)
		}
	}
	for i, t := range d.Ops {
		attempt := 0
		if len(d.Ops) > 1 {
			attempt = i + 1
		}
		walk(t, attempt)
	}
	p := narrative.NewProfile(profs, nil)
	d.TopStacks = stackDetails(p, narrative.MetricWall, ops)
	d.TopMemStacks = stackDetails(p, narrative.MetricMem, ops)
}

func stackDetails(p *narrative.Profile, m narrative.Metric, ops *narrative.OpsIndex) []StackDetail {
	total := p.Total().Get(m)
	var out []StackDetail
	for _, s := range p.TopStacks(m, topStacks) {
		sd := StackDetail{
			SpanID: s.SpanID, Span: ops.SpanLabel(s.SpanID), Samples: s.Samples,
			WallMS: nsToMS(s.WallNs), CPUMS: nsToMS(s.CPUNs), MemGrowthBytes: s.MemBytes,
		}
		switch m {
		case narrative.MetricMem:
			sd.Share = float64(s.MemBytes) / total
		default:
			sd.Share = float64(s.WallNs) / total
		}
		skipped, charm := 0, 0
		for i, f := range s.Frames {
			fn, loc := p.FrameLabel(f)
			label := fn + " (" + loc + ")"
			sd.Frames = append(sd.Frames, label)
			if sd.charmFrame == "" && p.IsCharmFrame(f) && !narrative.IsWrapperFrame(p.Frames[f]) {
				sd.charmFrame = label
			}
			if i < digestInner || p.IsCharmFrame(f) && !narrative.IsWrapperFrame(p.Frames[f]) && charm < digestCharm {
				if skipped > 0 {
					sd.digest = append(sd.digest, fmt.Sprintf("… %d %s", skipped, plural(skipped, "frame", "frames")))
				}
				skipped = 0
				sd.digest = append(sd.digest, label)
				if i >= digestInner {
					charm++
				}
				continue
			}
			skipped++
		}
		if skipped > 0 {
			sd.digest = append(sd.digest, fmt.Sprintf("… %d outer %s", skipped, plural(skipped, "frame", "frames")))
		}
		out = append(out, sd)
	}
	return out
}

func nsToMS(ns int64) float64 { return float64(ns/1000) / 1000 }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// KindLabel is the name of an event kind in the export: "hook", "action",
// "status", "relation", "leader", "secret", "settle" or "unknown".
func KindLabel(k narrative.EventKind) string { return eventKindLabel(k) }

func eventKindLabel(k narrative.EventKind) string {
	switch k {
	case narrative.EvHook:
		return "hook"
	case narrative.EvStatus:
		return "status"
	case narrative.EvRelation:
		return "relation"
	case narrative.EvLeader:
		return "leader"
	case narrative.EvAction:
		return "action"
	case narrative.EvSecret:
		return "secret"
	case narrative.EvSettle:
		return "settle"
	default:
		return "unknown"
	}
}

// failExplanation is the plain-text equivalent of the TUI inspector's
// renderFailure prose for each failState.
func failExplanation(ev narrative.Event, sp recording.SpanRow) string {
	switch ev.Fail {
	case narrative.FailErrored:
		return "hook in error state — it errored and the uniter is still retrying it; the unit is in error state"
	case narrative.FailRetried:
		return "hook retried — it errored, the uniter retried it, and it then completed successfully"
	case narrative.FailInterrupted:
		return "hook interrupted — another hook opened before this one reached done; it was genuinely abandoned mid-run"
	case narrative.FailLostContinue:
		return "capture gap — this hook actually finished (it reached done), but its own closing marker is missing from the recording; almost certainly a dropped span, not a charm or uniter problem"
	case narrative.FailRPCWarn:
		line := "an RPC in this hook returned an error (the hook still completed, and charm status is unaffected)"
		if sp.StatusMsg != "" {
			line += ": " + sp.StatusMsg
		}
		return line
	default:
		return ""
	}
}

// buildDatabagDetail mirrors the TUI inspector's renderDatabags, gathering
// structured data instead of writing styled text.
func buildDatabagDetail(db *index.DB, modelID int64, sp recording.SpanRow) []DatabagDetail {
	if sp.SpanID == "" {
		return nil
	}
	changed, err := db.SnapshotsByProducingSpan(sp.SpanID, string(index.KindDatabag))
	if err != nil || len(changed) == 0 {
		return nil
	}
	changedScope := map[string]bool{}
	var keys []string
	seenKey := map[string]bool{}
	for _, s := range changed {
		changedScope[s.Scope] = true
		if k, _ := narrative.SplitDatabagScope(s.Scope); k != "" && !seenKey[k] {
			seenKey[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	local := appOf(sp.Unit)
	rows, _ := db.LatestPerScopeAsOf(modelID, string(index.KindDatabag), sp.Start)
	body := make(map[string]string, len(rows))
	for _, r := range rows {
		body[r.Scope] = r.Body
	}

	var out []DatabagDetail
	for _, key := range keys {
		dd := DatabagDetail{Relation: narrative.RelationLabel(key, local)}
		for _, e := range narrative.LocalDatabagEntries(body, key, local) {
			cur := body[e.Scope]
			prev := cur
			if changedScope[e.Scope] {
				prev, _ = db.PrevSnapshotBefore(modelID, e.Scope, sp.Start)
			}
			entry := DatabagEntryDetail{
				Label: e.Label,
				Cur:   narrative.ExpandJSON([]byte(cur)),
				Prev:  narrative.ExpandJSON([]byte(prev)),
			}
			if prev != cur {
				entry.Diff = narrative.LineDiff(
					narrative.SplitLines(narrative.PrettyJSON(prev)),
					narrative.SplitLines(narrative.PrettyJSON(cur)))
			}
			dd.Entries = append(dd.Entries, entry)
		}
		out = append(out, dd)
	}
	return out
}

// buildConfigDetail mirrors the TUI inspector's renderConfig: nil unless the
// config was read during the hook.
func buildConfigDetail(db *index.DB, modelID int64, ev narrative.Event) *ConfigDetail {
	scope := "config:" + ev.App
	curBody, _ := db.LatestSnapshotBetween(modelID, scope, ev.TS, ev.TS.Add(ev.Dur))
	if curBody == "" {
		return nil
	}
	prevBody, _ := db.PrevSnapshotBefore(modelID, scope, ev.TS)
	cd := &ConfigDetail{
		Cur:  narrative.ExpandJSON([]byte(curBody)),
		Prev: narrative.ExpandJSON([]byte(prevBody)),
	}
	if prevBody != curBody {
		cd.Diff = narrative.LineDiff(
			narrative.SplitLines(narrative.PrettyJSON(prevBody)),
			narrative.SplitLines(narrative.PrettyJSON(curBody)))
	}
	return cd
}

// appOf extracts the application name from a unit tag. Deliberately
// duplicated per package, matching the convention internal/narrative,
// internal/viewer and internal/index each already follow.
func appOf(unit string) string {
	if i := strings.IndexByte(unit, '/'); i >= 0 {
		return unit[:i]
	}
	return unit
}
