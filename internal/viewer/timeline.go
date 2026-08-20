package viewer

import (
	"fmt"
	"strings"
	"time"
)

// renderEventsPane draws the Events pane (top-left): the timeline of derived
// events, the navigator that drives every other pane.
func (m *model) renderEventsPane() string {
	w, _ := topRowWidths(m.width)
	h, _ := rowHeights(m.bodyHeight())
	bodyH := max(1, h-2)

	var rows []paneRow
	if len(m.events) == 0 {
		rows = append(rows, row(styleDim.Render("(no events yet)")))
	}

	unitW := m.unitColWidth()
	start := 0
	if m.cursor >= bodyH {
		start = m.cursor - bodyH + 1
	}
	end := min(start+bodyH, len(m.events))
	for i := start; i < end; i++ {
		rows = append(rows, m.eventRow(i, unitW))
	}
	title := "Events"
	if m.eventQuery != "" {
		title += " · /" + m.eventQuery
	}
	return m.renderPane(paneEvents, w, h, title, rows)
}

// unitColWidth sizes the unit column to the longest unit name in view so names
// like "alertmanager/0" are never clipped, capped so a stray long name can't
// swallow the pane (the line still scrolls horizontally past the cap).
func (m *model) unitColWidth() int {
	const floor, ceil = 8, 24
	w := floor
	for _, ev := range m.events {
		u := ev.unit
		if u == "" {
			u = "·"
		}
		if n := len([]rune(u)); n > w {
			w = n
		}
	}
	return min(w, ceil)
}

// eventRow formats one event line: time · unit · glyph summary. A hook's end
// line trails its duration and, when its commit moved a databag (M7), a
// "(databag changes)" tag; verbose-only raw transitions read dimmed and
// indented beneath the hooks.
func (m *model) eventRow(i, unitW int) paneRow {
	ev := m.events[i]
	unit := ev.unit
	if unit == "" {
		unit = "·"
	}
	unit = pad(truncate(unit, unitW), unitW)
	g := ev.glyph()
	label := ev.summary
	if ev.verboseOnly {
		label = "  " + label // indent raw transitions under their hook
	}
	suffix := m.eventSuffix(ev)

	if i == m.cursor {
		// Selected: plain text so the reverse-video bar reads clean.
		return selRow(fmt.Sprintf("%s %s %s %s%s",
			ev.ts.UTC().Format("15:04:05"), unit, g, label, suffix))
	}
	gStyle, labelStyle := styleDim, styleText
	switch {
	case ev.isErrored():
		gStyle = styleErr
	case ev.kind == evSettle:
		labelStyle = statusStyleFor(ev.settleWorkload)
	case ev.verboseOnly:
		labelStyle = styleDim
	case ev.kind == evStatus && len(ev.statuses) > 0:
		// A promoted status-change marker (M12): colour its own "→ value"
		// label the same way statusSummary colours it inline on a hook row.
		labelStyle = statusStyleFor(ev.statuses[0].Value)
	}
	return row(fmt.Sprintf("%s %s %s %s%s",
		styleDim.Render(ev.ts.UTC().Format("15:04:05")),
		styleUnit.Render(unit),
		gStyle.Render(g),
		labelStyle.Render(label), suffix))
}

// pad right-pads s with spaces to n visible runes (no truncation; callers
// truncate first when needed).
func pad(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// eventSuffix is the trailing detail on a row. A hook row trails the failure
// state ("error"/"retried"/"rpc error" always; "interrupted"/"capture gap"
// only in verbose mode, since neither says the charm itself is broken — see
// the M14 comments on those two cases below), "(running)" for a hook still
// open at the tail of a live recording, or its run duration, plus a
// "(databag changes)" tag when its commit changed a databag. A promoted
// status-change marker (EvStatus, M12) instead trails which hook produced it,
// e.g. "→ active  (config-changed)" — the status itself is already the row's
// label, not repeated here (statusSummary is for a hook row's own Statuses,
// which a marker never carries alongside another status). Raw transitions
// carry no suffix at all, keeping the default view a clean time·unit·hook grid.
func (m *model) eventSuffix(ev event) string {
	if ev.verboseOnly {
		return ""
	}
	var b strings.Builder
	switch {
	case ev.fail == failErrored:
		b.WriteString("  " + styleErr.Render(ev.failLabel()))
	case ev.fail == failRetried:
		b.WriteString("  " + styleDim.Render(ev.failLabel()))
	case ev.fail == failInterrupted && m.verbose:
		// Like failLostContinue below, this is deliberately verbose-only
		// (M14): it usually means a truncated recording, an agent restart, or
		// the unit being torn down mid-hook — none of which is "the charm
		// broke". A hook flagged this way still ran; flagging it by default
		// reads as a deployment problem more often than it is one. Falls
		// through to the plain duration below when not verbose.
		b.WriteString("  " + styleDim.Render(ev.failLabel()))
	case ev.fail == failLostContinue && m.verbose:
		// Unlike the other fail states, this one says nothing about the charm
		// or the uniter — it means the recording itself lost a marker. Flagging
		// it in the default view would read as "something's wrong with your
		// deployment" when nothing is; it only earns a mention once the user
		// has opted into capture-level detail. Falls through to the plain
		// duration below when not verbose.
		b.WriteString("  " + styleWarn.Render(ev.failLabel()))
	case ev.fail == failRPCWarn:
		b.WriteString("  " + styleWarn.Render(ev.failLabel()))
	case ev.running:
		b.WriteString("  " + styleDim.Render("(running)"))
	case ev.dur > 0:
		b.WriteString("  " + styleDim.Render("("+formatDur(ev.dur)+")"))
	}
	if ev.kind != evStatus {
		if s := ev.statusSummary(m.verbose); s != "" {
			b.WriteString("  " + s)
		}
	}
	if ev.hasDatabag {
		b.WriteString("  " + styleHook.Render("(databag changes)"))
	}
	if ev.cause != "" {
		b.WriteString("  " + styleDim.Render("("+ev.cause+")"))
	}
	return b.String()
}

// formatDur renders a hook duration compactly: milliseconds under a second,
// else seconds with one decimal.
func formatDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len([]rune(s)) <= n {
		return s
	}
	rs := []rune(s)
	if n <= 1 {
		return string(rs[:n])
	}
	return string(rs[:n-1]) + "…"
}
