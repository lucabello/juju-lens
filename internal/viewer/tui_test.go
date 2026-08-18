package viewer

import (
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// hookSpan builds a CommitHookChanges span the classifier turns into a hook
// event (facade/method live in Attrs; a non-empty Hook makes it an event).
func hookSpan(id, unit, hook string, ts time.Time) recording.SpanRow {
	return recording.SpanRow{
		SpanID:     id,
		Model:      "default",
		Unit:       unit,
		Hook:       hook,
		Name:       "Uniter.CommitHookChanges",
		Start:      ts,
		End:        ts.Add(200 * time.Millisecond),
		StatusCode: "OK",
		Attrs:      map[string]string{"facade": "Uniter", "method": "CommitHookChanges"},
	}
}

// seedRecording populates a DB with the shape a synth trivial recording has:
// two units with hooks, a merged log line, and a bootstrap-only app (traefik)
// known solely from an M8 status snapshot.
func seedRecording(t *testing.T, db *index.DB) index.Model {
	t.Helper()
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	for _, sp := range []recording.SpanRow{
		hookSpan("a1", "grafana/0", "install", base),
		hookSpan("a2", "prometheus/0", "install", base.Add(90*time.Millisecond)),
		hookSpan("a3", "grafana/0", "grafana-source-relation-joined", base.Add(1500*time.Millisecond)),
	} {
		if err := db.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan: %v", err)
		}
	}
	if err := db.InsertLog(recording.LogRecord{
		Ts: base.Add(300 * time.Millisecond), Model: "default", Source: "debug-log",
		Entity: "unit-grafana-0", Unit: "grafana/0", Level: "INFO",
		Module: "juju.worker.uniter.operation", Message: `ran "install" hook`,
	}); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	// traefik is bootstrap-only: no spans, no logs, just a status snapshot.
	if err := db.InsertBootstrapSnapshot("default", base.Add(-time.Millisecond),
		"app-status", "app-status:traefik",
		`{"value":"active","message":"","since":"2026-07-03T14:30:11Z"}`); err != nil {
		t.Fatalf("InsertBootstrapSnapshot: %v", err)
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	return index.Model{ID: modelID, Name: "default"}
}

// TestThreeColumnRender drives the full render path: sizing the window and
// rendering all three columns without panicking, with each pane's title and the
// bootstrap-only app present.
func TestThreeColumnRender(t *testing.T) {
	db := openDB(t)
	active := seedRecording(t, db)
	m := buildModel(t, db, active)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(*model)

	view := m.View()
	for _, want := range []string{"Events", "Logs", "Status", "traefik", "install", "relation-joined"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing %q\n---\n%s", want, view)
		}
	}
}

// TestEventsShowsFullUnitName guards that a long unit name (longer than the old
// 12-char column) is rendered in full in the Events pane.
func TestEventsShowsFullUnitName(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	if err := db.InsertSpan(hookSpan("a1", "alertmanager/0", "install", base)); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatal(err)
	}
	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(*model)

	if got := m.View(); !strings.Contains(got, "alertmanager/0") {
		t.Errorf("Events pane truncated the unit name; want full \"alertmanager/0\"\n---\n%s", got)
	}
}

// TestMainViewNoWidthOverflow guards the three-pane layout (and the footer)
// against spilling past the terminal edge at a range of widths.
func TestMainViewNoWidthOverflow(t *testing.T) {
	db := openDB(t)
	active := seedRecording(t, db)
	m := buildModel(t, db, active)
	for _, w := range []int{60, 80, 100, 160} {
		updated, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		m = updated.(*model)
		for _, line := range strings.Split(m.View(), "\n") {
			if got := ansi.StringWidth(line); got > w {
				t.Fatalf("width %d: line width %d exceeds terminal: %q", w, got, line)
			}
		}
	}
}

// TestInspectorNoWidthOverflow guards the overlay against spilling past the
// terminal edge: even with a very long RPC attribute, every rendered line must
// fit within the window width.
func TestInspectorNoWidthOverflow(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	sp := hookSpan("a1", "grafana/0", "config-changed", base)
	sp.Attrs["params"] = strings.Repeat("abcdefghij", 60) // 600 cols, no spaces
	if err := db.InsertSpan(sp); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatal(err)
	}
	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})
	const width = 90
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
	m = updated.(*model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)

	for _, line := range strings.Split(m.View(), "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Fatalf("overlay line width %d exceeds terminal width %d: %q", w, width, line)
		}
	}
}

// TestHorizontalScrollClamps checks a pane cannot be scrolled right past its
// content: with a long log line, scrolling stops at the point the line's tail is
// visible and no further, and stays there however many more presses arrive.
func TestHorizontalScrollClamps(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	tail := "END-OF-LINE-MARKER"
	body := strings.Repeat("wide-", 60) + tail // ~318 cols, well past any pane
	if err := db.InsertLog(recording.LogRecord{
		Ts: base, Model: "default", Source: "debug-log",
		Unit: "grafana/0", Level: "INFO", Module: "x", Message: body,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSpan(hookSpan("a1", "grafana/0", "install", base)); err != nil {
		t.Fatal(err)
	}
	modelID, _ := db.UpsertModel("default")
	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(*model)
	tab := tea.KeyMsg{Type: tea.KeyTab}
	updated, _ = m.Update(tab) // Events -> Status
	m = updated.(*model)
	updated, _ = m.Update(tab) // Status -> Logs
	m = updated.(*model)

	right := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}}
	for i := 0; i < 100; i++ {
		updated, _ = m.Update(right)
		m = updated.(*model)
		m.View() // render applies the clamp
	}
	settled := m.hScroll
	if settled == 0 {
		t.Fatal("expected a positive scroll offset for an over-wide line")
	}
	// Further presses must not advance past the clamp.
	updated, _ = m.Update(right)
	m = updated.(*model)
	m.View()
	if m.hScroll != settled {
		t.Errorf("scroll advanced past content: %d then %d", settled, m.hScroll)
	}
	// The line's tail is now on screen, and there is no blank overscroll: the
	// clamp lands exactly where the widest line ends.
	if !strings.Contains(ansi.Strip(m.View()), tail) {
		t.Errorf("scrolled right but the line tail %q is not visible", tail)
	}
}

// TestInspectorOverlay verifies enter opens the inspector over the body and esc
// closes it back to the three columns.
func TestInspectorOverlay(t *testing.T) {
	db := openDB(t)
	active := seedRecording(t, db)
	// Make the newest event (span a3) inspectable by giving it a databag change.
	if err := db.InsertSnapshot("default", time.Date(2026, 7, 3, 14, 30, 13, 0, time.UTC),
		"databag", "databag:grafana.grafana-source#prometheus.grafana-source:grafana/0",
		`{"addr":"10.0.0.1"}`, "a3"); err != nil {
		t.Fatal(err)
	}
	m := buildModel(t, db, active)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	m = updated.(*model)
	// The cursor opens on the first event; step to the last (span a3, the one
	// made inspectable above) before inspecting it.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = updated.(*model)

	enter := tea.KeyMsg{Type: tea.KeyEnter}
	updated, _ = m.Update(enter)
	m = updated.(*model)
	if !m.overlayOn {
		t.Fatal("enter did not open the inspector overlay")
	}
	if got := m.View(); !strings.Contains(got, "Inspector") {
		t.Errorf("overlay View() missing %q\n---\n%s", "Inspector", got)
	}

	esc := tea.KeyMsg{Type: tea.KeyEsc}
	updated, _ = m.Update(esc)
	m = updated.(*model)
	if m.overlayOn {
		t.Fatal("esc did not close the inspector overlay")
	}
	if got := m.View(); !strings.Contains(got, "Events") {
		t.Errorf("after close, View() should show the columns again\n---\n%s", got)
	}
}
