package viewer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

// streamItem is one line of the merged log stream (column [2]): either a
// timeline event (rendered as a ruler) or a log record. Merging the two by
// timestamp is deliberately honest about correlation — we place logs next to
// the events they surround chronologically, and only claim an exact join (the »
// marker) when a log line literally carries the event's span id (VISION §6.2).
type streamItem struct {
	ts    time.Time
	evIdx int           // >= 0 when this item is events[evIdx]; -1 for a log line
	log   *index.LogRow // non-nil for a log line
}

// buildStream merges the model's events and every log source into one
// time-ordered stream, and records where each event landed so the pane can lock
// onto the selected one.
func (m *model) buildStream() {
	logs := m.filterLogs(m.rawLogs)
	items := make([]streamItem, 0, len(m.events)+len(logs))
	for i, ev := range m.events {
		items = append(items, streamItem{ts: ev.ts, evIdx: i})
	}
	for i := range logs {
		items = append(items, streamItem{ts: logs[i].Ts, evIdx: -1, log: &logs[i]})
	}
	sort.SliceStable(items, func(a, b int) bool { return items[a].ts.Before(items[b].ts) })
	m.stream = items
	m.eventStreamIdx = make([]int, len(m.events))
	for pos, it := range items {
		if it.evIdx >= 0 {
			m.eventStreamIdx[it.evIdx] = pos
		}
	}
	m.logCursor = clamp(m.logCursor, 0, max(0, len(items)-1))
}

// filterLogs applies the scope filter and the Logs search query to a batch of
// log rows before they're merged into the stream. Both are no-ops (returns
// logs unchanged) when neither is active.
func (m *model) filterLogs(logs []index.LogRow) []index.LogRow {
	if len(m.scopeFilter) == 0 && m.logQuery == "" {
		return logs
	}
	q := strings.ToLower(m.logQuery)
	out := make([]index.LogRow, 0, len(logs))
	for _, lg := range logs {
		who := lg.Unit
		if who == "" {
			who = lg.Entity
		}
		if !m.scopeMatches(appOf(who), lg.Unit) {
			continue
		}
		if q != "" && !logMatchesQuery(lg, q) {
			continue
		}
		out = append(out, lg)
	}
	return out
}

// logMatchesQuery reports whether lg's searchable fields contain the
// already-lowercased query q as a substring.
func logMatchesQuery(lg index.LogRow, q string) bool {
	return strings.Contains(strings.ToLower(lg.Body), q) ||
		strings.Contains(strings.ToLower(lg.Unit), q) ||
		strings.Contains(strings.ToLower(lg.Module), q) ||
		strings.Contains(strings.ToLower(lg.Level), q)
}

// lockedStreamIdx is the stream position the log pane centres on when it tracks
// the event cursor (the default, un-freed mode).
func (m *model) lockedStreamIdx() int {
	if m.cursor >= 0 && m.cursor < len(m.eventStreamIdx) {
		return m.eventStreamIdx[m.cursor]
	}
	return 0
}

func (m *model) renderLogPane() string {
	w := m.width
	_, h := rowHeights(m.bodyHeight())
	bodyH := max(1, h-2)

	lock := "following events"
	if m.logFree {
		lock = "free"
	}
	title := "Logs · " + lock
	if m.wrap {
		title += " · wrap"
	}
	if m.logQuery != "" {
		title += " · /" + m.logQuery
	}

	if len(m.stream) == 0 {
		return m.renderPane(paneLogs, w, h, title, []paneRow{row(styleDim.Render("(no events or logs)"))})
	}

	focus := m.lockedStreamIdx()
	if m.logFree {
		focus = m.logCursor
	}
	// Centre the focused item in the window.
	start := clamp(focus-bodyH/2, 0, max(0, len(m.stream)-bodyH))
	end := min(start+bodyH, len(m.stream))
	curSpan := m.currentEvent().spanID
	var rows []paneRow
	for i := start; i < end; i++ {
		rows = append(rows, m.streamLine(m.stream[i], i == focus, curSpan))
	}
	return m.renderPane(paneLogs, w, h, title, rows)
}

// streamLine renders one stream item. Event items become rulers; log items read
// "timestamp [source(unit)] level body", e.g. "… [k8s(grafana/0)] INFO ready".
// The selected line is marked with a leading "▸" rather than reversing the
// whole row, so the row's own colours (unit, level, …) stay legible.
func (m *model) streamLine(it streamItem, focused bool, curSpan string) paneRow {
	marker := " "
	switch {
	case focused:
		marker = styleHook.Render("▸")
	case it.evIdx < 0 && curSpan != "" && it.log.SpanID == curSpan:
		marker = styleHook.Render("»") // exact span-id join to the selected event
	}

	if it.evIdx >= 0 {
		ev := m.events[it.evIdx]
		ts := ev.ts.UTC().Format("15:04:05.000")
		label := styleText.Render(ev.summary)
		if ev.unit != "" {
			label = styleUnit.Render(ev.unit) + " " + label
		}
		return row(fmt.Sprintf("%s %s%s", marker, styleRuler.Render("══ "+ts+"  "), label))
	}
	lg := it.log
	who := lg.Unit
	if who == "" {
		who = lg.Entity
	}
	src := logSourceLabel(lg.Source)
	if who != "" {
		src = fmt.Sprintf("%s(%s)", src, who)
	}
	ts := lg.Ts.UTC().Format("15:04:05.000")

	// k8s logs echo their own timestamp inside the logline (Prometheus'
	// `ts=…`, avalanche's `hh:mm:ss`, …), a duplicate of ts above. Redact it
	// by default; verbose mode (`.`) shows the logline untouched.
	body := lg.Body
	if lg.Source == "k8s" && !m.verbose {
		body = recording.RedactK8sInlineTimestamp(body)
	}

	level := ""
	if lg.Level != "" {
		level = logLevelStyle(lg.Level).Render(lg.Level) + " "
	}
	return row(fmt.Sprintf("%s %s %s %s%s",
		marker,
		styleDim.Render(ts),
		styleSource.Render("["+src+"]"),
		level,
		styleText.Render(body)))
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

// logSourceLabel maps a raw log source to the short tag shown in the stream and
// inspector: k8s workload logs, juju's debug-log, machine journald.
func logSourceLabel(source string) string {
	switch source {
	case "debug-log", "":
		return "juju"
	case "journal":
		return "machine"
	default:
		return source // "k8s"
	}
}
