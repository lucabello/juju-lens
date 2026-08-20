package viewer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/index"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
)

func openDB(t *testing.T) *index.DB {
	t.Helper()
	db, err := index.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// buildModel skips Run() so tests can inject a DB.
func buildModel(t *testing.T, db *index.DB, active index.Model) *model {
	m := &model{
		dir:         t.TempDir(),
		db:          db,
		overlay:     viewport.New(0, 0),
		help:        help.New(),
		keys:        defaultKeymap(),
		focus:       paneEvents,
		searchInput: textinput.New(),
	}
	m.setActiveModel(active)
	return m
}

func TestStatusPaneLatestKnownReflectsSnapshots(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	// grafana/0 goes maintenance → active; only "active" should surface.
	if err := db.InsertSnapshot("default", base, "unit-status",
		"unit-status:grafana/0",
		`{"value":"maintenance","message":"starting","since":"2026-07-03T14:30:12Z"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSnapshot("default", base.Add(time.Second), "unit-status",
		"unit-status:grafana/0",
		`{"value":"active","message":"Ready","since":"2026-07-03T14:30:13Z"}`, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSnapshot("default", base, "app-status",
		"app-status:grafana",
		`{"value":"active","message":"All units ready","since":"2026-07-03T14:30:12Z"}`, ""); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatal(err)
	}

	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})

	if got := m.appStatuses["grafana"].Value; got != "active" {
		t.Errorf("app-status grafana = %q, want active", got)
	}
	if got := m.unitStatuses["grafana/0"].Value; got != "active" {
		t.Errorf("unit-status grafana/0 = %q, want active (latest snapshot)", got)
	}
	if got := m.unitStatuses["grafana/0"].Message; got != "Ready" {
		t.Errorf("unit-status message = %q, want Ready", got)
	}
	if !m.unitStatuses["grafana/0"].Known {
		t.Errorf("known must be true when a snapshot exists")
	}
}

// TestStatusPaneExplicitAppStatusOverridesRollup covers the leader calling
// status-set --application (e.g. mimir/2, captured as an rpc-origin
// app-status snapshot): that explicit value must win over the unit-derived
// rollup, even though a unit workload status is known. Before the fix,
// deriveAppStatuses always took precedence and the explicit call was
// silently discarded.
func TestStatusPaneExplicitAppStatusOverridesRollup(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	if err := db.InsertSnapshot("default", base, "unit-status",
		"unit-status:mimir/2",
		`{"value":"active","message":"Ready","since":"2026-07-03T14:30:12Z"}`, ""); err != nil {
		t.Fatal(err)
	}
	// The leader's explicit application-status call, attributed under the
	// bare app name (extract.go collapses the leader unit tag).
	if err := db.InsertSnapshot("default", base.Add(time.Second), "app-status",
		"app-status:mimir",
		`{"value":"waiting","message":"waiting for compactor","since":"2026-07-03T14:30:13Z"}`, "sp1"); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatal(err)
	}

	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})

	if got := m.appStatuses["mimir"].Value; got != "waiting" {
		t.Errorf("app-status mimir = %q, want waiting (explicit rpc snapshot should win)", got)
	}
	if got := m.appStatuses["mimir"].Message; got != "waiting for compactor" {
		t.Errorf("app-status mimir message = %q, want %q", got, "waiting for compactor")
	}
}

// TestStatusPaneBootstrapAppStatusDoesNotOverrideRollup covers the opposite:
// the app-status snapshot seeded once from `juju status` at recording start
// (origin "bootstrap") must not freeze the app row — the live unit rollup
// still wins once a unit status is known.
func TestStatusPaneBootstrapAppStatusDoesNotOverrideRollup(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	if err := db.InsertBootstrapSnapshot("default", base, "app-status",
		"app-status:mimir",
		`{"value":"waiting","message":"starting up","since":"2026-07-03T14:30:12Z"}`); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSnapshot("default", base.Add(time.Second), "unit-status",
		"unit-status:mimir/2",
		`{"value":"active","message":"Ready","since":"2026-07-03T14:30:13Z"}`, ""); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatal(err)
	}

	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})

	if got := m.appStatuses["mimir"].Value; got != "active" {
		t.Errorf("app-status mimir = %q, want active (unit rollup should win over the frozen bootstrap value)", got)
	}
}

func TestStatusPaneShowsLeader(t *testing.T) {
	db := openDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	for _, s := range []struct{ kind, scope, body string }{
		{"unit-status", "unit-status:grafana/0", `{"value":"active","since":"2026-07-03T14:30:12Z"}`},
		{"unit-status", "unit-status:grafana/1", `{"value":"active","since":"2026-07-03T14:30:12Z"}`},
		{"leadership", "leadership:grafana", `{"value":"grafana/1","since":"2026-07-03T14:30:12Z"}`},
	} {
		if err := db.InsertBootstrapSnapshot("default", base, s.kind, s.scope, s.body); err != nil {
			t.Fatal(err)
		}
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatal(err)
	}
	m := buildModel(t, db, index.Model{ID: modelID, Name: "default"})

	if got := m.leaders["grafana"]; got != "grafana/1" {
		t.Fatalf("leader for grafana = %q, want grafana/1", got)
	}
	rows := m.unitRows("grafana", "grafana/1", "", false)
	joined := ""
	for _, r := range rows {
		joined += r.text + "\n"
	}
	if !strings.Contains(joined, "grafana/1*") {
		t.Fatalf("leader unit is not marked with a star:\n%s", joined)
	}
}
