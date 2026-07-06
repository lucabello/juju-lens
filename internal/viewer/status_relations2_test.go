package viewer

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

// TestFocusCycleOrder checks Tab walks Events -> Status -> Logs and Shift+Tab
// reverses it.
func TestFocusCycleOrder(t *testing.T) {
	m := &model{keys: defaultKeymap(), focus: paneEvents}
	tab := tea.KeyMsg{Type: tea.KeyTab}
	want := []paneID{paneStatus, paneLogs, paneEvents}
	for i, w := range want {
		updated, _ := m.handleKey(tab)
		m = updated.(*model)
		if m.focus != w {
			t.Fatalf("Tab %d: focus = %d, want %d", i, m.focus, w)
		}
	}
	shiftTab := tea.KeyMsg{Type: tea.KeyShiftTab}
	wantBack := []paneID{paneLogs, paneStatus, paneEvents}
	for i, w := range wantBack {
		updated, _ := m.handleKey(shiftTab)
		m = updated.(*model)
		if m.focus != w {
			t.Fatalf("Shift+Tab %d: focus = %d, want %d", i, m.focus, w)
		}
	}
}

// TestRelationHiddenAfterBroken checks a relation whose databag lingers in the
// index disappears from the Status pane once its relation-broken hook has run as
// of the selected instant.
func TestRelationHiddenAfterBroken(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	if err := db.InsertSnapshot("default", base, "databag",
		"databag:grafana.gone#loki.gone:grafana/0", `{"z":"3"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSpan(recording.SpanRow{
		SpanID: "b1", Model: "default", Unit: "grafana/0", Hook: "relation-broken",
		Relation: "grafana.gone#loki.gone", Name: "Uniter.CommitHookChanges",
		Start: base.Add(5 * time.Second), End: base.Add(5 * time.Second), StatusCode: "OK",
		Attrs: map[string]string{"facade": "Uniter", "method": "CommitHookChanges"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSpan(hookSpan("e1", "grafana/0", "install", base.Add(time.Second))); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSpan(hookSpan("e2", "grafana/0", "update-status", base.Add(6*time.Second))); err != nil {
		t.Fatal(err)
	}
	modelID, _ := db.UpsertModel("default")
	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(*model)

	m.moveCursor(-1 << 30) // oldest event (before the break): relation present
	if !relationsContain(m.relations, "grafana.gone#loki.gone") {
		t.Errorf("relation should be present before relation-broken: %+v", m.relations)
	}
	m.moveCursor(1 << 30) // newest event (after the break): relation gone
	if relationsContain(m.relations, "grafana.gone#loki.gone") {
		t.Errorf("relation should be hidden after relation-broken: %+v", m.relations)
	}
}

// TestRelationReAddedAfterBroken checks a relation's existence follows a full
// remove-then-re-add cycle. Existence is decided by the newest databag write vs
// the newest relation-broken, so a relation created mid-recording (a later
// databag write, no earlier presence) reappears just like one that existed at
// bootstrap — the case that broke when we relied on relation-created hooks.
func TestRelationReAddedAfterBroken(t *testing.T) {
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	rel := "a.ep#b.ep"
	broken := func(at time.Duration) recording.SpanRow {
		return recording.SpanRow{Unit: "a/0", Hook: "relation-broken", Relation: rel, Start: base.Add(at)}
	}
	// Broken at +5s and +15s (given out of order to prove they get sorted).
	m := &model{relationBroken: relationBrokenTimes([]recording.SpanRow{
		broken(15 * time.Second), broken(5 * time.Second),
	})}
	rels := []relationSummary{{Key: rel}}
	// lastWrite is the newest databag write as of the cursor.
	cases := []struct {
		now, lastWrite time.Duration
		want           bool
		when           string
	}{
		{1 * time.Second, 1 * time.Second, true, "created mid-recording, before any break"},
		{7 * time.Second, 1 * time.Second, false, "after first break, no re-write"},
		{12 * time.Second, 10 * time.Second, true, "re-added: newer databag write"},
		{20 * time.Second, 10 * time.Second, false, "after second break"},
	}
	for _, c := range cases {
		lw := map[string]time.Time{rel: base.Add(c.lastWrite)}
		got := len(m.currentRelations(rels, lw, base.Add(c.now))) == 1
		if got != c.want {
			t.Errorf("%s: present=%v, want %v", c.when, got, c.want)
		}
	}
}

func relationsContain(rels []relationSummary, key string) bool {
	for _, r := range rels {
		if r.Key == key {
			return true
		}
	}
	return false
}

// TestRelationsHeaderNoANSILeak guards against the styleSection-over-styled-text
// corruption: with colour on, the focused Relations header must not leave
// literal "[90m"/"[0m" text after the escape sequences are stripped.
func TestRelationsHeaderNoANSILeak(t *testing.T) {
	// Force colour so styles emit real codes; restore the uncoloured default
	// afterwards so this global doesn't leak into other tests.
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	lipgloss.SetColorProfile(termenv.ANSI256)
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	if err := db.InsertSnapshot("default", base, "databag",
		"databag:grafana.grafana-source#prometheus.grafana-source:grafana/0", `{"x":"1"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSpan(hookSpan("e1", "grafana/0", "install", base)); err != nil {
		t.Fatal(err)
	}
	modelID, _ := db.UpsertModel("default")
	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 40})
	m = updated.(*model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}}) // focus Status
	m = updated.(*model)

	stripped := ansi.Strip(m.renderStatusPane())
	for _, bad := range []string{"[90m", "[0m", "[1;"} {
		if strings.Contains(stripped, bad) {
			t.Errorf("ANSI leaked into visible text (%q):\n%s", bad, stripped)
		}
	}
}
