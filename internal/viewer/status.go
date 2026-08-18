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

// statusSeverities ranks workload statuses so an application's status can be
// rolled up from its units, mirroring Juju's own derivation
// (domain/status/service applicationDisplayStatusFromUnits): when a leader has
// not explicitly set an application status, Juju displays the highest-severity
// unit workload status. Higher wins; anything unlisted (including "unset") is
// treated as lowest so it never masks a real unit status.
var statusSeverities = map[string]int{
	"error":       100,
	"blocked":     90,
	"maintenance": 80,
	"waiting":     70,
	"active":      60,
	"terminated":  50,
	"unknown":     40,
}

// deriveAppStatuses rolls each application's status up from its units' workload
// statuses using Juju's severity precedence, keyed by app name. The winning
// unit's message is carried so the pane can explain the status, and Since is the
// winner's timestamp. Apps with no known unit statuses are omitted, letting an
// explicit application snapshot (or "unknown") stand in refreshStatus.
func deriveAppStatuses(unitStatuses map[string]statusValue) map[string]statusValue {
	out := map[string]statusValue{}
	for unit, sv := range unitStatuses {
		if !sv.Known {
			continue
		}
		app := appOf(unit)
		cur, ok := out[app]
		if !ok || statusSeverities[sv.Value] > statusSeverities[cur.Value] {
			out[app] = statusValue{
				Value:   sv.Value,
				Message: sv.Message,
				Since:   sv.Since,
				Known:   true,
			}
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

	rows := []paneRow{row(styleDim.Render("as of " + m.statusTs().UTC().Format("15:04:05.000")))}

	// The app/unit universe comes from spans *and* snapshots, so a bootstrap-only
	// app (one with no captured RPCs, M8) still appears.
	apps, unitsByApp := m.statusUniverse()
	rows = append(rows, row(styleSection.Render("Applications")+styleDim.Render("  workload / agent")))
	appRows, next := m.appTreeRows(apps, unitsByApp, changedApp, changedUnit, 0)
	rows = append(rows, appRows...)

	if len(m.relations) > 0 {
		rows = append(rows, row(""))
		// Render the section title and the hint separately: styleSection
		// underlines per-grapheme, which mangles any ANSI already inside the
		// string into literal "[90m" text.
		header := styleSection.Render("Relations")
		if m.focus == paneStatus {
			header += styleDim.Render("  (↑/↓ select, space pin app/unit)")
		}
		rows = append(rows, row(header))
		sel := -1
		if m.focus == paneStatus {
			if local := m.statusCursor - next; local >= 0 && local < len(m.relations) {
				sel = local
			}
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

// appTreeRows renders the aggregated Applications section: each app on its own
// row followed by its units, indented beneath it. The leader unit is marked with
// a trailing "*" on its name (juju's own convention), so leadership reads without
// a separate label. changedApp/changedUnit name whatever the selected event moved
// at this instant; those rows get a ▲ pip. startIdx is this section's offset into
// the Status pane's unified cursor (m.statusCursor); it returns the next free
// index so the caller can continue numbering the Relations section after it.
func (m *model) appTreeRows(apps []string, unitsByApp map[string][]string, changedApp, changedUnit string, startIdx int) ([]paneRow, int) {
	// Pad app names to the longest one (plus a two-space gap) so their statuses
	// line up with each other while staying close to the name, independent of the
	// wider unit column beneath them.
	appWidth := 0
	for _, app := range apps {
		if len(app) > appWidth {
			appWidth = len(app)
		}
	}
	appWidth += 2
	idx := startIdx
	var out []paneRow
	for _, app := range apps {
		selected := m.focus == paneStatus && idx == m.statusCursor
		out = append(out, m.appHeaderRow(app, changedApp, appWidth, selected))
		idx++
		for _, unit := range unitsByApp[app] {
			selected := m.focus == paneStatus && idx == m.statusCursor
			out = append(out, m.unitRows(app, unit, changedUnit, selected)...)
			idx++
		}
	}
	return out, idx
}

// appHeaderRow renders one application line: "<app>  <status>". A pinned app
// (in the scope filter) renders its name in styleScoped instead of styleText;
// selected (the Status pane cursor) draws the whole row reverse-video.
func (m *model) appHeaderRow(app, changed string, appWidth int, selected bool) paneRow {
	pip := " "
	if app == changed && changed != "" {
		pip = styleWarn.Render("▲")
	}
	nameStyle := styleText
	if m.scopeFilter[app] {
		nameStyle = styleScoped
	}
	st, ok := m.appStatuses[app]
	name := nameStyle.Render(fmt.Sprintf("%-*s", appWidth, app))
	var text string
	if !ok || !st.Known {
		text = fmt.Sprintf("%s %s%s", pip, name, styleDim.Render("unknown"))
	} else {
		text = fmt.Sprintf("%s %s%s", pip, name, statusStyleFor(st.Value).Render(st.Value))
	}
	if selected {
		return selRow(text)
	}
	return row(text)
}

// unitRows renders a unit nested under its app: an indented
// "  <unit>[*]  workload / agent  "message"" line, with the workload message (when
// set) beside the status rather than below it. The leader unit's name carries a
// trailing "*". A pinned unit (in the scope filter) renders its name in
// styleScoped instead of styleText; selected (the Status pane cursor) draws the
// whole row reverse-video.
func (m *model) unitRows(app, unit, changed string, selected bool) []paneRow {
	pip := " "
	if unit == changed && changed != "" {
		pip = styleWarn.Render("▲")
	}
	label := unit
	if m.leaders[app] == unit {
		label += "*"
	}
	labelStyle := styleText
	if m.scopeFilter[unit] {
		labelStyle = styleScoped
	}
	wl, known := m.unitStatuses[unit]
	workload := styleDim.Render("unknown")
	if known && wl.Known {
		workload = statusStyleFor(wl.Value).Render(wl.Value)
	}
	// The agent (idle/executing) status is always shown alongside the workload,
	// "unknown" when we never captured it, so the "workload / agent" pair is
	// never half-empty — except once the unit is terminated: there's no agent
	// left to report on, so a stale pre-termination value ("executing") would
	// be misleading. Show nothing next to "terminated" instead.
	terminated := known && wl.Known && wl.Value == "terminated"
	if !terminated {
		agentPart := styleDim.Render("unknown")
		if agent, ok := m.agentStatuses[unit]; ok && agent.Known && agent.Value != "" {
			agentPart = agentStyleFor(agent.Value).Render(agent.Value)
		}
		workload += styleDim.Render(" / ") + agentPart
	}
	if known && wl.Known && wl.Message != "" {
		workload += "  " + styleDim.Render("\""+wl.Message+"\"")
	}
	text := fmt.Sprintf("%s   %s %s",
		pip, labelStyle.Render(fmt.Sprintf("%-14s", label)), workload)
	if selected {
		return []paneRow{selRow(text)}
	}
	return []paneRow{row(text)}
}

// relationRows renders the Relations section: one line per relation showing its
// endpoints as "app:endpoint ↔ app:endpoint". The application name is plain and
// only the ":endpoint" carries the accent colour. sel is the index of the
// selected relation (-1 for none), drawn as a reverse-video bar.
func relationRows(rels []relationSummary, sel int) []paneRow {
	var out []paneRow
	for i, r := range rels {
		eps := renderEndpoints(r.Endpoints)
		if i == sel {
			out = append(out, selRow("▸ "+eps))
		} else {
			out = append(out, row("  "+eps))
		}
	}
	return out
}

// renderEndpoints renders a relation's endpoints as "app:endpoint ↔ …" with the
// application name in normal text and only the ":endpoint" accented, joined by a
// dim arrow. Each segment is rendered on its own so no styled string is nested
// inside another (which would corrupt into literal escape text).
func renderEndpoints(eps []string) string {
	parts := make([]string, len(eps))
	for i, ep := range eps {
		if app, name, ok := strings.Cut(ep, ":"); ok {
			parts[i] = styleText.Render(app) + styleHook.Render(":"+name)
		} else {
			parts[i] = styleHook.Render(ep)
		}
	}
	return strings.Join(parts, styleDim.Render(" ↔ "))
}

// agentStyleFor colours the agent (executing/idle) status. idle is the healthy
// resting state, so it reads green like an active workload; executing/allocating
// draw the eye; failures are red.
func agentStyleFor(value string) lipgloss.Style {
	switch value {
	case "idle":
		return styleOK
	case "executing", "allocating", "rebooting":
		return styleWarn
	case "failed", "error", "lost":
		return styleErr
	default:
		return styleDim
	}
}
