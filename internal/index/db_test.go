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

// countSnapshots returns how many snapshot rows exist for a scope.
func countSnapshots(t *testing.T, db *DB, scope string) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM snapshots WHERE scope = ?`, scope).Scan(&n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	return n
}

// TestSnapshotDedup covers M7: a re-committed identical body records nothing,
// but a changed body (even a revert to an earlier value) records a new row.
func TestSnapshotDedup(t *testing.T) {
	db := openTempDB(t)
	base := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const scope = "databag:a.b#c.d:prometheus/0"
	ins := func(sec int, body string) {
		if err := db.InsertSnapshot("m", base.Add(time.Duration(sec)*time.Second),
			"databag", scope, body, "sp"); err != nil {
			t.Fatalf("InsertSnapshot: %v", err)
		}
	}
	ins(0, `{"addr":"1.1.1.1"}`) // first value -> recorded
	ins(1, `{"addr":"1.1.1.1"}`) // identical re-commit -> deduped
	ins(2, `{"addr":"1.1.1.1"}`) // identical again -> deduped
	if got := countSnapshots(t, db, scope); got != 1 {
		t.Fatalf("identical re-commits: want 1 snapshot, got %d", got)
	}
	ins(3, `{"addr":"2.2.2.2"}`) // real change -> recorded
	if got := countSnapshots(t, db, scope); got != 2 {
		t.Fatalf("after a real change: want 2 snapshots, got %d", got)
	}
	ins(4, `{"addr":"1.1.1.1"}`) // revert to an earlier value -> still a change
	if got := countSnapshots(t, db, scope); got != 3 {
		t.Fatalf("after reverting to an earlier value: want 3 snapshots, got %d", got)
	}
}

// TestBootstrapThenIdenticalDelta covers M7+M8: a bootstrap snapshot at t0
// followed by an RPC delta asserting the same value dedups, so point-in-time
// still resolves to the bootstrap baseline rather than 'unknown'.
func TestBootstrapThenIdenticalDelta(t *testing.T) {
	db := openTempDB(t)
	t0 := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const scope = "unit-status:grafana/0"
	if err := db.InsertBootstrapSnapshot("m", t0, "unit-status", scope, `{"value":"active"}`); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// A later RPC re-asserts "active": no new row.
	if err := db.InsertSnapshot("m", t0.Add(time.Minute), "unit-status", scope, `{"value":"active"}`, "sp1"); err != nil {
		t.Fatalf("delta: %v", err)
	}
	if got := countSnapshots(t, db, scope); got != 1 {
		t.Fatalf("identical delta after bootstrap: want 1 snapshot, got %d", got)
	}
	modelID, _ := db.UpsertModel("m")
	rows, err := db.LatestPerScopeAsOf(modelID, "unit-status", t0.Add(30*time.Second))
	if err != nil {
		t.Fatalf("LatestPerScopeAsOf: %v", err)
	}
	if len(rows) != 1 || rows[0].Body != `{"value":"active"}` {
		t.Fatalf("point-in-time did not resolve to bootstrap baseline: %+v", rows)
	}
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

func TestLogsForSpanCorrelation(t *testing.T) {
	db := openTempDB(t)
	base := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	mustLog := func(rec recording.LogRecord) {
		t.Helper()
		if err := db.InsertLog(rec); err != nil {
			t.Fatal(err)
		}
	}
	// grafana/0 lines: two within the window, one far outside.
	mustLog(recording.LogRecord{Model: "m", Ts: base.Add(-2 * time.Second), Entity: "unit-grafana-0", Unit: "grafana/0", Level: "INFO", Message: "before"})
	mustLog(recording.LogRecord{Model: "m", Ts: base.Add(1 * time.Second), Entity: "unit-grafana-0", Unit: "grafana/0", Level: "INFO", Message: "during"})
	mustLog(recording.LogRecord{Model: "m", Ts: base.Add(1 * time.Hour), Entity: "unit-grafana-0", Unit: "grafana/0", Level: "INFO", Message: "way later"})
	// A different unit inside the window must NOT correlate by window.
	mustLog(recording.LogRecord{Model: "m", Ts: base, Entity: "unit-prometheus-0", Unit: "prometheus/0", Level: "INFO", Message: "other unit"})
	// A line carrying the span id must correlate even out of window / other unit.
	mustLog(recording.LogRecord{Model: "m", Ts: base.Add(2 * time.Hour), Entity: "unit-prometheus-0", Unit: "prometheus/0", Level: "DEBUG", SpanID: "abc123", Message: "exact span match"})

	// Span: grafana/0, 200ms around base, id abc123.
	logs, err := db.LogsForSpan(0, "abc123", "grafana/0", base, base.Add(200*time.Millisecond), 5*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range logs {
		got[l.Body] = l.Matched
	}
	if _, ok := got["before"]; !ok {
		t.Errorf("expected 'before' (window) to correlate")
	}
	if _, ok := got["during"]; !ok {
		t.Errorf("expected 'during' (window) to correlate")
	}
	if got["exact span match"] != "span" {
		t.Errorf("expected span-id line marked 'span', got %q", got["exact span match"])
	}
	if got["during"] != "window" {
		t.Errorf("expected same-unit window line marked 'window', got %q", got["during"])
	}
	if _, ok := got["way later"]; ok {
		t.Errorf("out-of-window same-unit line must not correlate")
	}
	if _, ok := got["other unit"]; ok {
		t.Errorf("in-window different-unit line must not correlate")
	}
}

func TestPointInTimeAndDatabagQueries(t *testing.T) {
	db := openTempDB(t)
	base := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	// Two unit-status snapshots for grafana/0 (waiting -> active) and one for
	// prometheus/0 that only appears later.
	_ = db.InsertSnapshot("m", base, "unit-status", "unit-status:grafana/0", `{"value":"waiting"}`, "sp1")
	_ = db.InsertSnapshot("m", base.Add(10*time.Second), "unit-status", "unit-status:grafana/0", `{"value":"active"}`, "sp2")
	_ = db.InsertSnapshot("m", base.Add(20*time.Second), "unit-status", "unit-status:prometheus/0", `{"value":"active"}`, "sp3")
	// A databag written twice (diff source).
	_ = db.InsertSnapshot("m", base, "databag", "databag:a.b#c.d:grafana/0", `{"addr":"1.1.1.1"}`, "spA")
	_ = db.InsertSnapshot("m", base.Add(30*time.Second), "databag", "databag:a.b#c.d:grafana/0", `{"addr":"2.2.2.2"}`, "spB")

	mid, _ := db.Models() // model id
	modelID := mid[0].ID

	// As of base+5s: grafana/0 is still "waiting", prometheus not present yet.
	rows, err := db.LatestPerScopeAsOf(modelID, "unit-status", base.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Scope != "unit-status:grafana/0" {
		t.Fatalf("as-of+5s: want only grafana/0, got %+v", rows)
	}
	if rows[0].Body != `{"value":"waiting"}` {
		t.Errorf("as-of+5s grafana body = %s, want waiting", rows[0].Body)
	}
	// As of base+25s: both units present, grafana now active.
	rows, _ = db.LatestPerScopeAsOf(modelID, "unit-status", base.Add(25*time.Second))
	if len(rows) != 2 {
		t.Fatalf("as-of+25s: want 2 scopes, got %d", len(rows))
	}

	// Databag diff source: the span spB wrote the second value; the previous
	// value (before spB's ts) is the first.
	snaps, _ := db.SnapshotsByProducingSpan("spB", "databag")
	if len(snaps) != 1 || snaps[0].Body != `{"addr":"2.2.2.2"}` {
		t.Fatalf("SnapshotsByProducingSpan(spB) = %+v", snaps)
	}
	prev, _ := db.PrevSnapshotBefore(modelID, "databag:a.b#c.d:grafana/0", base.Add(30*time.Second))
	if prev != `{"addr":"1.1.1.1"}` {
		t.Errorf("prev databag = %s, want 1.1.1.1", prev)
	}
}
