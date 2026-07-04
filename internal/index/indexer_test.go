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
