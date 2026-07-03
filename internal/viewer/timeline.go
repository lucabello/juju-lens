package viewer

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// renderCentreColumn stacks timeline + details inside the middle column.
func (m *model) renderCentreColumn() string {
	_, w, _ := columnWidths(m.width)
	tlH, detH := centreSplit(m.bodyHeight())
	tl := m.renderTimeline(w, tlH)
	det := m.renderDetails(w, detH)
	return lipgloss.JoinVertical(lipgloss.Left, tl, det)
}

func (m *model) renderTimeline(w, h int) string {
	inner := max(1, h-2)
	rows := make([]string, 0, len(m.spans))

	start := 0
	if m.cursor >= inner {
		start = m.cursor - inner + 1
	}
	end := start + inner
	if end > len(m.spans) {
		end = len(m.spans)
	}

	// -4 accounts for borders (2) and horizontal padding (2).
	textWidth := max(10, w-4)
	for i := start; i < end; i++ {
		sp := m.spans[i]
		hook := sp.Hook
		if hook == "" {
			hook = "-"
		}
		unit := sp.Unit
		if unit == "" {
			unit = "?"
		}
		// Reserve fixed widths for the leading time/unit/hook columns
		// and give the span name whatever fits after.
		line := fmt.Sprintf("%s %-14s %s %s",
			formatTime(sp.Start),
			styleUnit.Render(truncate(unit, 14)),
			styleHook.Render(truncate(hook, 22)),
			truncate(sp.Name, max(0, textWidth-52)),
		)
		if i == m.cursor {
			line = styleSelected.Width(textWidth).Render(line)
		}
		rows = append(rows, line)
	}
	for len(rows) < inner {
		rows = append(rows, "")
	}
	return styleTimelineBox.Width(w).Height(h).Render(strings.Join(rows, "\n"))
}

func formatTime(t time.Time) string {
	return t.UTC().Format("15:04:05.000")
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
