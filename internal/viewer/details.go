package viewer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

// renderOverlayFrame draws the inspector overlay over the body. It is opened
// with `enter` on an event and shows everything behind that event — the databag
// it wrote, the RPCs it collapsed, and the logs that carried its span id.
func (m *model) renderOverlayFrame() string {
	h := m.bodyHeight()
	m.overlay.Width = max(0, m.width-4)
	m.overlay.Height = max(1, h-2)
	box := styleBox.BorderForeground(lipgloss.Color("4"))
	return box.Width(m.width).Height(h).Render(m.overlay.View())
}

// renderOverlay fills the overlay viewport with the selected event's detail.
func (m *model) renderOverlay() {
	ev := m.currentEvent()
	var b strings.Builder

	title := fmt.Sprintf("Inspector · %s", ev.summary)
	fmt.Fprintf(&b, "%s\n", styleApp.Render(title))
	fmt.Fprintf(&b, "%s\n", styleDim.Render("esc close · d diff · ↑/↓ scroll"))

	statusStyle := styleOK
	statusCode := "OK"
	sp, ok := m.spanByID(ev.spanID)
	if ok {
		if sp.StatusCode == "ERROR" {
			statusStyle, statusCode = styleErr, "ERROR"
		}
		loc := make([]string, 0, 3)
		if ev.unit != "" {
			loc = append(loc, "unit "+styleUnit.Render(ev.unit))
		}
		if sp.Model != "" {
			loc = append(loc, "model "+sp.Model)
		}
		fmt.Fprintf(&b, "\n%s · %s · %s\n", statusStyle.Render(statusCode), sp.Duration(), ev.ts.UTC().Format("15:04:05.000"))
		if len(loc) > 0 {
			fmt.Fprintf(&b, "%s\n", strings.Join(loc, " · "))
		}
		if ev.detail != "" {
			fmt.Fprintf(&b, "%s\n", styleWarn.Render(ev.detail))
		}
	}

	if ok && ev.hasDatabag {
		m.renderDatabags(&b, sp)
	}
	if ok {
		m.renderNetwork(&b, sp)
		m.renderCorrelatedLogs(&b, sp)
	}
	m.overlay.SetContent(b.String())
	m.overlay.GotoTop()
}

// renderNetwork lists the RPC behind the event: facade.method, timing, and the
// verbatim params/response envelope fields. This is the "Network" view — the
// raw traffic, demoted from the timeline to a drill-down.
func (m *model) renderNetwork(b *strings.Builder, sp recording.SpanRow) {
	fmt.Fprintf(b, "\n%s\n", lipgloss.NewStyle().Bold(true).Render("network"))
	fmt.Fprintf(b, "  %s\n", sp.Name)
	if len(sp.Attrs) == 0 {
		return
	}
	keys := make([]string, 0, len(sp.Attrs))
	for k := range sp.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "  %s = %s\n", k, oneLine(sp.Attrs[k]))
	}
}

// renderDatabags shows the relation databags a span wrote. In diff mode (`d`)
// each databag is rendered as a per-key diff against the value it held just
// before this write; otherwise the current key/values are listed. Because no-op
// commits dedup away (M7), only real changes reach this path.
func (m *model) renderDatabags(b *strings.Builder, sp recording.SpanRow) {
	if m.db == nil {
		return
	}
	snaps, err := m.db.SnapshotsByProducingSpan(sp.SpanID, string(index.KindDatabag))
	if err != nil || len(snaps) == 0 {
		return
	}
	title := "databags (press d for diff)"
	if m.diff {
		title = "databags (diff — press d for values)"
	}
	fmt.Fprintf(b, "\n%s\n", lipgloss.NewStyle().Bold(true).Render(title))
	for _, s := range snaps {
		label := strings.TrimPrefix(s.Scope, "databag:")
		fmt.Fprintf(b, "  %s\n", styleHook.Render(label))
		cur := parseFlatMap([]byte(s.Body))
		if !m.diff {
			for _, k := range sortedKeys(cur) {
				fmt.Fprintf(b, "    %s = %s\n", k, oneLine(cur[k]))
			}
			continue
		}
		prevBody, _ := m.db.PrevSnapshotBefore(m.activeModel.ID, s.Scope, sp.Start)
		writeDatabagDiff(b, parseFlatMap([]byte(prevBody)), cur)
	}
}

// writeDatabagDiff renders the key-level diff between two flat databags.
func writeDatabagDiff(b *strings.Builder, prev, cur map[string]string) {
	changed := false
	for _, k := range sortedKeys(cur) {
		switch old, ok := prev[k]; {
		case !ok:
			fmt.Fprintf(b, "    %s\n", styleOK.Render("+ "+k+" = "+oneLine(cur[k])))
			changed = true
		case old != cur[k]:
			fmt.Fprintf(b, "    %s\n", styleWarn.Render("~ "+k+": "+oneLine(old)+" → "+oneLine(cur[k])))
			changed = true
		}
	}
	for _, k := range sortedKeys(prev) {
		if _, ok := cur[k]; !ok {
			fmt.Fprintf(b, "    %s\n", styleErr.Render("- "+k+" = "+oneLine(prev[k])))
			changed = true
		}
	}
	if !changed {
		fmt.Fprintf(b, "    %s\n", styleDim.Render("(no change)"))
	}
}

// parseFlatMap decodes a databag JSON object into a flat string map.
func parseFlatMap(body []byte) map[string]string {
	out := map[string]string{}
	if len(body) == 0 {
		return out
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return out
	}
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out[k] = s
		} else {
			out[k] = string(v)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// oneLine collapses whitespace/newlines so a multi-line databag value stays on
// one row.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// logWindow is how far on either side of a span's timespan we pull same-unit
// log lines for the fuzzy (unit, time-window) correlation of VISION §7.
const logWindow = 5 * time.Second

// renderCorrelatedLogs appends the debug-log lines correlated to sp: any line
// carrying its span-id (exact, marked »), plus same-unit lines within logWindow.
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
		fmt.Fprintf(b, "  %s %s %s [%s] %s\n", marker, ts,
			logLevelStyle(lg.Level).Render(fmt.Sprintf("%-7s", lg.Level)),
			logSourceLabel(lg.Source), lg.Body)
	}
}

// logLevelStyle colours a log level so ERROR/WARNING stand out.
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
