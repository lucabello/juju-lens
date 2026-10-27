package viewer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	tea "github.com/charmbracelet/bubbletea"
)

// opsFixtureModel opens the viewer on a unit-only lab recording: otelcol/1
// runs config-changed hooks, otelcol/0 a reconcile action.
func opsFixtureModel(t *testing.T) *model {
	t.Helper()
	spans, _, err := recording.LoadOps(filepath.Join("..", "recording", "testdata", "ops", "k8s"))
	if err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	for _, sp := range spans {
		if err := db.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan: %v", err)
		}
	}
	id, err := db.UpsertModel(spans[0].Model)
	if err != nil {
		t.Fatal(err)
	}
	return buildModel(t, db, index.Model{ID: id, Name: spans[0].Model})
}

func eventUnits(evs []event) map[string]int {
	out := map[string]int{}
	for _, ev := range evs {
		out[ev.unit+" "+ev.summary]++
	}
	return out
}

// TestSearchMatchesObserverNames: `/` finds the hooks whose charm-side tree
// reached an observer, and composes with the scope filter.
func TestSearchMatchesObserverNames(t *testing.T) {
	m := opsFixtureModel(t)
	if len(m.events) != 4 {
		t.Fatalf("%d hook rows from the unit-only recording, want 4: %v", len(m.events), eventUnits(m.events))
	}
	m.setSearchQuery(paneEvents, "kubernetescomputeresourcespatch")
	if got := eventUnits(m.events); len(got) != 1 || got["otelcol/1 config-changed"] != 3 {
		t.Errorf("observer search kept %v, want the 3 config-changed runs on otelcol/1", got)
	}
	m.setSearchQuery(paneEvents, "reconcile_action")
	if got := eventUnits(m.events); len(got) != 1 || got["otelcol/0 reconcile action"] != 1 {
		t.Errorf("event search kept %v, want the reconcile action", got)
	}
	m.scopeFilter = map[string]bool{"otelcol/1": true}
	m.applyFilters()
	if len(m.events) != 0 {
		t.Errorf("scope otelcol/1 and an otelcol/0 event: kept %v", eventUnits(m.events))
	}
}

// TestInspectorOpensOnTrace: Enter opens a hook from a unit-only recording on
// its trace, `.` shows the short calls and functions as well, and `d` shows
// what the hook changed instead.
func TestInspectorOpensOnTrace(t *testing.T) {
	m := opsFixtureModel(t)
	m.width, m.height = 160, 60
	m.overlay.Width, m.overlay.Height = 156, 1000 // renderOverlayFrame sizes it when the view draws
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if !m.overlayOn || m.inspMode != inspTrace {
		t.Fatalf("Enter opened overlay %v in mode %v, want the trace", m.overlayOn, m.inspMode)
	}
	compact := m.overlay.TotalLineCount()
	if !strings.Contains(m.overlay.View(), "KubernetesComputeResourcesPatch._on_config") {
		t.Errorf("trace lacks the observer:\n%s", m.overlay.View())
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(".")})
	m = updated.(*model)
	if full := m.overlay.TotalLineCount(); full <= compact+50 {
		t.Errorf(".: %d lines, compact %d", full, compact)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	m = updated.(*model)
	if m.inspMode != inspDetail {
		t.Errorf("d: mode %v, want what the hook changed", m.inspMode)
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(*model)
	if !m.overlayOn || m.inspMode != inspTrace {
		t.Error("esc from what it changed should go back to the trace")
	}
	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(*model)
	if m.overlayOn {
		t.Error("esc on the trace should close the inspector")
	}
}
