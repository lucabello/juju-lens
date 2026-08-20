package viewer

import (
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// event is the viewer's local view of narrative.Event. It keeps the same
// field names the TUI code has always used (tui.go, timeline.go, details.go,
// status.go, logstream.go) so the derivation-logic move to internal/narrative
// (M11) didn't require renaming every call site in this package — only the
// derivation itself moved. See internal/narrative/events.go for the actual
// hook/status/failure logic; this file is now just the viewer-shaped mirror
// of it, plus the bits (glyph, colouring) that are genuinely TUI concerns.
type event struct {
	ts             time.Time
	kind           narrative.EventKind
	unit           string
	app            string
	summary        string
	detail         string
	spanID         string
	fail           narrative.FailState
	statuses       []narrative.HookStatus
	hasDatabag     bool
	dur            time.Duration
	running        bool
	verboseOnly    bool
	settleWorkload string
	cause          string // hook that produced this promoted status marker (EvStatus, M12)
}

// Aliased so the rest of the package can keep writing evHook, failErrored,
// etc. exactly as before.
const (
	evHook     = narrative.EvHook
	evStatus   = narrative.EvStatus
	evRelation = narrative.EvRelation
	evLeader   = narrative.EvLeader
	evAction   = narrative.EvAction
	evSecret   = narrative.EvSecret
	evSettle   = narrative.EvSettle

	failNone         = narrative.FailNone
	failErrored      = narrative.FailErrored
	failRetried      = narrative.FailRetried
	failInterrupted  = narrative.FailInterrupted
	failLostContinue = narrative.FailLostContinue
	failRPCWarn      = narrative.FailRPCWarn
)

// fromNarrative copies a narrative.Event into the viewer's local shape.
func fromNarrative(e narrative.Event) event {
	return event{
		ts: e.TS, kind: e.Kind, unit: e.Unit, app: e.App, summary: e.Summary,
		detail: e.Detail, spanID: e.SpanID, fail: e.Fail, statuses: e.Statuses,
		hasDatabag: e.HasDatabag, dur: e.Dur, running: e.Running,
		verboseOnly: e.VerboseOnly, settleWorkload: e.SettleWorkload, cause: e.Cause,
	}
}

func fromNarrativeSlice(in []narrative.Event) []event {
	out := make([]event, len(in))
	for i, e := range in {
		out[i] = fromNarrative(e)
	}
	return out
}

// buildEvents distils a span list into the timeline's events. See
// narrative.BuildEvents for the derivation itself.
func buildEvents(spans []recording.SpanRow, databagChanged map[string]bool) []event {
	return fromNarrativeSlice(narrative.BuildEvents(spans, databagChanged))
}

// buildEventsWithStatus is buildEvents with status attribution (M10). See
// narrative.BuildEventsWithStatus.
func buildEventsWithStatus(spans []recording.SpanRow, databagChanged map[string]bool, statusBySpan map[string]narrative.HookStatus) []event {
	return fromNarrativeSlice(narrative.BuildEventsWithStatus(spans, databagChanged, statusBySpan))
}

// statusMapFromSnapshots turns the index's span→status-snapshot maps into the
// span→HookStatus map buildEventsWithStatus consumes. See
// narrative.StatusMapFromSnapshots.
func statusMapFromSnapshots(unit, app map[string]index.SnapshotRow) map[string]narrative.HookStatus {
	return narrative.StatusMapFromSnapshots(unit, app)
}

// ident is a stable key for one event, used to keep the selection pinned
// across a verbose-filter re-slice. Each event has a distinct representative
// span.
func (e event) ident() string { return e.spanID }

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
// error now from one that recovered after a retry, from a hook genuinely
// abandoned mid-run, from one that finished but lost its own closing marker
// (almost always a dropped span, not a charm or uniter event), from a caught
// RPC error (M10, M13).
func (e event) failLabel() string {
	switch e.fail {
	case failErrored:
		return "error"
	case failRetried:
		return "retried"
	case failInterrupted:
		return "interrupted"
	case failLostContinue:
		return "capture gap"
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
		if s.Redundant && !verbose {
			continue
		}
		p := "→ " + s.Value
		if s.App {
			p = "app → " + s.Value
		}
		parts = append(parts, statusStyleFor(s.Value).Render(p))
	}
	return strings.Join(parts, "  ")
}
