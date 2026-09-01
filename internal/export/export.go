// Package export assembles a recording's derived narrative — hook runs,
// their failure state, the statuses/databags/config they changed, and every
// correlated log line — into a single, merged, time-ordered document meant
// to be handed to a human or an LLM agent debugging an issue offline. It sits
// on top of internal/narrative (the derivation) and internal/index (the
// SQLite query layer); it never imports internal/viewer, so it carries none
// of the TUI's rendering concerns.
package export

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// Options configures what an export includes.
type Options struct {
	Model      string    `json:"model,omitempty"`
	Units      []string  `json:"units,omitempty"` // each may be a unit ("grafana/0") or an application ("grafana") name; empty = all
	Since      time.Time `json:"since,omitempty"`
	Until      time.Time `json:"until,omitempty"`
	ErrorsOnly bool      `json:"errors_only,omitempty"` // keep only failed hook runs, plus one same-unit neighbour on each side for context
}

// contextPad is how far around the surviving events' time span logs are
// still included, so a log line immediately before/after the first/last
// included event isn't dropped at the boundary.
const contextPad = 10 * time.Second

// RecordingInfo is the manifest-derived header of an export.
type RecordingInfo struct {
	Dir        string    `json:"dir"`
	Controller string    `json:"controller"`
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended,omitempty"`
}

// ModelInfo identifies the model an export was built from.
type ModelInfo struct {
	Name string `json:"name"`
	UUID string `json:"uuid,omitempty"`
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
// statuses it set, and the databag/config it changed.
type EventDetail struct {
	TS              time.Time       `json:"ts"`
	Kind            string          `json:"kind"`
	Unit            string          `json:"unit,omitempty"`
	App             string          `json:"app,omitempty"`
	Summary         string          `json:"summary"`
	Detail          string          `json:"detail,omitempty"`
	Fail            string          `json:"fail"` // narrative.FailState.String(); "none" when clean
	FailExplanation string          `json:"fail_explanation,omitempty"`
	Statuses        []StatusDetail  `json:"statuses,omitempty"`
	DurationMS      int64           `json:"duration_ms,omitempty"`
	Running         bool            `json:"running,omitempty"`
	Databags        []DatabagDetail `json:"databags,omitempty"`
	Config          *ConfigDetail   `json:"config,omitempty"`
	SpanID          string          `json:"span_id,omitempty"`
	Context         bool            `json:"context,omitempty"` // included only as a neighbour of a failure under --errors-only, not itself a failure
}

// LogDetail is one merged log line.
type LogDetail struct {
	TS     time.Time `json:"ts"`
	Source string    `json:"source"`
	Unit   string    `json:"unit,omitempty"`
	Entity string    `json:"entity,omitempty"`
	Level  string    `json:"level,omitempty"`
	Module string    `json:"module,omitempty"`
	Body   string    `json:"body"`
	SpanID string    `json:"span_id,omitempty"` // the span this log line correlated to, if any (index.DB.Logs' exact-match join)
}

// Item is one line of the merged, time-ordered export: either an event or a
// log line, never both.
type Item struct {
	TS    time.Time    `json:"ts"`
	Kind  string       `json:"kind"` // "event" | "log"
	Event *EventDetail `json:"event,omitempty"`
	Log   *LogDetail   `json:"log,omitempty"`
}

// Summary is the at-a-glance header both output formats lead with.
type Summary struct {
	TotalEvents   int       `json:"total_events"`
	ErroredEvents int       `json:"errored_events"`
	RetriedEvents int       `json:"retried_events"`
	RPCWarnEvents int       `json:"rpc_warn_events"`
	TotalLogs     int       `json:"total_logs"`
	UnitsInScope  []string  `json:"units_in_scope,omitempty"`
	Start         time.Time `json:"start,omitempty"`
	End           time.Time `json:"end,omitempty"`
}

// Result is the fully assembled, filtered export, format-agnostic.
type Result struct {
	Recording RecordingInfo `json:"recording"`
	Model     ModelInfo     `json:"model"`
	Filters   Options       `json:"filters"`
	Summary   Summary       `json:"summary"`
	Items     []Item        `json:"items"`
}

// Build opens a recording's index (which must already exist — the same
// precondition as `juju-lens view`; export does not index on the fly), derives
// its narrative, applies opts, and assembles a Result. It does not render
// anything.
func Build(dir string, opts Options) (*Result, error) {
	man, err := recording.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("loading manifest at %s: %w", dir, err)
	}
	layout := recording.NewLayout(dir)
	db, err := index.Open(layout.IndexDB())
	if err != nil {
		return nil, fmt.Errorf("opening index (run `juju-lens index %s`): %w", dir, err)
	}
	defer db.Close()

	mm, err := resolveModel(db, opts.Model)
	if err != nil {
		return nil, err
	}

	spans, err := db.Spans(mm.ID)
	if err != nil {
		return nil, fmt.Errorf("loading spans: %w", err)
	}
	spanByID := make(map[string]recording.SpanRow, len(spans))
	for _, sp := range spans {
		spanByID[sp.SpanID] = sp
	}
	databagSpans, _ := db.ProducingSpanIDs(mm.ID, string(index.KindDatabag))
	unitStatusBySpan, _ := db.StatusBySpan(mm.ID, string(index.KindUnitStatus))
	appStatusBySpan, _ := db.StatusBySpan(mm.ID, string(index.KindAppStatus))
	statusBySpan := narrative.StatusMapFromSnapshots(unitStatusBySpan, appStatusBySpan)
	events := narrative.BuildEventsWithStatus(spans, databagSpans, statusBySpan)

	units := unitSet(opts.Units)
	scoped := filterEvents(events, units, opts.Since, opts.Until)
	final, contextOnly := applyErrorsOnly(scoped, opts.ErrorsOnly)

	lo, hi := timeWindow(opts, final)
	logs, err := db.Logs(mm.ID, 0)
	if err != nil {
		return nil, fmt.Errorf("loading logs: %w", err)
	}
	logs = filterLogs(logs, units, lo, hi)

	items := make([]Item, 0, len(final)+len(logs))
	summary := Summary{UnitsInScope: opts.Units}
	for _, ev := range final {
		sp := spanByID[ev.SpanID]
		detail := buildEventDetail(db, mm.ID, ev, sp, contextOnly[ev.SpanID])
		items = append(items, Item{TS: ev.TS, Kind: "event", Event: &detail})
		summary.TotalEvents++
		switch ev.Fail {
		case narrative.FailErrored:
			summary.ErroredEvents++
		case narrative.FailRetried:
			summary.RetriedEvents++
		case narrative.FailRPCWarn:
			summary.RPCWarnEvents++
		}
	}
	for i := range logs {
		lg := logs[i]
		items = append(items, Item{TS: lg.Ts, Kind: "log", Log: &LogDetail{
			TS: lg.Ts, Source: lg.Source, Unit: lg.Unit, Entity: lg.Entity,
			Level: lg.Level, Module: lg.Module, Body: lg.Body, SpanID: lg.SpanID,
		}})
		summary.TotalLogs++
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].TS.Before(items[j].TS) })
	if len(items) > 0 {
		summary.Start, summary.End = items[0].TS, items[len(items)-1].TS
	}

	return &Result{
		Recording: RecordingInfo{
			Dir: dir, Controller: man.Controller.Name,
			Started: man.Started, Ended: man.Ended,
		},
		Model:   ModelInfo{Name: mm.Name, UUID: mm.UUID},
		Filters: opts,
		Summary: summary,
		Items:   items,
	}, nil
}

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

// applyErrorsOnly, when enabled, keeps only hook runs that actually failed
// (errored/retried/rpc-warn — not "interrupted", which usually just means the
// recording was truncated, not that the charm broke) plus one same-unit
// neighbour on each side so a reader sees what led up to and followed the
// failure. It returns the final event list and the set of span ids that were
// pulled in only as context (not themselves failures).
func applyErrorsOnly(scoped []narrative.Event, errorsOnly bool) (final []narrative.Event, contextOnly map[string]bool) {
	if !errorsOnly {
		return scoped, nil
	}
	contextOnly = map[string]bool{}
	keep := map[string]bool{}
	byUnit := map[string][]int{}
	for i, ev := range scoped {
		byUnit[ev.Unit] = append(byUnit[ev.Unit], i)
	}
	isFailure := func(ev narrative.Event) bool {
		switch ev.Fail {
		case narrative.FailErrored, narrative.FailRetried, narrative.FailRPCWarn:
			return true
		}
		return false
	}
	for i, ev := range scoped {
		if !isFailure(ev) {
			continue
		}
		keep[ev.SpanID] = true
		idxs := byUnit[ev.Unit]
		pos := -1
		for p, idx := range idxs {
			if idx == i {
				pos = p
				break
			}
		}
		if pos < 0 {
			continue
		}
		if pos > 0 {
			nb := scoped[idxs[pos-1]]
			if !keep[nb.SpanID] {
				contextOnly[nb.SpanID] = true
			}
			keep[nb.SpanID] = true
		}
		if pos+1 < len(idxs) {
			nb := scoped[idxs[pos+1]]
			if !keep[nb.SpanID] {
				contextOnly[nb.SpanID] = true
			}
			keep[nb.SpanID] = true
		}
	}
	for _, ev := range scoped {
		if keep[ev.SpanID] {
			final = append(final, ev)
		}
	}
	return final, contextOnly
}

// timeWindow is the [lo, hi] range logs are kept within: the explicit
// --since/--until bounds when given, else the surviving events' own span
// padded by contextPad so boundary logs aren't dropped.
func timeWindow(opts Options, events []narrative.Event) (lo, hi time.Time) {
	lo, hi = opts.Since, opts.Until
	if !lo.IsZero() && !hi.IsZero() {
		return lo, hi
	}
	var evLo, evHi time.Time
	for _, ev := range events {
		if evLo.IsZero() || ev.TS.Before(evLo) {
			evLo = ev.TS
		}
		if evHi.IsZero() || ev.TS.After(evHi) {
			evHi = ev.TS
		}
	}
	if lo.IsZero() && !evLo.IsZero() {
		lo = evLo.Add(-contextPad)
	}
	if hi.IsZero() && !evHi.IsZero() {
		hi = evHi.Add(contextPad)
	}
	return lo, hi
}

// filterLogs keeps log lines matching the unit scope (or carrying no
// unit/entity at all) within [lo, hi]. A zero lo/hi bound is unbounded.
func filterLogs(logs []index.LogRow, units map[string]bool, lo, hi time.Time) []index.LogRow {
	out := make([]index.LogRow, 0, len(logs))
	for _, lg := range logs {
		who := lg.Unit
		if who == "" {
			who = lg.Entity
		}
		if !scopeMatches(units, appOf(who), lg.Unit) {
			continue
		}
		if !lo.IsZero() && lg.Ts.Before(lo) {
			continue
		}
		if !hi.IsZero() && lg.Ts.After(hi) {
			continue
		}
		out = append(out, lg)
	}
	return out
}

// buildEventDetail enriches ev with the same data the TUI's inspector
// overlay gathers for it: the failure explanation, statuses set, and the
// databag/config it changed.
func buildEventDetail(db *index.DB, modelID int64, ev narrative.Event, sp recording.SpanRow, contextOnly bool) EventDetail {
	d := EventDetail{
		TS: ev.TS, Kind: eventKindLabel(ev.Kind), Unit: ev.Unit, App: ev.App,
		Summary: ev.Summary, Detail: ev.Detail, Fail: ev.Fail.String(),
		DurationMS: ev.Dur.Milliseconds(), Running: ev.Running,
		SpanID: ev.SpanID, Context: contextOnly,
	}
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

// buildConfigDetail mirrors the TUI inspector's renderConfig.
func buildConfigDetail(db *index.DB, modelID int64, ev narrative.Event) *ConfigDetail {
	scope := "config:" + ev.App
	curBody, _ := db.LatestSnapshotBefore(modelID, scope, ev.TS.Add(ev.Dur))
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
