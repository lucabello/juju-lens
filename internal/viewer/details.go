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
	// When the Relations pane is focused, the details area shows the selected
	// relation's databags instead of the current span.
	if m.focus == paneRelations && m.relCursor < len(m.relations) {
		m.renderRelationDetails(m.relations[m.relCursor])
		return
	}
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

	// Databags this span wrote (CommitHookChanges), with `d` toggling a diff
	// against the previous value of each relation databag.
	m.renderDatabags(&b, sp)
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

// renderRelationDetails shows every entity's databag on the selected relation.
// With diff mode (`d`) each entity's databag is shown as a diff against its
// previous value; otherwise the current key/values are listed. Point-in-time
// mode flows through automatically, since m.databags holds the as-of rows.
func (m *model) renderRelationDetails(rel relationSummary) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", lipgloss.NewStyle().Bold(true).Render("Relation "+strings.Join(rel.Endpoints, " ↔ ")))
	if m.pit && len(m.spans) > 0 {
		fmt.Fprintf(&b, "  %s\n", styleDim.Render("as of "+m.currentSpan().Start.Format("15:04:05.000")))
	}
	hint := "press d for diff"
	if m.diff {
		hint = "diff vs previous — press d for values"
	}
	fmt.Fprintf(&b, "  %s\n", styleDim.Render(hint))

	for _, entity := range rel.Entities {
		scope := "databag:" + rel.Key + ":" + entity
		row, ok := m.databags[scope]
		fmt.Fprintf(&b, "\n%s\n", styleHook.Render("  "+entity))
		if !ok {
			fmt.Fprintf(&b, "    %s\n", styleDim.Render("(no databag)"))
			continue
		}
		cur := parseFlatMap([]byte(row.Body))
		if m.diff {
			prevBody, _ := m.db.PrevSnapshotBefore(m.activeModel.ID, scope, row.Ts)
			writeDatabagDiff(&b, parseFlatMap([]byte(prevBody)), cur)
			continue
		}
		if len(cur) == 0 {
			fmt.Fprintf(&b, "    %s\n", styleDim.Render("(empty)"))
		}
		for _, k := range sortedKeys(cur) {
			fmt.Fprintf(&b, "    %s = %s\n", k, oneLine(cur[k]))
		}
	}
	m.details.SetContent(b.String())
	m.details.GotoTop()
}

// renderDatabags shows the relation databags a span wrote. In diff mode (`d`)
// each databag is rendered as a per-key diff against the value it held just
// before this write (green +added, red -removed, yellow ~changed); otherwise
// the current key/values are listed.
func (m *model) renderDatabags(b *strings.Builder, sp recording.SpanRow) {
	if m.db == nil {
		return
	}
	snaps, err := m.db.SnapshotsByProducingSpan(sp.SpanID, string(index.KindDatabag))
	if err != nil || len(snaps) == 0 {
		return
	}
	title := "databags"
	if m.diff {
		title = "databags (diff — press d for values)"
	} else {
		title = "databags (press d for diff)"
	}
	fmt.Fprintf(b, "\n%s\n", lipgloss.NewStyle().Bold(true).Render(title))
	for _, s := range snaps {
		// scope = "databag:<relation>:<entity>"
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
		prev := parseFlatMap([]byte(prevBody))
		writeDatabagDiff(b, prev, cur)
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

// parseFlatMap decodes a databag JSON object into a flat string map. Values that
// are not plain strings are re-encoded compactly so they still diff cleanly.
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

// oneLine collapses whitespace/newlines so a multi-line databag value (a PEM
// cert, a YAML blob) stays on one details row.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
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
