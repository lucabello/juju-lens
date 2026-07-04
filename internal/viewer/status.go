package viewer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"

	"github.com/charmbracelet/lipgloss"
)

// statusValue is the flattened view of a status snapshot for either an app
// or a unit. Kept alongside its scope key so the pane can render "unknown"
// rows when we know a scope exists but no snapshot has arrived yet.
type statusValue struct {
	Value   string
	Message string
	Since   time.Time
	Known   bool
}

// snapBody mirrors the JSON shape written by index.ExtractSnapshots. Kept
// separately (not shared with the extract package) so the viewer never
// depends on the extractor's internal types.
type snapBody struct {
	Value   string    `json:"value"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
}

// scopeMap parses status snapshot rows into a map keyed by the tail of the
// scope (the app or unit name). The caller supplies the rows (latest, or
// as-of-cursor in point-in-time mode), keeping this pure and testable.
func scopeMap(rows []index.SnapshotRow, scopePrefix string) map[string]statusValue {
	out := map[string]statusValue{}
	for _, r := range rows {
		key := strings.TrimPrefix(r.Scope, scopePrefix)
		var body snapBody
		if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
			out[key] = statusValue{Known: false}
			continue
		}
		out[key] = statusValue{
			Value:   body.Value,
			Message: body.Message,
			Since:   body.Since,
			Known:   true,
		}
	}
	return out
}

// relationSummary is a relation and the entities (units/apps) that have written
// a databag on it, derived from databag snapshot scopes
// "databag:<relation>:<entity>".
type relationSummary struct {
	Key       string   // e.g. "loki.certificates#ca.certificates"
	Endpoints []string // e.g. ["loki:certificates", "ca:certificates"]
	Entities  []string // units/apps with a databag on this relation
}

// buildRelations groups databag snapshot rows by relation.
func buildRelations(rows []index.SnapshotRow) []relationSummary {
	byRel := map[string]map[string]bool{}
	for _, r := range rows {
		// scope = "databag:<relation>:<entity>"
		parts := strings.SplitN(r.Scope, ":", 3)
		if len(parts) != 3 {
			continue
		}
		rel, entity := parts[1], parts[2]
		if byRel[rel] == nil {
			byRel[rel] = map[string]bool{}
		}
		byRel[rel][entity] = true
	}
	out := make([]relationSummary, 0, len(byRel))
	for rel, ents := range byRel {
		entities := make([]string, 0, len(ents))
		for e := range ents {
			entities = append(entities, e)
		}
		sort.Strings(entities)
		out = append(out, relationSummary{Key: rel, Endpoints: relationEndpoints(rel), Entities: entities})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// relationEndpoints turns a relation key into human endpoints:
//
//	"loki.certificates#ca.certificates" -> ["loki:certificates", "ca:certificates"]
//	"mimir.mimir-peers"                 -> ["mimir:mimir-peers"] (peer relation)
func relationEndpoints(key string) []string {
	var out []string
	for _, ep := range strings.Split(key, "#") {
		out = append(out, strings.Replace(ep, ".", ":", 1))
	}
	return out
}

func statusStyleFor(value string) lipgloss.Style {
	switch value {
	case "active":
		return styleOK
	case "waiting", "maintenance", "blocked":
		return styleWarn
	case "error":
		return styleErr
	default:
		return styleDim
	}
}

func statusGlyph(value string) string {
	switch value {
	case "active":
		return "●"
	case "waiting", "maintenance":
		return "◐"
	case "blocked", "error":
		return "●"
	default:
		return "○"
	}
}

func (m *model) renderStatusPane() string {
	_, _, w := columnWidths(m.width)
	h := m.bodyHeight()
	inner := max(1, w-4)

	var lines []string
	// In point-in-time mode, label the pane with the instant it reflects.
	if m.pit && len(m.spans) > 0 {
		lines = append(lines, styleHook.Render("● as of "+m.currentSpan().Start.Format("15:04:05.000")))
	}
	lines = append(lines, styleSection.Render("Applications"))
	lines = append(lines, statusRows(m.appTree.apps, m.appStatuses, inner)...)
	lines = append(lines, "")
	lines = append(lines, styleSection.Render("Units"))
	// Iterate units in stable app-major, unit-minor order so the pane
	// reads top-down like `juju status` output.
	units := []string{}
	for _, app := range m.appTree.apps {
		units = append(units, m.appTree.units[app]...)
	}
	// unit rows can also include units that reported status but that we
	// haven't inferred an app for (defensive; buildAppTree already
	// includes anything with a unit).
	extras := []string{}
	for u := range m.unitStatuses {
		if !stringIn(units, u) {
			extras = append(extras, u)
		}
	}
	sort.Strings(extras)
	units = append(units, extras...)
	lines = append(lines, statusRows(units, m.unitStatuses, inner)...)

	if len(m.relations) > 0 {
		lines = append(lines, "")
		header := "Relations"
		if m.focus == paneRelations {
			header += styleDim.Render("  (↑/↓ select · Tab exit)")
		}
		lines = append(lines, styleSection.Render(header))
		sel := -1
		if m.focus == paneRelations {
			sel = m.relCursor
		}
		lines = append(lines, relationRows(m.relations, inner, sel)...)
	}

	body := max(1, h-2)
	for len(lines) < body {
		lines = append(lines, "")
	}
	if len(lines) > body {
		lines = lines[:body]
	}
	return styleStatusBox.Width(w).Height(h).Render(strings.Join(lines, "\n"))
}

// statusRows renders one row per scope. Unknown scopes are dimmed and
// labelled so the user can tell "no snapshot yet" from "silent".
func statusRows(names []string, byName map[string]statusValue, inner int) []string {
	var out []string
	for _, name := range names {
		st, ok := byName[name]
		if !ok || !st.Known {
			row := fmt.Sprintf("  %s %-*s %s", "○", nameWidth(name, inner), name, styleDim.Render("unknown"))
			out = append(out, truncate(row, inner))
			continue
		}
		st.Since = st.Since.UTC()
		style := statusStyleFor(st.Value)
		row := fmt.Sprintf("  %s %-*s %s",
			style.Render(statusGlyph(st.Value)),
			nameWidth(name, inner),
			name,
			style.Render(st.Value))
		out = append(out, truncate(row, inner))
		if st.Message != "" {
			out = append(out, truncate("      "+styleDim.Render(st.Message), inner))
		}
	}
	return out
}

// relationRows renders the Relations section: one line per relation showing its
// endpoints, then a dim line listing the entities with a databag on it. sel is
// the index of the selected relation (-1 for none), marked with a caret.
func relationRows(rels []relationSummary, inner, sel int) []string {
	var out []string
	for i, r := range rels {
		marker := "  "
		eps := styleHook.Render(strings.Join(r.Endpoints, " ↔ "))
		if i == sel {
			marker = styleHook.Render("▸ ")
			eps = styleSelected.Render(strings.Join(r.Endpoints, " ↔ "))
		}
		out = append(out, truncate(marker+eps, inner))
		if len(r.Entities) > 0 {
			out = append(out, truncate("      "+styleDim.Render(strings.Join(r.Entities, ", ")), inner))
		}
	}
	return out
}

// nameWidth caps the name column to a reasonable width so long unit names
// don't push status text off-screen in narrow terminals.
func nameWidth(name string, inner int) int {
	max := 16
	if inner < 40 {
		max = 10
	}
	if len(name) < max {
		return len(name)
	}
	return max
}

func stringIn(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
