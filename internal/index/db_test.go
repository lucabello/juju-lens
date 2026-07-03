package index

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

func openTempDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestUpsertModelIsIdempotent(t *testing.T) {
	db := openTempDB(t)
	a, err := db.UpsertModel("prod")
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.UpsertModel("prod")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("upsert must return the same id: got %d then %d", a, b)
	}
	// A distinct name gets a new row.
	c, err := db.UpsertModel("staging")
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatalf("expected different ids for different names")
	}
	models, err := db.Models()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("want 2 models, got %d", len(models))
	}
}

func TestSpanRoundTrip(t *testing.T) {
	db := openTempDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	rows := []recording.SpanRow{
		{
			TraceID: "aa", SpanID: "01", Model: "default",
			Start: base, End: base.Add(100 * time.Millisecond),
			Name: "uniter.RunOperation: install",
			Unit: "grafana/0", Service: "jujud",
			Hook:  "install",
			Attrs: map[string]string{"juju.hook": "install"},
		},
		{
			TraceID: "aa", SpanID: "02", ParentSpanID: "01", Model: "default",
			Start: base.Add(50 * time.Millisecond), End: base.Add(80 * time.Millisecond),
			Name: "hook: install",
			Unit: "grafana/0",
			Hook: "install",
		},
	}
	for _, r := range rows {
		if err := db.InsertSpan(r); err != nil {
			t.Fatalf("InsertSpan(%s): %v", r.SpanID, err)
		}
	}

	n, err := db.SpanCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 spans, got %d", n)
	}

	// Insert-or-update semantics: re-insert with a new name should replace.
	rows[0].Name = "renamed"
	if err := db.InsertSpan(rows[0]); err != nil {
		t.Fatalf("re-insert: %v", err)
	}

	got, err := db.Spans(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	// Rows come back start-ordered; the first is span 01 (renamed).
	if got[0].SpanID != "01" || got[0].Name != "renamed" {
		t.Fatalf("row[0] = %+v, want span 01 renamed", got[0])
	}
	if got[0].Unit != "grafana/0" || got[0].Hook != "install" {
		t.Fatalf("row[0] lost unit/hook: %+v", got[0])
	}
	if got[0].Attrs["juju.hook"] != "install" {
		t.Fatalf("row[0] attrs missing: %+v", got[0].Attrs)
	}
	// Child span carries the parent id.
	if got[1].ParentSpanID != "01" {
		t.Fatalf("row[1] missing parent: %+v", got[1])
	}
	if !got[0].Start.Equal(base) {
		t.Fatalf("row[0] start wrong: %v", got[0].Start)
	}
}

func TestSpansFilteredByModel(t *testing.T) {
	db := openTempDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	if err := db.InsertSpan(recording.SpanRow{
		TraceID: "aa", SpanID: "a1", Model: "prod",
		Start: base, End: base, Name: "prod-span",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSpan(recording.SpanRow{
		TraceID: "aa", SpanID: "b1", Model: "staging",
		Start: base, End: base, Name: "staging-span",
	}); err != nil {
		t.Fatal(err)
	}
	prod, err := db.UpsertModel("prod")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.Spans(prod)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Model != "prod" {
		t.Fatalf("want single prod span, got %+v", got)
	}
}

func TestSnapshotLatest(t *testing.T) {
	db := openTempDB(t)
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	// Two snapshots for grafana at t and t+1s; expect the later to win.
	if err := db.InsertSnapshot("default", base, "unit-status", "unit-status:grafana/0",
		`{"value":"maintenance","message":"starting"}`, "sp1"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSnapshot("default", base.Add(time.Second), "unit-status", "unit-status:grafana/0",
		`{"value":"active","message":"Ready"}`, "sp2"); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertSnapshot("default", base, "unit-status", "unit-status:prometheus/0",
		`{"value":"waiting"}`, "sp3"); err != nil {
		t.Fatal(err)
	}
	modelID, err := db.UpsertModel("default")
	if err != nil {
		t.Fatal(err)
	}

	// Before t: no snapshot for grafana.
	if s, err := db.LatestSnapshotBefore(modelID, "unit-status:grafana/0", base.Add(-time.Second)); err != nil {
		t.Fatal(err)
	} else if s != "" {
		t.Fatalf("expected empty snapshot, got %q", s)
	}
	// At t: the "maintenance" snapshot.
	if s, err := db.LatestSnapshotBefore(modelID, "unit-status:grafana/0", base); err != nil {
		t.Fatal(err)
	} else if s == "" || s[:20] != `{"value":"maintenanc` {
		t.Fatalf("wrong snapshot at t: %q", s)
	}
	// At t+2s: the later "active" snapshot.
	if s, err := db.LatestSnapshotBefore(modelID, "unit-status:grafana/0", base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	} else if s == "" || s[:15] != `{"value":"activ` {
		t.Fatalf("wrong snapshot at t+2s: %q", s)
	}

	// LatestPerScope returns exactly two rows, most recent per scope.
	rows, err := db.LatestPerScope(modelID, "unit-status")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 latest-per-scope rows, got %d", len(rows))
	}
	byScope := map[string]string{}
	for _, r := range rows {
		byScope[r.Scope] = r.Body
	}
	if byScope["unit-status:grafana/0"][:15] != `{"value":"activ` {
		t.Fatalf("grafana row is not the latest: %q", byScope["unit-status:grafana/0"])
	}
	if byScope["unit-status:prometheus/0"][:15] != `{"value":"waiti` {
		t.Fatalf("prometheus row wrong: %q", byScope["unit-status:prometheus/0"])
	}
}
