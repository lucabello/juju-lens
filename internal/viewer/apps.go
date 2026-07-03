package viewer

import (
	"sort"
	"strings"

	"github.com/lucabello/juju-lens/internal/recording"
)

// appTree is a snapshot of apps → units built from every unit-bearing span.
// It is deterministic (sorted alphabetically) so the sidebar order is
// stable across recording reloads.
type appTree struct {
	apps  []string
	units map[string][]string // app -> units
}

// buildAppTree derives the sidebar tree from a span list. Any span with a
// non-empty Unit contributes its app + unit; empty units are skipped so
// controller-side facade spans don't create phantom applications.
func buildAppTree(spans []recording.SpanRow) appTree {
	seen := map[string]map[string]struct{}{}
	for _, sp := range spans {
		if sp.Unit == "" {
			continue
		}
		app := appOf(sp.Unit)
		if app == "" {
			continue
		}
		if _, ok := seen[app]; !ok {
			seen[app] = map[string]struct{}{}
		}
		seen[app][sp.Unit] = struct{}{}
	}
	var apps []string
	for a := range seen {
		apps = append(apps, a)
	}
	sort.Strings(apps)
	units := map[string][]string{}
	for a, us := range seen {
		list := make([]string, 0, len(us))
		for u := range us {
			list = append(list, u)
		}
		sort.Strings(list)
		units[a] = list
	}
	return appTree{apps: apps, units: units}
}

// appOf mirrors index.appOf so the viewer can flatten units without
// depending on the index package's private helper.
func appOf(unit string) string {
	if i := strings.IndexByte(unit, '/'); i >= 0 {
		return unit[:i]
	}
	return unit
}

func (m *model) renderAppsPane() string {
	w, _, _ := columnWidths(m.width)
	h := m.bodyHeight()
	var lines []string
	lines = append(lines, styleSection.Render("Applications"))
	if len(m.appTree.apps) == 0 {
		lines = append(lines, styleDim.Render("(no unit-bearing spans)"))
	}
	// Highlight the app/unit that owns the currently-selected timeline
	// span so the sidebar reflects context.
	selUnit := ""
	if len(m.spans) > 0 {
		selUnit = m.currentSpan().Unit
	}
	selApp := appOf(selUnit)
	inner := max(1, w-4)
	for _, app := range m.appTree.apps {
		label := "▾ " + app
		if app == selApp {
			label = styleApp.Render(label)
		} else {
			label = styleApp.Render(label)
		}
		lines = append(lines, truncate(label, inner))
		for _, u := range m.appTree.units[app] {
			row := "    " + u
			if u == selUnit {
				row = styleSelected.Width(inner).Render(row)
			} else {
				row = styleUnit.Render(row)
			}
			lines = append(lines, truncate(row, inner))
		}
	}
	inner2 := max(1, h-2)
	for len(lines) < inner2 {
		lines = append(lines, "")
	}
	if len(lines) > inner2 {
		lines = lines[:inner2]
	}
	return styleAppsBox.Width(w).Height(h).Render(strings.Join(lines, "\n"))
}
