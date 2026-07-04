package recording

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/wire"
)

func TestLoadSpansMissingDirReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadSpans(dir)
	if err != nil {
		t.Fatalf("LoadSpans on empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(got))
	}
}

// writeCalls appends the given captured messages to a model's calls file.
func writeCalls(t *testing.T, l Layout, model string, hourTs time.Time, msgs ...wire.CapturedMessage) {
	t.Helper()
	w := NewRotatingWriter(l.RPCFileFor(model), func() time.Time { return hourTs })
	defer w.Close()
	for _, m := range msgs {
		line, err := m.MarshalLine()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteLine(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func req(ts time.Time, rid uint64, facade, method, unit string) wire.CapturedMessage {
	return wire.CapturedMessage{
		Ts: ts, PID: 1001, Dir: wire.DirWrite, Conn: 7, Model: "default", Unit: unit,
		Msg: wire.Envelope{RequestID: rid, Type: facade, Request: method, Params: json.RawMessage(`{}`)},
	}
}

func resp(ts time.Time, rid uint64) wire.CapturedMessage {
	return wire.CapturedMessage{
		Ts: ts, PID: 1001, Dir: wire.DirRead, Conn: 7, Model: "default",
		Msg: wire.Envelope{RequestID: rid, Response: json.RawMessage(`{}`)},
	}
}

func TestLoadSpansPairsAndSorts(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.Init(); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC)

	// Emit a "late" RPC before an "early" one to prove sorting by start time.
	writeCalls(t, l, "default", base,
		req(base.Add(200*time.Millisecond), 2, "Uniter", "Late", "grafana/0"),
		resp(base.Add(300*time.Millisecond), 2),
		req(base.Add(100*time.Millisecond), 1, "Uniter", "Early", "grafana/0"),
		resp(base.Add(150*time.Millisecond), 1),
	)

	rows, err := LoadSpans(dir)
	if err != nil {
		t.Fatalf("LoadSpans: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 paired spans, got %d", len(rows))
	}
	if rows[0].Name != "Uniter.Early" || rows[1].Name != "Uniter.Late" {
		t.Fatalf("expected [Uniter.Early, Uniter.Late], got [%s, %s]", rows[0].Name, rows[1].Name)
	}
	// Duration is request→response.
	if got := rows[0].Duration(); got != 50*time.Millisecond {
		t.Fatalf("early duration = %s, want 50ms", got)
	}
	if rows[0].Unit != "grafana/0" || rows[0].Model != "default" {
		t.Fatalf("attribution broken: %+v", rows[0])
	}
	if rows[0].RawFile == "" {
		t.Fatalf("expected raw_file pointer to be populated")
	}
}

func TestLoadSpansUnmatchedRequestStillEmitted(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.Init(); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC)
	// Request with no response (recorder stopped mid-flight).
	writeCalls(t, l, "default", base, req(base, 9, "Uniter", "Orphan", "grafana/0"))

	rows, err := LoadSpans(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the unmatched request to still yield a span, got %d", len(rows))
	}
	if rows[0].Start != rows[0].End {
		t.Fatalf("unmatched request should be zero-duration, got %s..%s", rows[0].Start, rows[0].End)
	}
}
