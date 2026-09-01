package export

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Part names one artifact WriteFolder can produce. Parts is deliberately
// orthogonal to Options' scoping (--unit/--since/--until/--errors-only):
// scoping decides what content is in scope, Parts decides which files that
// content gets written into. Skipping a part is an optimisation for a huge
// recording (less to write, less disk); it is not how you keep an agent's
// context small — a details/ file nobody reads costs nothing to have on
// disk, since Read only pays for what's actually opened.
type Part string

const (
	PartSummary  Part = "summary"  // SUMMARY.md
	PartTimeline Part = "timeline" // timeline.jsonl
	PartEvents   Part = "events"   // events.jsonl
	PartDetails  Part = "details"  // details/<span_id>.json
	PartReport   Part = "report"   // report.md
)

// AllParts is every part WriteFolder knows how to produce, in the order a
// reader would naturally reach for them.
var AllParts = []Part{PartSummary, PartTimeline, PartEvents, PartDetails, PartReport}

// LineRecord is one line of timeline.jsonl or events.jsonl: a flat,
// grep-friendly projection of an Item. It deliberately omits everything that
// only lives inside a hook's own bracket — statuses set, databag/config
// diffs, the failure explanation — so a reader can `grep -C` this file
// cheaply; that detail lives in details/<span_id>.json instead, addressed by
// SpanID. Fields are shared between event and log lines (Type tells them
// apart) rather than split into two record types, so timeline.jsonl can
// interleave both with one schema.
type LineRecord struct {
	TS         time.Time `json:"ts"`
	Type       string    `json:"type"` // "event" | "log"
	Unit       string    `json:"unit,omitempty"`
	App        string    `json:"app,omitempty"`
	Entity     string    `json:"entity,omitempty"`      // log only: non-unit source (machine-0, a pod name)
	Kind       string    `json:"kind,omitempty"`        // event only: hook/status/relation/leader/action/secret/settle
	Summary    string    `json:"summary,omitempty"`     // event only
	Fail       string    `json:"fail,omitempty"`        // event only
	DurationMS int64     `json:"duration_ms,omitempty"` // event only
	Source     string    `json:"source,omitempty"`      // log only
	Level      string    `json:"level,omitempty"`       // log only
	Body       string    `json:"body,omitempty"`        // log only
	SpanID     string    `json:"span_id,omitempty"`
}

// WriteFolder renders r as a directory of agent-oriented artifacts under
// dir, instead of one monolithic document: a short SUMMARY.md for
// orientation, a merged timeline.jsonl and an events-only events.jsonl for
// grepping, one details/<span_id>.json per event for full drill-down,
// report.md for a human reading top to bottom, and an AGENTS.md legend
// naming whichever of those parts is actually present. An agent is meant to
// start from SUMMARY.md/AGENTS.md, grep the .jsonl files for whatever the
// failing test told it, and read individual details/ files only for the
// spans that turn out to matter — instead of paying for the whole
// recording's narrative up front the way one exported document does.
//
// parts selects which files get written; nil or empty means AllParts.
func WriteFolder(dir string, r *Result, parts []Part) error {
	if len(parts) == 0 {
		parts = AllParts
	}
	want := make(map[Part]bool, len(parts))
	for _, p := range parts {
		want[p] = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating export dir %s: %w", dir, err)
	}
	if want[PartSummary] {
		if err := writeSummaryMD(dir, r); err != nil {
			return err
		}
	}
	if want[PartTimeline] {
		if err := writeJSONL(filepath.Join(dir, "timeline.jsonl"), r.Items, false); err != nil {
			return err
		}
	}
	if want[PartEvents] {
		if err := writeJSONL(filepath.Join(dir, "events.jsonl"), r.Items, true); err != nil {
			return err
		}
	}
	if want[PartDetails] {
		if err := writeDetails(dir, r); err != nil {
			return err
		}
	}
	if want[PartReport] {
		if err := writeReportMD(dir, r); err != nil {
			return err
		}
	}
	// Written last since its content depends on which of the above actually
	// ran — it must never point at a file that isn't there.
	return writeAgentsMD(dir, want)
}

func lineRecordForEvent(ev *EventDetail) LineRecord {
	return LineRecord{
		TS: ev.TS, Type: "event", Unit: ev.Unit, App: ev.App, Kind: ev.Kind,
		Summary: ev.Summary, Fail: ev.Fail, DurationMS: ev.DurationMS, SpanID: ev.SpanID,
	}
}

func lineRecordForLog(lg *LogDetail) LineRecord {
	return LineRecord{
		TS: lg.TS, Type: "log", Unit: lg.Unit, App: appOf(lg.Unit), Entity: lg.Entity,
		Source: lg.Source, Level: lg.Level, Body: lg.Body, SpanID: lg.SpanID,
	}
}

// writeJSONL writes one flat LineRecord per item, one JSON object per line.
// eventsOnly skips log items — that's events.jsonl; timeline.jsonl passes
// false and keeps both, merged in the same chronological order Items
// already carries.
func writeJSONL(path string, items []Item, eventsOnly bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		var rec LineRecord
		switch it.Kind {
		case "event":
			rec = lineRecordForEvent(it.Event)
		case "log":
			if eventsOnly {
				continue
			}
			rec = lineRecordForLog(it.Log)
		default:
			continue
		}
		if err := enc.Encode(rec); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

// writeDetails writes one details/<span_id>.json per event — the full
// EventDetail (statuses, databag/config diffs, failure explanation)
// LineRecord leaves out, addressable by the span_id every LineRecord and
// details file shares.
func writeDetails(dir string, r *Result) error {
	detailsDir := filepath.Join(dir, "details")
	if err := os.MkdirAll(detailsDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", detailsDir, err)
	}
	for _, it := range r.Items {
		if it.Kind != "event" || it.Event.SpanID == "" {
			continue
		}
		data, err := json.MarshalIndent(it.Event, "", "  ")
		if err != nil {
			return err
		}
		path := filepath.Join(detailsDir, it.Event.SpanID+".json")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

func writeReportMD(dir string, r *Result) error {
	f, err := os.Create(filepath.Join(dir, "report.md"))
	if err != nil {
		return err
	}
	defer f.Close()
	return WriteMarkdown(f, r)
}

// folderSummary is the terrain SUMMARY.md describes: not a verdict, since a
// hook can run cleanly and still do the wrong thing (docs on why
// --errors-only alone can't be trusted as a completeness signal).
type folderSummary struct {
	failCounts   map[string]int // every FailState.String() value, including "none"
	unitEvents   map[string]int
	unitNotClean map[string]int // events whose Fail != "none", per unit
	neverSettled []string       // units with events in scope but no EvSettle marker
	slowest      []EventDetail  // up to 5, by DurationMS descending, DurationMS > 0 only
}

func summarizeFolder(r *Result) folderSummary {
	fs := folderSummary{failCounts: map[string]int{}, unitEvents: map[string]int{}, unitNotClean: map[string]int{}}
	seenUnit := map[string]bool{}
	settled := map[string]bool{}
	var events []EventDetail
	for _, it := range r.Items {
		if it.Kind != "event" {
			continue
		}
		ev := *it.Event
		events = append(events, ev)
		fs.failCounts[ev.Fail]++
		if ev.Unit == "" {
			continue
		}
		seenUnit[ev.Unit] = true
		fs.unitEvents[ev.Unit]++
		if ev.Fail != "none" {
			fs.unitNotClean[ev.Unit]++
		}
		if ev.Kind == "settle" {
			settled[ev.Unit] = true
		}
	}
	for u := range seenUnit {
		if !settled[u] {
			fs.neverSettled = append(fs.neverSettled, u)
		}
	}
	sort.Strings(fs.neverSettled)

	sort.SliceStable(events, func(i, j int) bool { return events[i].DurationMS > events[j].DurationMS })
	for _, ev := range events {
		if ev.DurationMS <= 0 {
			break // DurationMS descending: everything after this is 0 too
		}
		if len(fs.slowest) == 5 {
			break
		}
		fs.slowest = append(fs.slowest, ev)
	}
	return fs
}

// failStateOrder is every FailState.String() value, in the order they read
// most-to-least alarming, so SUMMARY.md's breakdown isn't shuffled by map
// iteration.
var failStateOrder = []string{"errored", "retried", "rpc-warn", "interrupted", "lost-continue", "none"}

func writeSummaryMD(dir string, r *Result) error {
	fs := summarizeFolder(r)
	b := &strings.Builder{}

	fmt.Fprintf(b, "# Recording shape\n\n")
	fmt.Fprintf(b, "Orientation, not a diagnosis. A hook can run cleanly and still do the "+
		"wrong thing — a bad databag value, a relation that never forms, a status that "+
		"never changes — and none of that shows up as a fail state below. Cross-reference "+
		"against what the test actually expected.\n\n")

	if !r.Recording.Started.IsZero() {
		ended := "(in progress)"
		if !r.Recording.Ended.IsZero() {
			ended = fmtTS(r.Recording.Ended)
		}
		fmt.Fprintf(b, "- captured: %s → %s\n", fmtTS(r.Recording.Started), ended)
	}
	if !r.Summary.Start.IsZero() {
		fmt.Fprintf(b, "- events span: %s → %s\n", fmtTS(r.Summary.Start), fmtTS(r.Summary.End))
	}
	fmt.Fprintf(b, "- %d events, %d log lines\n", r.Summary.TotalEvents, r.Summary.TotalLogs)
	if len(r.Summary.UnitsInScope) > 0 {
		fmt.Fprintf(b, "- units in scope: %s\n", strings.Join(r.Summary.UnitsInScope, ", "))
	}
	b.WriteByte('\n')

	fmt.Fprintf(b, "## Hook outcomes\n\n")
	for _, k := range failStateOrder {
		if n := fs.failCounts[k]; n > 0 {
			fmt.Fprintf(b, "- %s: %d\n", k, n)
		}
	}
	b.WriteByte('\n')

	if len(fs.unitEvents) > 0 {
		fmt.Fprintf(b, "## Per unit\n\n")
		units := make([]string, 0, len(fs.unitEvents))
		for u := range fs.unitEvents {
			units = append(units, u)
		}
		sort.Strings(units)
		for _, u := range units {
			fmt.Fprintf(b, "- %s: %d events, %d not clean\n", u, fs.unitEvents[u], fs.unitNotClean[u])
		}
		b.WriteByte('\n')
	}

	if len(fs.neverSettled) > 0 {
		fmt.Fprintf(b, "## Never reached idle\n\n")
		fmt.Fprintf(b, "Units with events in scope but no settle marker — the agent never "+
			"stayed idle long enough to register as settled, or the recording ends mid-hook:\n")
		for _, u := range fs.neverSettled {
			fmt.Fprintf(b, "- %s\n", u)
		}
		b.WriteByte('\n')
	}

	if len(fs.slowest) > 0 {
		fmt.Fprintf(b, "## Slowest events\n\n")
		for _, ev := range fs.slowest {
			fmt.Fprintf(b, "- %s — %s — %s (%dms) [span %s]\n",
				fmtTS(ev.TS), orDash(ev.Unit), ev.Summary, ev.DurationMS, ev.SpanID)
		}
		b.WriteByte('\n')
	}

	return os.WriteFile(filepath.Join(dir, "SUMMARY.md"), []byte(b.String()), 0o644)
}

// writeAgentsMD is a legend, not a procedure: it names what each present
// file holds and where to find a field, not how to investigate. want is
// consulted so it never references a file WriteFolder didn't actually write.
func writeAgentsMD(dir string, want map[Part]bool) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "# juju-lens export\n\n")
	if want[PartSummary] {
		fmt.Fprintf(b, "- SUMMARY.md — shape of this recording: time range, units, hook-outcome "+
			"counts. Orientation, not a diagnosis.\n")
	}
	if want[PartTimeline] {
		fmt.Fprintf(b, "- timeline.jsonl — events and logs merged, chronological, one JSON object "+
			"per line. `grep -C` around a line of interest: adjacency here means adjacency in "+
			"time, across both kinds.\n")
		fmt.Fprintf(b, "  Fields: ts, type (event|log), unit, app, kind, summary, fail, "+
			"duration_ms, source, level, body, span_id.\n")
	}
	if want[PartEvents] {
		fmt.Fprintf(b, "- events.jsonl — the same event lines as timeline.jsonl, without the "+
			"(usually far more numerous) log lines.\n")
	}
	if want[PartDetails] {
		fmt.Fprintf(b, "- details/<span_id>.json — full detail for one event: statuses set, "+
			"databag/config diffs, failure explanation. span_id comes from any line above.\n")
	}
	if want[PartReport] {
		fmt.Fprintf(b, "- report.md — the same data as one narrated document, for reading top "+
			"to bottom or pasting elsewhere.\n")
	}
	b.WriteByte('\n')
	fmt.Fprintf(b, "`fail` only reflects hook-execution failures (errored/retried/rpc-warn/"+
		"interrupted/lost-continue). A hook can run cleanly and still set a wrong value or "+
		"never do what was expected — that will not show up here.\n")
	return os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(b.String()), 0o644)
}
