package viewer

import (
	"fmt"
	"os"
	"sort"
	"strings"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// renderOverlayFrame draws the inspector overlay over the body. It is opened
// with `enter` on an event and fills the whole body area (everything but the
// header and the help footer). The inner viewport width is bounded to the box's
// content area so nothing ever spills past the terminal edge.
func (m *model) renderOverlayFrame() string {
	h := m.bodyHeight()
	m.overlay.Width = max(1, m.width-4)
	m.overlay.Height = max(1, h-2)
	box := styleBox.BorderForeground(lipgloss.Color("4"))
	// lipgloss adds the border outside .Width()/.Height(), so pass the inner box
	// size (terminal minus the 1-cell border on each edge) to keep the frame
	// within the terminal instead of spilling 2 cols/rows past it.
	return box.Width(max(1, m.width-2)).Height(max(1, h-2)).Render(m.overlay.View())
}

// renderOverlay fills the overlay viewport with the selected event's detail. The
// head is deliberately spare — when the event happened, on which unit, and the
// facade.method behind it — followed by the databag or config it changed. Those
// are shown as recursively pretty-printed JSON (nested JSON/YAML string values
// expanded in place); `d` overlays a git-style diff against the previous value.
// Long lines are hard-wrapped to the viewport width so the box never exceeds the
// terminal. Logs live in the Logs pane, not here.
func (m *model) renderOverlay() {
	ev := m.currentEvent()
	sp, ok := m.spanByID(ev.spanID)
	m.copyCur, m.copyPrev = "", ""

	// One spare head line: when · on which unit · the facade.method behind it.
	head := []string{styleDim.Render(ev.ts.UTC().Format("15:04:05.000"))}
	if ev.unit != "" {
		head = append(head, "unit "+styleUnit.Render(ev.unit))
	}
	if ok {
		head = append(head, sp.Name)
	}
	var body strings.Builder
	fmt.Fprintf(&body, "%s\n", strings.Join(head, " · "))
	m.renderFailure(&body, ev, sp, ok)
	m.renderStatusChanges(&body, ev)
	if ok && sp.StatusCode == "ERROR" {
		line := "ERROR"
		if sp.StatusMsg != "" {
			line += ": " + sp.StatusMsg
		}
		fmt.Fprintf(&body, "%s\n", styleErr.Render(line))
	}
	if ev.detail != "" {
		fmt.Fprintf(&body, "%s\n", styleWarn.Render(ev.detail))
	}

	if ok && ev.hasDatabag {
		m.renderDatabags(&body, sp)
	}
	if isConfigChanged(ev) {
		m.renderConfig(&body, ev)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", styleApp.Render("Inspector · "+ev.summary))
	fmt.Fprintf(&b, "%s\n\n", styleDim.Render(m.overlayHelp()))
	b.WriteString(body.String())

	m.overlay.SetContent(ansi.Hardwrap(b.String(), max(1, m.width-4), false))
}

// renderFailure explains, in the inspector, how a hook run went wrong (M10).
// Each failState reads differently because they mean different things: a unit
// stuck in error state now, a hook that recovered after a retry, a bracket a
// later hook genuinely interrupted mid-run, one that finished but lost its own
// closing marker (a capture gap, not a charm/uniter event, M13), or a completed
// hook whose RPC merely errored.
func (m *model) renderFailure(b *strings.Builder, ev event, sp recording.SpanRow, ok bool) {
	switch ev.fail {
	case failErrored:
		fmt.Fprintf(b, "%s\n", styleErr.Render("hook in error state — it errored and the uniter is still retrying it; the unit is in error state"))
	case failRetried:
		fmt.Fprintf(b, "%s\n", styleDim.Render("hook retried — it errored, the uniter retried it, and it then completed successfully"))
	case failInterrupted:
		fmt.Fprintf(b, "%s\n", styleWarn.Render("hook interrupted — another hook opened before this one reached done; it was genuinely abandoned mid-run"))
	case failLostContinue:
		fmt.Fprintf(b, "%s\n", styleWarn.Render("capture gap — this hook actually finished (it reached done), but its own closing marker is missing from the recording; almost certainly a dropped span, not a charm or uniter problem"))
	case failRPCWarn:
		line := "an RPC in this hook returned an error (the hook still completed, and charm status is unaffected)"
		if ok && sp.StatusMsg != "" {
			line += ": " + sp.StatusMsg
		}
		fmt.Fprintf(b, "%s\n", styleWarn.Render(line))
	}
}

// renderStatusChanges lists the workload/application statuses the charm set from
// inside this hook (M10). Every status a charm reports is set while a hook runs,
// so this is the hook that drove the charm into its current state.
func (m *model) renderStatusChanges(b *strings.Builder, ev event) {
	if len(ev.statuses) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", styleSection.Render("status set by this hook"))
	for _, s := range ev.statuses {
		who := "unit"
		if s.App {
			who = "app"
		}
		line := fmt.Sprintf("  %s → %s", who, statusStyleFor(s.Value).Render(s.Value))
		if s.Message != "" {
			line += "  " + styleDim.Render("\""+s.Message+"\"")
		}
		fmt.Fprintf(b, "%s\n", line)
	}
}

// overlayHelp is the keybinding line at the top of the inspector. The diff /
// copy hints only appear when the event actually carries a databag or config to
// act on.
func (m *model) overlayHelp() string {
	help := "esc close · ↑/↓ scroll"
	if m.copyCur != "" || m.copyPrev != "" {
		help += " · d diff · y copy after · Y copy before"
	}
	return help
}

// isConfigChanged reports whether an event is a unit's config-changed hook, for
// which the inspector shows the charm-config diff. See narrative.IsConfigChanged.
func isConfigChanged(ev event) bool {
	return narrative.IsConfigChanged(narrative.Event{Kind: ev.kind, Summary: ev.summary})
}

// inspectable reports whether an event has anything worth drilling into: a
// databag change, a config change, a failure, or a status the hook drove the
// charm into (M10). Enter is a no-op on anything else. See
// narrative.Inspectable.
func inspectable(ev event) bool {
	return narrative.Inspectable(narrative.Event{
		Kind: ev.kind, Summary: ev.summary, HasDatabag: ev.hasDatabag,
		Fail: ev.fail, Statuses: ev.statuses,
	})
}

// renderDatabags shows, for each relation the hook touched, all of the local
// application's databags on it, grouped under the relation:
//
//	alertmanager:grafana-source → grafana:grafana-source
//	  app
//	    { … }
//	  unit (grafana/0)
//	    { … }
//
// Values are recursively pretty-printed JSON; `d` switches to a git-style diff
// against the value each held just before this write (only the side the hook
// actually moved shows +/- lines). `y`/`Y` copy the current/previous contents.
// Relations are separated by a blank line.
func (m *model) renderDatabags(b *strings.Builder, sp recording.SpanRow) {
	if m.db == nil {
		return
	}
	changed, err := m.db.SnapshotsByProducingSpan(sp.SpanID, string(index.KindDatabag))
	if err != nil || len(changed) == 0 {
		return
	}
	// The relations this hook touched, and which scopes it actually changed.
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
	// Every databag value as of the event, so we can list all of the local
	// application's databags (app + each unit) on the touched relations.
	rows, _ := m.db.LatestPerScopeAsOf(m.activeModel.ID, string(index.KindDatabag), sp.Start)
	body := make(map[string]string, len(rows))
	for _, r := range rows {
		body[r.Scope] = r.Body
	}

	fmt.Fprintf(b, "\n%s\n", styleSection.Render("databags"))
	curAll := map[string]any{}
	prevAll := map[string]any{}
	for i, key := range keys {
		if i > 0 {
			b.WriteByte('\n') // blank line between relations
		}
		fmt.Fprintf(b, "  %s\n", renderRelationLabel(key, local))
		for _, e := range narrative.LocalDatabagEntries(body, key, local) {
			cur := body[e.Scope]
			prev := cur // unchanged: rendered as plain context
			if changedScope[e.Scope] {
				prev, _ = m.db.PrevSnapshotBefore(m.activeModel.ID, e.Scope, sp.Start)
			}
			fmt.Fprintf(b, "    %s\n", styleUnit.Render(e.Label))
			if m.diffMode {
				writeStructuredDiff(b, "      ", prev, cur)
			} else {
				writeStructured(b, "      ", cur)
			}
			ck := key + " " + e.Label
			curAll[ck] = narrative.ExpandJSON([]byte(cur))
			prevAll[ck] = narrative.ExpandJSON([]byte(prev))
		}
	}
	m.copyCur = narrative.MarshalIndent(curAll)
	m.copyPrev = narrative.MarshalIndent(prevAll)
}

// renderConfig shows the charm-config for a config-changed hook: the config read
// during the hook, pretty-printed, with `d` overlaying a git-style diff against
// the value in effect before the hook — i.e. what the config change actually
// did. Copyable with `y`/`Y`. Nothing is shown when no config was captured.
func (m *model) renderConfig(b *strings.Builder, ev event) {
	if m.db == nil {
		return
	}
	scope := "config:" + ev.app
	curBody, _ := m.db.LatestSnapshotBefore(m.activeModel.ID, scope, ev.ts.Add(ev.dur))
	if curBody == "" {
		return
	}
	prevBody, _ := m.db.PrevSnapshotBefore(m.activeModel.ID, scope, ev.ts)
	m.copyCur = narrative.PrettyJSON(curBody)
	m.copyPrev = narrative.PrettyJSON(prevBody)

	fmt.Fprintf(b, "\n%s\n", styleSection.Render("config"))
	if m.diffMode {
		writeStructuredDiff(b, "  ", prevBody, curBody)
		return
	}
	writeStructured(b, "  ", curBody)
}

// formatDatabagLabel turns a databag scope into a readable (uncoloured) relation
// label and reports whether it is the application databag (vs a unit databag):
//
//	databag:loki.certificates#ca.certificates:loki/0 -> "loki:certificates → ca:certificates", false
//	databag:mimir.mimir-peers:mimir/0                -> "mimir:mimir-peers (peer)", false
func formatDatabagLabel(scope string) (rel string, isApp bool) {
	key, entity := narrative.SplitDatabagScope(scope)
	return narrative.RelationLabel(key, appOf(entity)), !strings.ContainsRune(entity, '/')
}

// renderRelationLabel is the coloured relation label for the inspector: the
// application name in normal text, only the ":endpoint" accented, local side
// first. Structurally the same "which side is local, peer vs two-sided" logic
// as narrative.RelationLabel, just with lipgloss styling per segment instead
// of plain text.
func renderRelationLabel(key, local string) string {
	seg := func(app, ep string) string {
		return styleText.Render(app) + styleHook.Render(":"+ep)
	}
	if before, after, ok := strings.Cut(key, "#"); ok {
		a1, e1 := narrative.CutDot(before)
		a2, e2 := narrative.CutDot(after)
		if a2 == local && a1 != local { // put the local side first
			a1, e1, a2, e2 = a2, e2, a1, e1
		}
		return seg(a1, e1) + styleDim.Render(" → ") + seg(a2, e2)
	}
	a, e := narrative.CutDot(key)
	return seg(a, e) + styleDim.Render(" (peer)")
}

// writeStructured pretty-prints body as recursively-expanded JSON, each line
// prefixed with indent.
func writeStructured(b *strings.Builder, indent, body string) {
	p := narrative.PrettyJSON(body)
	if p == "" {
		fmt.Fprintf(b, "%s%s\n", indent, styleDim.Render("(empty)"))
		return
	}
	for _, line := range strings.Split(p, "\n") {
		fmt.Fprintf(b, "%s%s\n", indent, styleText.Render(line))
	}
}

// writeStructuredDiff renders a git-style line diff between the pretty-printed
// prev and cur bodies: unchanged lines in normal text (so an untouched databag
// still reads cleanly), additions in green with "+", removals in red with "-".
func writeStructuredDiff(b *strings.Builder, indent, prevBody, curBody string) {
	prev := narrative.SplitLines(narrative.PrettyJSON(prevBody))
	cur := narrative.SplitLines(narrative.PrettyJSON(curBody))
	diff := narrative.LineDiff(prev, cur)
	if len(diff) == 0 {
		fmt.Fprintf(b, "%s%s\n", indent, styleDim.Render("(empty)"))
		return
	}
	for _, d := range diff {
		switch d.Op {
		case '+':
			fmt.Fprintf(b, "%s%s\n", indent, styleOK.Render("+ "+d.Text))
		case '-':
			fmt.Fprintf(b, "%s%s\n", indent, styleDel.Render("- "+d.Text))
		default:
			fmt.Fprintf(b, "%s%s\n", indent, styleText.Render("  "+d.Text))
		}
	}
}

// copyCmd emits an OSC52 clipboard-set escape to the terminal (works over SSH),
// copying s to the system clipboard. A blank string is a no-op.
func copyCmd(s string) tea.Cmd {
	return func() tea.Msg {
		if s != "" {
			fmt.Fprint(os.Stderr, osc52.New(s))
		}
		return nil
	}
}
