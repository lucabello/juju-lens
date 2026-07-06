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

	start := 0
	if m.cursor >= bodyH {
		start = m.cursor - bodyH + 1
	}
	end := min(start+bodyH, len(m.events))
	for i := start; i < end; i++ {
		rows = append(rows, m.eventRow(i))
	}
	return m.renderPane(paneEvents, w, h, "Events", rows)
}

// eventRow formats one event line: time · unit · glyph summary. A hook's end
// line trails its duration and, when its commit moved a databag (M7), a ✎db pip;
// verbose-only raw transitions read dimmed and indented beneath the hooks.
func (m *model) eventRow(i int) paneRow {
	ev := m.events[i]
	unit := ev.unit
	if unit == "" {
		unit = "·"
	}
	g := ev.glyph()
	label := ev.summary
	if ev.verboseOnly {
		label = "  " + label // indent raw transitions under their hook
	}
	suffix := m.eventSuffix(ev)

	if i == m.cursor {
		// Selected: plain text so the reverse-video bar reads clean.
		return selRow(fmt.Sprintf("%s %-12s %s %s%s",
			ev.ts.UTC().Format("15:04:05"), truncate(unit, 12), g, label, suffix))
	}
	gStyle, labelStyle := styleDim, styleText
	switch {
	case ev.failed:
		gStyle = styleErr
	case ev.verboseOnly:
		labelStyle = styleDim
	}
	return row(fmt.Sprintf("%s %s %s %s%s",
		styleDim.Render(ev.ts.UTC().Format("15:04:05")),
		styleUnit.Render(fmt.Sprintf("%-12s", truncate(unit, 12))),
		gStyle.Render(g),
		labelStyle.Render(label), suffix))
}

// eventSuffix is the trailing detail on a hook row: "failed", "(running)" for a
// hook still open at the tail of a live recording, or the run duration, plus a
// ✎db pip when the hook changed a databag. Raw transitions carry none, keeping
// the default view a clean time·unit·hook grid.
func (m *model) eventSuffix(ev event) string {
	if ev.verboseOnly {
		return ""
	}
	var b strings.Builder
	switch {
	case ev.failed:
		b.WriteString("  " + styleErr.Render("failed"))
	case ev.running:
		b.WriteString("  " + styleDim.Render("(running)"))
	case ev.dur > 0:
		b.WriteString("  " + styleDim.Render("("+formatDur(ev.dur)+")"))
	}
	if ev.hasDatabag {
		b.WriteString(" " + styleHook.Render("✎db"))
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
