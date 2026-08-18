package viewer

import (
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	tea "github.com/charmbracelet/bubbletea"
)

// twoAppFixture seeds grafana/0 and prometheus/0, each with an install hook
// and a log line, so scope-filter tests can assert on both Events and Logs.
func twoAppFixture(t *testing.T) *model {
	t.Helper()
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	for _, sp := range []recording.SpanRow{
		hookSpan("a1", "grafana/0", "install", base),
		hookSpan("a2", "prometheus/0", "install", base.Add(90*time.Millisecond)),
	} {
		if err := db.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan: %v", err)
		}
	}
	for _, lg := range []recording.LogRecord{
		{Ts: base.Add(150 * time.Millisecond), Model: "default", Source: "debug-log",
			Entity: "unit-grafana-0", Unit: "grafana/0", Level: "INFO",
			Module: "juju.worker.uniter.operation", Message: `ran "install" hook`},
		{Ts: base.Add(200 * time.Millisecond), Model: "default", Source: "debug-log",
			Entity: "unit-prometheus-0", Unit: "prometheus/0", Level: "INFO",
			Module: "juju.worker.uniter.operation", Message: "prometheus log line"},
	} {
		if err := db.InsertLog(lg); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})
	m.focus = paneStatus
	return m
}

// streamHasLogForUnit reports whether the built stream carries a log line for
// the given unit.
func streamHasLogForUnit(items []streamItem, unit string) bool {
	for _, it := range items {
		if it.evIdx < 0 && it.log.Unit == unit {
			return true
		}
	}
	return false
}

// TestStatusRowsOrder checks the Status pane's flat cursor list walks apps
// with their units nested beneath, in the same order the pane renders them.
func TestStatusRowsOrder(t *testing.T) {
	m := twoAppFixture(t)
	want := []statusRow{
		{kind: statusRowApp, key: "grafana"},
		{kind: statusRowUnit, key: "grafana/0"},
		{kind: statusRowApp, key: "prometheus"},
		{kind: statusRowUnit, key: "prometheus/0"},
	}
	if len(m.statusRows) != len(want) {
		t.Fatalf("statusRows = %+v, want %+v", m.statusRows, want)
	}
	for i, w := range want {
		if m.statusRows[i] != w {
			t.Fatalf("statusRows[%d] = %+v, want %+v", i, m.statusRows[i], w)
		}
	}
}

// TestScopeFilterPinAppScopesEventsAndLogs checks that pinning an app from
// the Status pane (space on its header row) narrows both Events and Logs to
// that app, and unpinning it (space again) clears the filter.
func TestScopeFilterPinAppScopesEventsAndLogs(t *testing.T) {
	m := twoAppFixture(t)
	m.statusCursor = 0 // the grafana app row

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(*model)

	if !m.scopeFilter["grafana"] {
		t.Fatalf("scopeFilter = %+v, want grafana pinned", m.scopeFilter)
	}
	if len(m.events) == 0 {
		t.Fatal("scope filter left no events visible")
	}
	for _, ev := range m.events {
		if ev.app != "grafana" {
			t.Fatalf("event for app %q leaked through the grafana scope filter", ev.app)
		}
	}
	if streamHasLogForUnit(m.stream, "prometheus/0") {
		t.Fatal("prometheus log leaked through the grafana scope filter")
	}
	if !streamHasLogForUnit(m.stream, "grafana/0") {
		t.Fatal("grafana's own log was filtered out by its own scope pin")
	}

	// Pressing space again on the same row unpins it.
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(*model)
	if m.scopeFilter["grafana"] {
		t.Fatal("second space did not unpin grafana")
	}
	if !streamHasLogForUnit(m.stream, "prometheus/0") {
		t.Fatal("prometheus log still hidden after the scope filter was cleared")
	}
}

// TestScopeFilterPinUnitOnlyScopesThatUnit checks that pinning a unit row
// (rather than its app header) narrows to just that unit, not its siblings.
func TestScopeFilterPinUnitOnlyScopesThatUnit(t *testing.T) {
	m := twoAppFixture(t)
	m.statusCursor = 1 // the grafana/0 unit row

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
	m = updated.(*model)

	if !m.scopeFilter["grafana/0"] {
		t.Fatalf("scopeFilter = %+v, want grafana/0 pinned", m.scopeFilter)
	}
	if m.scopeFilter["grafana"] {
		t.Fatal("pinning a unit row should not also pin its app")
	}
	for _, ev := range m.events {
		if ev.unit != "grafana/0" {
			t.Fatalf("event for unit %q leaked through the grafana/0 scope filter", ev.unit)
		}
	}
}

// TestResetFiltersClearsScopeAndSearch checks `r` resets the scope filter and
// both search queries at once.
func TestResetFiltersClearsScopeAndSearch(t *testing.T) {
	m := twoAppFixture(t)
	m.statusCursor = 0
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeySpace}) // pin grafana
	m = updated.(*model)
	if len(m.scopeFilter) == 0 {
		t.Fatal("setup: expected a pin before testing reset")
	}

	m.focus = paneEvents
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = updated.(*model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = updated.(*model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if m.eventQuery != "x" {
		t.Fatal("setup: expected a committed search before testing reset")
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	m = updated.(*model)
	if len(m.scopeFilter) != 0 {
		t.Fatalf("scopeFilter = %+v after 'r', want empty", m.scopeFilter)
	}
	if m.eventQuery != "" {
		t.Fatalf("eventQuery = %q after 'r', want empty", m.eventQuery)
	}
	if !streamHasLogForUnit(m.stream, "prometheus/0") {
		t.Fatal("prometheus log still hidden after resetting filters")
	}
}

// TestSearchFiltersFocusedPaneIndependently checks `/` opens a prompt scoped
// to whichever of Events/Logs is focused, filters live as you type, and each
// pane keeps its own query when focus moves away and a different search
// begins.
func TestSearchFiltersFocusedPaneIndependently(t *testing.T) {
	m := twoAppFixture(t)
	m.focus = paneEvents

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = updated.(*model)
	if !m.searchActive || m.searchTarget != paneEvents {
		t.Fatalf("search not opened on Events: active=%v target=%v", m.searchActive, m.searchTarget)
	}
	for _, r := range "prometheus" {
		updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(*model)
	}
	if m.eventQuery != "prometheus" {
		t.Fatalf("eventQuery = %q, want %q", m.eventQuery, "prometheus")
	}
	for _, ev := range m.events {
		if ev.app != "prometheus" {
			t.Fatalf("event for app %q survived the %q search", ev.app, m.eventQuery)
		}
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if m.searchActive {
		t.Fatal("enter should close the search prompt")
	}
	if m.eventQuery != "prometheus" {
		t.Fatal("enter should keep the committed query, not clear it")
	}

	// A fresh Logs search must not disturb the Events query still in effect.
	m.focus = paneLogs
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = updated.(*model)
	if m.searchTarget != paneLogs {
		t.Fatalf("searchTarget = %v, want paneLogs", m.searchTarget)
	}
	for _, r := range "grafana" {
		updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = updated.(*model)
	}
	if m.logQuery != "grafana" {
		t.Fatalf("logQuery = %q, want %q", m.logQuery, "grafana")
	}
	if m.eventQuery != "prometheus" {
		t.Fatalf("eventQuery clobbered by the Logs search: got %q", m.eventQuery)
	}
	if streamHasLogForUnit(m.stream, "prometheus/0") {
		t.Fatal("prometheus log survived the grafana Logs search")
	}
}

// TestSearchEscCancelsAndClears checks Esc closes the prompt and drops the
// in-progress query rather than committing it.
func TestSearchEscCancelsAndClears(t *testing.T) {
	m := twoAppFixture(t)
	m.focus = paneEvents

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = updated.(*model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = updated.(*model)
	if m.eventQuery != "x" {
		t.Fatalf("eventQuery = %q, want %q before Esc", m.eventQuery, "x")
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(*model)
	if m.searchActive {
		t.Fatal("esc should close the search prompt")
	}
	if m.eventQuery != "" {
		t.Fatalf("eventQuery = %q after Esc, want cleared", m.eventQuery)
	}
}

// TestScopeSearchVerboseCompose checks the scope filter, a search query and
// the verbose toggle all narrow the same event list together rather than one
// silently overriding another.
func TestScopeSearchVerboseCompose(t *testing.T) {
	m := twoAppFixture(t)
	m.statusCursor = 0                                        // grafana app row
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeySpace}) // pin grafana
	m = updated.(*model)

	m.focus = paneEvents
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = updated.(*model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("install")})
	m = updated.(*model)
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)

	if len(m.events) == 0 {
		t.Fatal("scope + search left nothing visible")
	}
	for _, ev := range m.events {
		if ev.app != "grafana" {
			t.Fatalf("event for app %q survived the grafana scope filter", ev.app)
		}
		if !matchesEventQuery(ev, "install") {
			t.Fatalf("event %q survived the %q search", ev.summary, "install")
		}
	}
}
