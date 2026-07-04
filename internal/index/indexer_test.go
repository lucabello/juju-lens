package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/wire"
)

func TestIndexerIncremental(t *testing.T) {
	dir := t.TempDir()
	layout := recording.NewLayout(dir)
	if err := layout.Init(); err != nil {
		t.Fatal(err)
	}
	callsPath := layout.RPCFileFor("m")("2026-07-04T14")
	if err := os.MkdirAll(filepath.Dir(callsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	appendLine := func(cm wire.CapturedMessage) {
		line, _ := cm.MarshalLine()
		f, err := os.OpenFile(callsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(line)
		f.Write([]byte("\n"))
		f.Close()
	}
	base := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	req := func(rid uint64, method, params string) wire.CapturedMessage {
		return wire.CapturedMessage{
			Ts: base, PID: 100, Dir: wire.DirWrite, Conn: 1, Model: "m", Unit: "a/0",
			Msg: wire.Envelope{RequestID: rid, Type: "Uniter", Request: method, Params: json.RawMessage(params)},
		}
	}
	resp := func(rid uint64) wire.CapturedMessage {
		return wire.CapturedMessage{
			Ts: base.Add(5 * time.Millisecond), PID: 100, Dir: wire.DirRead, Conn: 1, Model: "m", Unit: "a/0",
			Msg: wire.Envelope{RequestID: rid, Response: json.RawMessage(`{}`)},
		}
	}

	db := openTempDB(t)
	ix := NewIndexer(db)

	// Sync 1: only the request is present — it's buffered, no span yet.
	appendLine(req(1, "SetUnitStatus", `{"entities":[{"tag":"unit-a-0","status":"active"}]}`))
	if err := ix.Sync(dir); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.SpanCount(); c != 0 {
		t.Fatalf("after request-only sync, span count = %d, want 0", c)
	}

	// Sync 2: the response arrives in a later chunk and must pair across Syncs.
	appendLine(resp(1))
	if err := ix.Sync(dir); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.SpanCount(); c != 1 {
		t.Fatalf("after response sync, span count = %d, want 1", c)
	}
	// The status snapshot must have been extracted too.
	models, _ := db.Models()
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	rows, _ := db.LatestPerScope(models[0].ID, "unit-status")
	if len(rows) != 1 || rows[0].Scope != "unit-status:a/0" {
		t.Fatalf("unit-status snapshot missing: %+v", rows)
	}

	// Sync 3: nothing new; offsets must prevent reprocessing (idempotent).
	if err := ix.Sync(dir); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.SpanCount(); c != 1 {
		t.Fatalf("idempotent sync changed span count to %d, want 1", c)
	}
}

// TestIndexerLogTrees checks the incremental indexer ingests all three log
// trees, including the deeper raw/k8s/<model>/<pod>/<container>/ layout, and
// does so idempotently across Syncs.
func TestIndexerLogTrees(t *testing.T) {
	dir := t.TempDir()
	layout := recording.NewLayout(dir)
	if err := layout.Init(); err != nil {
		t.Fatal(err)
	}
	hour := "2026-07-04T19"
	appendLine := func(pathFor func(string) string, line string) {
		p := pathFor(hour)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(line + "\n")
		f.Close()
	}

	db := openTempDB(t)
	ix := NewIndexer(db)

	appendLine(layout.JujuLogFileFor("cos-lite"),
		`unit-loki-0: 2026-07-04 19:04:24.406 INFO juju.worker.uniter ran hook`)
	appendLine(layout.K8sLogFileFor("cos-lite", "grafana-0", "grafana"),
		`2026-07-04T19:13:25.277Z [grafana] ready`)
	appendLine(layout.MachineLogFileFor("otelcol-scrape-test", "machine-0"),
		`{"__REALTIME_TIMESTAMP":"1783192466043395","PRIORITY":"6","SYSLOG_IDENTIFIER":"systemd","MESSAGE":"up"}`)

	if err := ix.Sync(dir); err != nil {
		t.Fatal(err)
	}
	got := logCountsBySource(t, db)
	want := map[string]int{"debug-log": 1, "k8s": 1, "journal": 1}
	for src, n := range want {
		if got[src] != n {
			t.Errorf("source %q count = %d, want %d (all: %v)", src, got[src], n, got)
		}
	}

	// A second Sync with no new bytes must not duplicate rows.
	if err := ix.Sync(dir); err != nil {
		t.Fatal(err)
	}
	if got := logCountsBySource(t, db); got["k8s"] != 1 || got["journal"] != 1 || got["debug-log"] != 1 {
		t.Errorf("idempotent sync changed counts: %v", got)
	}

	// A newly-appended k8s line (same nested file) is picked up on the next Sync.
	appendLine(layout.K8sLogFileFor("cos-lite", "grafana-0", "grafana"),
		`2026-07-04T19:13:26.000Z [grafana] tick`)
	if err := ix.Sync(dir); err != nil {
		t.Fatal(err)
	}
	if got := logCountsBySource(t, db); got["k8s"] != 2 {
		t.Errorf("k8s count after append = %d, want 2", got["k8s"])
	}
}

func logCountsBySource(t *testing.T, db *DB) map[string]int {
	t.Helper()
	rows, err := db.SQL().Query(`SELECT source, count(*) FROM log_records GROUP BY source`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var src string
		var n int
		if err := rows.Scan(&src, &n); err != nil {
			t.Fatal(err)
		}
		out[src] = n
	}
	return out
}
