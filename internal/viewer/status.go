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

// latestByScope reads the latest snapshot per scope for the given kind and
// returns a map keyed by the tail of the scope (the app or unit name).
// Failures are treated as "no data known" — the pane still renders,
// dimmed, so recording gaps are visible instead of invisible.
func latestByScope(db *index.DB, modelID int64, kind, scopePrefix string) map[string]statusValue {
	out := map[string]statusValue{}
	rows, err := db.LatestPerScope(modelID, kind)
	if err != nil {
		return out
	}
	for _, r := range rows {
		key := strings.TrimPrefix(r.Scope, scopePrefix)
		var body snapBody
		if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
			// Malformed body ends up as an unknown row.
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
