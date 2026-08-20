package viewer

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

// TestDatabagInspectorLayout drives the inspector for a relation-changed hook on
// grafana/0 (a two-unit app) and checks the grouped layout — relation heading,
// then "app" and one "unit (…)" row per local unit — plus that only the unit the
// hook moved shows a diff and unchanged sides stay in normal text (not grey).
func TestDatabagInspectorLayout(t *testing.T) {
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
	lipgloss.SetColorProfile(termenv.ANSI256)

	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	commit := base.Add(time.Second)
	if err := db.InsertSpan(recording.SpanRow{
		SpanID: "h1", Model: "cos", Unit: "grafana/0", Hook: "grafana-source-relation-changed",
		Name: "Uniter.CommitHookChanges", Start: commit, End: commit.Add(300 * time.Millisecond),
		StatusCode: "OK", Attrs: map[string]string{"facade": "Uniter", "method": "CommitHookChanges"},
	}); err != nil {
		t.Fatal(err)
	}
	rel := "alertmanager.grafana-source#grafana.grafana-source"
	seed := func(scope, body string, ts time.Time, psid string) {
		if err := db.InsertSnapshot("cos", ts, "databag", scope, body, psid); err != nil {
			t.Fatal(err)
		}
	}
	seed("databag:"+rel+":grafana", `{"uid":"abc"}`, base, "old-app") // app, unchanged
	seed("databag:"+rel+":grafana/0", `{"addr":"10.0.0.1"}`, base, "old0")
	seed("databag:"+rel+":grafana/0", `{"addr":"10.0.0.2"}`, commit, "h1") // unit 0, changed
	seed("databag:"+rel+":grafana/1", `{"addr":"10.0.0.9"}`, base, "old1") // unit 1, unchanged
	seed("databag:"+rel+":alertmanager/0", `{"x":"y"}`, base, "rem")       // remote side, excluded
	modelID, _ := db.UpsertModel("cos")
	m := buildModel(t, db, index.Model{ID: modelID, Name: "cos"})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 44})
	m = updated.(*model)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(*model)
	if !m.overlayOn {
		t.Fatal("relation-changed with a databag change should be inspectable")
	}

	view := ansi.Strip(m.overlay.View())
	for _, want := range []string{
		"grafana:grafana-source → alertmanager:grafana-source", // local-first relation heading
		"    app",
		"    unit (grafana/0)",
		"    unit (grafana/1)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("default view missing %q\n%s", want, view)
		}
	}
	if strings.Contains(view, "alertmanager/0") {
		t.Errorf("remote-side databag should not be shown\n%s", view)
	}

	// Diff mode: only grafana/0 moved, so exactly it shows +/- lines.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = updated.(*model)
	raw := m.overlay.View() // keep ANSI to check colours
	if !strings.Contains(ansi.Strip(raw), `+   "addr": "10.0.0.2"`) ||
		!strings.Contains(ansi.Strip(raw), `-   "addr": "10.0.0.1"`) {
		t.Errorf("expected a diff for grafana/0\n%s", ansi.Strip(raw))
	}
	// The unchanged grafana/1 value must render in normal text, never dim (grey).
	dim := styleDim.Render(`  "addr": "10.0.0.9"`)
	if strings.Contains(raw, dim) {
		t.Errorf("unchanged databag line rendered dim; want normal text")
	}
}

func TestFormatDatabagLabel(t *testing.T) {
	cases := []struct {
		scope, rel string
		isApp      bool
	}{
		// local app already first: no swap.
		{"databag:grafana.grafana-source#prometheus.grafana-source:grafana/0",
			"grafana:grafana-source → prometheus:grafana-source", false},
		// local app is the second segment: it moves to the front.
		{"databag:ca.certificates#loki.certificates:loki/0",
			"loki:certificates → ca:certificates", false},
		// peer relation (single segment).
		{"databag:mimir.mimir-peers:mimir/0", "mimir:mimir-peers (peer)", false},
		// application databag (entity is a bare app name).
		{"databag:loki.certificates#ca.certificates:loki",
			"loki:certificates → ca:certificates", true},
	}
	for _, c := range cases {
		rel, isApp := formatDatabagLabel(c.scope)
		if rel != c.rel || isApp != c.isApp {
			t.Errorf("formatDatabagLabel(%q) = %q,%v; want %q,%v", c.scope, rel, isApp, c.rel, c.isApp)
		}
	}
}

// prettyJSON/lineDiff's own behaviour (nested JSON/YAML expansion, the LCS
// diff) is tested in internal/narrative/detail_test.go now that the logic
// lives there.
