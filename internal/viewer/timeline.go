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
	return m.renderPane(paneEvents, w, h, "Events", rows)
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
// line trails its duration and, when its commit moved a databag (M7), a ✎db pip;
// verbose-only raw transitions read dimmed and indented beneath the hooks.
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
	case ev.verboseOnly:
		labelStyle = styleDim
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

// eventSuffix is the trailing detail on a hook row: the failure state
// ("error"/"interrupted"/"rpc error"), "(running)" for a hook still open at the
// tail of a live recording, or the run duration, plus the status the hook drove
// the charm into (M10) and a "(databag changes)" tag when it changed a databag.
// Raw transitions carry none, keeping the default view a clean time·unit·hook
// grid.
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
	case ev.fail == failInterrupted:
		b.WriteString("  " + styleDim.Render(ev.failLabel()))
	case ev.fail == failRPCWarn:
		b.WriteString("  " + styleWarn.Render(ev.failLabel()))
	case ev.running:
		b.WriteString("  " + styleDim.Render("(running)"))
	case ev.dur > 0:
		b.WriteString("  " + styleDim.Render("("+formatDur(ev.dur)+")"))
	}
	if s := ev.statusSummary(); s != "" {
		b.WriteString("  " + statusStyleFor(ev.statuses[len(ev.statuses)-1].value).Render(s))
	}
	if ev.hasDatabag {
		b.WriteString("  " + styleHook.Render("(databag changes)"))
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
