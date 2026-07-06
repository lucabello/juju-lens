package viewer

import (
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	tea "github.com/charmbracelet/bubbletea"
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

// TestInspectorOverlay verifies enter opens the inspector over the body and esc
// closes it back to the three columns.
func TestInspectorOverlay(t *testing.T) {
	db := openDB(t)
	active := seedRecording(t, db)
	m := buildModel(t, db, active)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
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
