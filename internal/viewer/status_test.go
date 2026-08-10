package viewer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/index"

	"github.com/charmbracelet/bubbles/help"
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
		dir:     t.TempDir(),
		db:      db,
		overlay: viewport.New(0, 0),
		help:    help.New(),
		keys:    defaultKeymap(),
		focus:   paneEvents,
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
	rows := m.unitRows("grafana", "grafana/1", "")
	joined := ""
	for _, r := range rows {
		joined += r.text + "\n"
	}
	if !strings.Contains(joined, "grafana/1*") {
		t.Fatalf("leader unit is not marked with a star:\n%s", joined)
	}
}
