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
	_, w := topRowWidths(m.width)
	h, _ := rowHeights(m.bodyHeight())

	// Change pips: mark whatever the *selected* event changed at this instant,
	// so scrubbing shows what moved without leaving the pane.
	changedApp, changedUnit := "", ""
	if ev := m.currentEvent(); ev.kind == evStatus {
		if strings.HasPrefix(ev.summary, "app") {
			changedApp = ev.app
		} else {
			changedUnit = ev.unit
		}
	}

	label := "latest known"
	if m.pit {
		label = "as of " + m.currentTs().UTC().Format("15:04:05.000")
	}
	rows := []paneRow{row(styleDim.Render(label))}

	// The app/unit universe comes from spans *and* snapshots, so a bootstrap-only
	// app (one with no captured RPCs, M8) still appears.
	apps, unitsByApp := m.statusUniverse()
	rows = append(rows, row(styleSection.Render("Applications")))
	rows = append(rows, appStatusRows(apps, m.appStatuses, changedApp)...)
	rows = append(rows, row(""))
	rows = append(rows, row(styleSection.Render("Units  ")+styleDim.Render("workload / agent")))
	var units []string
	for _, app := range apps {
		units = append(units, unitsByApp[app]...)
	}
	rows = append(rows, m.unitStatusRows(units, changedUnit)...)

	if len(m.relations) > 0 {
		rows = append(rows, row(""))
		header := "Relations"
		if m.focus == paneStatus {
			header += styleDim.Render("  (↑/↓ select)")
		}
		rows = append(rows, row(styleSection.Render(header)))
		sel := -1
		if m.focus == paneStatus {
			sel = m.relCursor
		}
		rows = append(rows, relationRows(m.relations, sel)...)
	}

	return m.renderPane(paneStatus, w, h, "Status", rows)
}

// statusUniverse returns the apps (sorted) and their units to show in the Status
// pane, unioning the RPC-derived app tree with any app/unit that only has a
// status snapshot (a bootstrap-only app, M8).
func (m *model) statusUniverse() (apps []string, unitsByApp map[string][]string) {
	appSet := map[string]bool{}
	unitSet := map[string]map[string]bool{}
	add := func(app, unit string) {
		if app == "" {
			return
		}
		appSet[app] = true
		if unitSet[app] == nil {
			unitSet[app] = map[string]bool{}
		}
		if unit != "" {
			unitSet[app][unit] = true
		}
	}
	for _, app := range m.appTree.apps {
		add(app, "")
		for _, u := range m.appTree.units[app] {
			add(app, u)
		}
	}
	for app := range m.appStatuses {
		add(app, "")
	}
	for unit := range m.unitStatuses {
		add(appOf(unit), unit)
	}
	for a := range appSet {
		apps = append(apps, a)
	}
	sort.Strings(apps)
	unitsByApp = map[string][]string{}
	for _, a := range apps {
		var us []string
		for u := range unitSet[a] {
			us = append(us, u)
		}
		sort.Strings(us)
		unitsByApp[a] = us
	}
	return apps, unitsByApp
}

// appStatusRows renders one row per application. Unknown scopes are dimmed and
// labelled so the user can tell "no snapshot yet" from "silent". changed names
// an app that moved at the selected instant; it gets a ▲ pip.
func appStatusRows(names []string, byName map[string]statusValue, changed string) []paneRow {
	var out []paneRow
	for _, name := range names {
		pip := " "
		if name == changed && changed != "" {
			pip = styleWarn.Render("▲")
		}
		st, ok := byName[name]
		if !ok || !st.Known {
			out = append(out, row(fmt.Sprintf("%s %s %s %s",
				pip, "○", styleText.Render(fmt.Sprintf("%-16s", name)), styleDim.Render("unknown"))))
			continue
		}
		style := statusStyleFor(st.Value)
		out = append(out, row(fmt.Sprintf("%s %s %s %s",
			pip,
			style.Render(statusGlyph(st.Value)),
			styleText.Render(fmt.Sprintf("%-16s", name)),
			style.Render(st.Value))))
		if st.Message != "" {
			out = append(out, row("     "+styleDim.Render(st.Message)))
		}
	}
	return out
}

// unitStatusRows renders one row per unit showing both statuses juju tracks
// separately: the workload (charm) status and the agent status (idle/executing),
// as "workload / agent". changed names a unit that moved at the selected instant.
func (m *model) unitStatusRows(names []string, changed string) []paneRow {
	var out []paneRow
	for _, name := range names {
		pip := " "
		if name == changed && changed != "" {
			pip = styleWarn.Render("▲")
		}
		wl, known := m.unitStatuses[name]
		glyph, workload := "○", styleDim.Render("unknown")
		if known && wl.Known {
			ws := statusStyleFor(wl.Value)
			glyph, workload = ws.Render(statusGlyph(wl.Value)), ws.Render(wl.Value)
		}
		if agent, ok := m.agentStatuses[name]; ok && agent.Known && agent.Value != "" {
			workload += styleDim.Render(" / ") + agentStyleFor(agent.Value).Render(agent.Value)
		}
		out = append(out, row(fmt.Sprintf("%s %s %s %s",
			pip, glyph, styleText.Render(fmt.Sprintf("%-16s", name)), workload)))
		if known && wl.Known && wl.Message != "" {
			out = append(out, row("     "+styleDim.Render(wl.Message)))
		}
	}
	return out
}

// relationRows renders the Relations section: one line per relation showing its
// endpoints, then a dim line listing the entities with a databag on it. sel is
// the index of the selected relation (-1 for none), marked with a caret.
func relationRows(rels []relationSummary, sel int) []paneRow {
	var out []paneRow
	for i, r := range rels {
		marker := "  "
		eps := styleHook.Render(strings.Join(r.Endpoints, " ↔ "))
		if i == sel {
			marker = styleHook.Render("▸ ")
			eps = styleSelected.Render(strings.Join(r.Endpoints, " ↔ "))
		}
		out = append(out, row(marker+eps))
		if len(r.Entities) > 0 {
			out = append(out, row("      "+styleDim.Render(strings.Join(r.Entities, ", "))))
		}
	}
	return out
}

// agentStyleFor colours the agent (executing/idle) status. idle is the resting
// state so it stays calm; executing/allocating draw the eye; failures are red.
func agentStyleFor(value string) lipgloss.Style {
	switch value {
	case "idle":
		return styleDim
	case "executing", "allocating", "rebooting":
		return styleWarn
	case "failed", "error", "lost":
		return styleErr
	default:
		return styleDim
	}
}
