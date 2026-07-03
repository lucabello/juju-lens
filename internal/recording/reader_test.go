package recording

import (
	"testing"
	"time"
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

func TestLoadSpansParsesJSONLAndSorts(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.Init(); err != nil {
		t.Fatal(err)
	}

	// Two payloads produced with different start times; verify sort.
	w := NewRotatingWriter(func(h string) string { return l.OTLPTracesFile(h) }, func() time.Time {
		return time.Date(2026, 7, 3, 14, 0, 0, 0, time.UTC)
	})
	defer w.Close()

	// A minimally valid OTLP JSON line built by hand — enough to exercise
	// the parser without pulling synth into the recording package.
	late := `{"resource_spans":[{"resource":{"attributes":[{"key":"service.name","value":{"string_value":"late"}}]},"scope_spans":[{"spans":[{"trace_id":"AAAA","span_id":"BB","name":"late","start_time_unix_nano":"200","end_time_unix_nano":"300","status":{}}]}]}]}`
	early := `{"resource_spans":[{"resource":{"attributes":[{"key":"service.name","value":{"string_value":"early"}}]},"scope_spans":[{"spans":[{"trace_id":"AAAA","span_id":"CC","name":"early","start_time_unix_nano":"100","end_time_unix_nano":"150","status":{}}]}]}]}`
	for _, line := range []string{late, early} {
		if _, err := w.WriteLine([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err := LoadSpans(dir)
	if err != nil {
		t.Fatalf("LoadSpans: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(rows))
	}
	if rows[0].Name != "early" || rows[1].Name != "late" {
		t.Fatalf("expected [early, late], got [%s, %s]", rows[0].Name, rows[1].Name)
	}
	if rows[0].Service != "early" || rows[1].Service != "late" {
		t.Fatalf("service.name propagation broken: %+v %+v", rows[0], rows[1])
	}
}
