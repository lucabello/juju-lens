package viewer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
	"gopkg.in/yaml.v3"
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
// which the inspector shows the charm-config diff.
func isConfigChanged(ev event) bool {
	return ev.kind == evHook && ev.summary == "config-changed"
}

// inspectable reports whether an event has anything worth drilling into: a
// databag change or a config change. Enter is a no-op on anything else.
func inspectable(ev event) bool {
	return ev.hasDatabag || isConfigChanged(ev)
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
		if k, _ := splitDatabagScope(s.Scope); k != "" && !seenKey[k] {
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
		for _, e := range localDatabagEntries(body, key, local) {
			cur := body[e.scope]
			prev := cur // unchanged: rendered as plain context
			if changedScope[e.scope] {
				prev, _ = m.db.PrevSnapshotBefore(m.activeModel.ID, e.scope, sp.Start)
			}
			fmt.Fprintf(b, "    %s\n", styleUnit.Render(e.label))
			if m.diffMode {
				writeStructuredDiff(b, "      ", prev, cur)
			} else {
				writeStructured(b, "      ", cur)
			}
			ck := key + " " + e.label
			curAll[ck] = expandJSON([]byte(cur))
			prevAll[ck] = expandJSON([]byte(prev))
		}
	}
	m.copyCur = marshalIndent(curAll)
	m.copyPrev = marshalIndent(prevAll)
}

// databagEntry is one row under a relation: the "app"/"unit (name)" label and
// the scope its body lives at.
type databagEntry struct {
	label string
	scope string
}

// localDatabagEntries lists the local application's databags on a relation — its
// application databag first, then each of its units' databags (sorted) — from
// the set of databag scopes present at the event.
func localDatabagEntries(body map[string]string, key, local string) []databagEntry {
	var entries []databagEntry
	if _, ok := body["databag:"+key+":"+local]; ok {
		entries = append(entries, databagEntry{"app", "databag:" + key + ":" + local})
	}
	var units []string
	for scope := range body {
		k, entity := splitDatabagScope(scope)
		if k == key && appOf(entity) == local && strings.ContainsRune(entity, '/') {
			units = append(units, entity)
		}
	}
	sort.Strings(units)
	for _, u := range units {
		entries = append(entries, databagEntry{"unit (" + u + ")", "databag:" + key + ":" + u})
	}
	return entries
}

// splitDatabagScope splits "databag:<key>:<entity>" into its relation key and
// entity (unit or application name).
func splitDatabagScope(scope string) (key, entity string) {
	s := strings.TrimPrefix(scope, "databag:")
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
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
	m.copyCur = prettyJSON(curBody)
	m.copyPrev = prettyJSON(prevBody)

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
	key, entity := splitDatabagScope(scope)
	return relationLabel(key, appOf(entity), false), !strings.ContainsRune(entity, '/')
}

// renderRelationLabel is the coloured relation label for the inspector: the
// application name in normal text, only the ":endpoint" accented, local side
// first.
func renderRelationLabel(key, local string) string {
	return relationLabel(key, local, true)
}

// relationLabel renders "app:endpoint → app:endpoint" (or "app:endpoint (peer)")
// with the local application placed first. When coloured, the application name
// is plain and only ":endpoint" is accented; each segment is rendered on its own
// so no styled string is nested inside another.
func relationLabel(key, local string, colour bool) string {
	seg := func(app, ep string) string {
		if colour {
			return styleText.Render(app) + styleHook.Render(":"+ep)
		}
		return app + ":" + ep
	}
	arrow, peer := " → ", " (peer)"
	if colour {
		arrow, peer = styleDim.Render(" → "), styleDim.Render(" (peer)")
	}
	if before, after, ok := strings.Cut(key, "#"); ok {
		a1, e1 := cutDot(before)
		a2, e2 := cutDot(after)
		if a2 == local && a1 != local { // put the local side first
			a1, e1, a2, e2 = a2, e2, a1, e1
		}
		return seg(a1, e1) + arrow + seg(a2, e2)
	}
	a, e := cutDot(key)
	return seg(a, e) + peer
}

// cutDot splits "app.endpoint" into its two parts.
func cutDot(s string) (app, ep string) {
	if a, b, ok := strings.Cut(s, "."); ok {
		return a, b
	}
	return s, ""
}

// writeStructured pretty-prints body as recursively-expanded JSON, each line
// prefixed with indent.
func writeStructured(b *strings.Builder, indent, body string) {
	p := prettyJSON(body)
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
	prev := splitLines(prettyJSON(prevBody))
	cur := splitLines(prettyJSON(curBody))
	diff := lineDiff(prev, cur)
	if len(diff) == 0 {
		fmt.Fprintf(b, "%s%s\n", indent, styleDim.Render("(empty)"))
		return
	}
	for _, d := range diff {
		switch d.op {
		case '+':
			fmt.Fprintf(b, "%s%s\n", indent, styleOK.Render("+ "+d.text))
		case '-':
			fmt.Fprintf(b, "%s%s\n", indent, styleDel.Render("- "+d.text))
		default:
			fmt.Fprintf(b, "%s%s\n", indent, styleText.Render("  "+d.text))
		}
	}
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// diffLine is one line of a git-style diff: op is ' ' (context), '+' or '-'.
type diffLine struct {
	op   byte
	text string
}

// lineDiff computes a minimal line diff between a and b via an LCS table. Inputs
// are small (a pretty-printed databag/config), so the O(n·m) table is fine.
func lineDiff(a, b []string) []diffLine {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []diffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, diffLine{' ', a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, diffLine{'-', a[i]})
			i++
		default:
			out = append(out, diffLine{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{'-', a[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{'+', b[j]})
	}
	return out
}

// prettyJSON renders a JSON body as indented, recursively-expanded JSON: any
// string field whose value is itself JSON — or nested YAML — is parsed and
// expanded in place, so a databag holding `{"alert_rules":"{...}"}` or a
// YAML relation-state blob reads as structure rather than an escaped string.
// Object keys are sorted, so two versions diff cleanly. Returns the input
// unchanged when it isn't JSON.
func prettyJSON(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	out, err := json.MarshalIndent(expandJSON([]byte(body)), "", "  ")
	if err != nil {
		return body
	}
	return string(out)
}

// expandJSON decodes raw and recursively expands any string leaf that is itself
// JSON or YAML. Non-JSON input is returned as a plain string.
func expandJSON(raw []byte) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	return expandValue(v)
}

func expandValue(v any) any {
	switch t := v.(type) {
	case string:
		if e, ok := expandString(t); ok {
			return e
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = expandValue(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = expandValue(val)
		}
		return t
	}
	return v
}

// expandString tries to interpret a leaf string as embedded structure: JSON when
// it opens with {/[, else multi-line YAML (Juju stores relation-state and some
// databag values as YAML). It only expands when the result is a non-empty
// mapping or sequence — plain scalars (numbers, URLs, addresses) are left as-is.
func expandString(s string) (any, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return nil, false
	}
	if t[0] == '{' || t[0] == '[' {
		var inner any
		if json.Unmarshal([]byte(t), &inner) == nil {
			return expandValue(inner), true
		}
	}
	if strings.Contains(t, "\n") {
		var inner any
		if yaml.Unmarshal([]byte(s), &inner) == nil {
			if norm := normalizeYAML(inner); isStructured(norm) {
				return expandValue(norm), true
			}
		}
	}
	return nil, false
}

// isStructured reports whether v is a non-empty map or slice (as opposed to a
// scalar we should leave untouched).
func isStructured(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return false
}

// normalizeYAML converts YAML's map shapes into JSON-marshalable
// map[string]any recursively (yaml.v3 already uses string keys for interface
// targets, but be defensive about map[any]any from older shapes).
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeYAML(val)
		}
		return t
	case map[any]any:
		mm := make(map[string]any, len(t))
		for k, val := range t {
			mm[fmt.Sprint(k)] = normalizeYAML(val)
		}
		return mm
	case []any:
		for i, val := range t {
			t[i] = normalizeYAML(val)
		}
		return t
	}
	return v
}

// marshalIndent renders v as indented JSON, or "" on error.
func marshalIndent(v any) string {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
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
