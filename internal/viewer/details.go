package viewer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucabello/juju-lens/internal/recording"
)

// renderDetails renders the currently-selected span's attributes inside a
// bordered viewport. The viewport itself is a member of the model so the
// user can scroll long attribute lists independently.
func (m *model) renderDetails(w, h int) string {
	m.details.Width = max(0, w-4)
	m.details.Height = max(1, h-2)
	box := styleDetailsBox
	// Highlight the border when the details pane holds focus, so it is obvious
	// that navigation keys now scroll it (Tab toggles).
	if m.focus == paneDetails {
		box = box.BorderForeground(lipgloss.Color("4"))
	}
	return box.Width(w).Height(h).Render(m.details.View())
}

func (m *model) refreshDetails() {
	if len(m.spans) == 0 {
		m.details.SetContent("")
		return
	}
	sp := m.currentSpan()
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Render(sp.Name))

	// Compact header: status · duration · start, then unit/model/service. Kept
	// short so the correlated logs below sit near the top of the pane.
	statusStyle := styleOK
	if sp.StatusCode == "ERROR" {
		statusStyle = styleErr
	}
	fmt.Fprintf(&b, "  %s · %s · %s\n", statusStyle.Render(sp.StatusCode), sp.Duration(), sp.Start.Format("15:04:05.000"))
	loc := make([]string, 0, 3)
	if sp.Unit != "" {
		loc = append(loc, "unit "+styleUnit.Render(sp.Unit))
	}
	if sp.Model != "" {
		loc = append(loc, "model "+sp.Model)
	}
	if sp.Service != "" {
		loc = append(loc, "service "+sp.Service)
	}
	if len(loc) > 0 {
		fmt.Fprintf(&b, "  %s\n", strings.Join(loc, " · "))
	}
	if sp.StatusMsg != "" {
		fmt.Fprintf(&b, "  %s\n", styleErr.Render(sp.StatusMsg))
	}

	// Trace context is shown only when the wire envelope actually carried it
	// (real Juju tracing). Otherwise the ids are synthesised per-RPC from
	// (pid, conn, request-id) and would imply a causal chain we don't
	// reconstruct, so we hide them rather than mislead.
	if traceID := sp.Attrs["trace-id"]; traceID != "" {
		fmt.Fprintf(&b, "  trace: %s\n", traceID)
		if sp.ParentSpanID != "" {
			fmt.Fprintf(&b, "  parent: %s\n", sp.ParentSpanID)
		}
	}

	// Correlated logs come before the (often large) attributes blob so they are
	// visible the moment a span is selected, without scrolling past a big
	// params payload.
	m.renderCorrelatedLogs(&b, sp)
	if len(sp.Attrs) > 0 {
		fmt.Fprintf(&b, "\n%s\n", lipgloss.NewStyle().Bold(true).Render("attributes"))
		keys := make([]string, 0, len(sp.Attrs))
		for k := range sp.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s = %s\n", k, sp.Attrs[k])
		}
	}
	m.details.SetContent(b.String())
	m.details.GotoTop()
}

// logWindow is how far on either side of a span's timespan we pull same-unit
// log lines for the fuzzy (unit, time-window) correlation of VISION §7. RPC
// spans are sub-second, so a few seconds of context around them is what makes
// the hook a charm was running at that instant legible.
const logWindow = 5 * time.Second

// renderCorrelatedLogs appends the debug-log lines correlated to sp: any line
// carrying its span-id (exact), plus same-unit lines within logWindow. Lines
// that matched by span-id are marked so the exact join stands out from the
// fuzzy one.
func (m *model) renderCorrelatedLogs(b *strings.Builder, sp recording.SpanRow) {
	if m.db == nil {
		return
	}
	logs, err := m.db.LogsForSpan(m.activeModel.ID, sp.SpanID, sp.Unit, sp.Start, sp.End, logWindow, 100)
	if err != nil || len(logs) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf("logs (%d)", len(logs))))
	for _, lg := range logs {
		marker := " "
		if lg.Matched == "span" {
			marker = styleHook.Render("»")
		}
		ts := styleDim.Render(lg.Ts.Format("15:04:05.000"))
		fmt.Fprintf(b, "  %s %s %s %s\n", marker, ts, logLevelStyle(lg.Level).Render(fmt.Sprintf("%-7s", lg.Level)), lg.Body)
	}
}

// logLevelStyle colours a log level so ERROR/WARNING stand out while routine
// INFO/DEBUG stay quiet.
func logLevelStyle(level string) lipgloss.Style {
	switch level {
	case "ERROR", "CRITICAL":
		return styleErr
	case "WARNING":
		return styleWarn
	case "INFO":
		return styleOK
	default:
		return styleDim
	}
}
