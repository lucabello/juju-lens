package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFolderAllPartsByDefault(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := WriteFolder(out, r, nil); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"AGENTS.md", "SUMMARY.md", "timeline.jsonl", "events.jsonl", "report.md"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(out, "details"))
	if err != nil {
		t.Fatalf("details/: %v", err)
	}
	if len(entries) != r.Summary.TotalEvents {
		t.Errorf("details/ has %d files, want %d (one per event)", len(entries), r.Summary.TotalEvents)
	}
}

func TestWriteFolderPartsSubset(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := WriteFolder(out, r, []Part{PartSummary}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(out, "SUMMARY.md")); err != nil {
		t.Errorf("expected SUMMARY.md to exist: %v", err)
	}
	for _, name := range []string{"timeline.jsonl", "events.jsonl", "report.md", "details"} {
		if _, err := os.Stat(filepath.Join(out, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s not to exist when only PartSummary is selected, got err=%v", name, err)
		}
	}

	// AGENTS.md must not mention a file that wasn't written.
	agents, err := os.ReadFile(filepath.Join(out, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"timeline.jsonl", "events.jsonl", "details/", "report.md"} {
		if strings.Contains(string(agents), absent) {
			t.Errorf("AGENTS.md mentions %q but that part wasn't selected:\n%s", absent, agents)
		}
	}
	if !strings.Contains(string(agents), "SUMMARY.md") {
		t.Errorf("AGENTS.md should still mention SUMMARY.md:\n%s", agents)
	}
}

func TestWriteFolderTimelineJSONLIsGreppable(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := WriteFolder(out, r, []Part{PartTimeline, PartEvents}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(out, "timeline.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != r.Summary.TotalEvents+r.Summary.TotalLogs {
		t.Fatalf("timeline.jsonl has %d lines, want %d (events + logs)", len(lines), r.Summary.TotalEvents+r.Summary.TotalLogs)
	}
	var sawEvent, sawLog bool
	for i, line := range lines {
		var rec LineRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d not valid single-line JSON: %v\nline: %q", i, err, line)
		}
		if strings.Contains(line, "\n") {
			t.Fatalf("line %d contains a literal newline, breaks grep -C context math: %q", i, line)
		}
		switch rec.Type {
		case "event":
			sawEvent = true
		case "log":
			sawLog = true
		default:
			t.Errorf("line %d has unexpected type %q", i, rec.Type)
		}
	}
	if !sawEvent || !sawLog {
		t.Errorf("expected both event and log lines in timeline.jsonl (event=%v log=%v)", sawEvent, sawLog)
	}

	// events.jsonl must contain only event-typed lines.
	evData, err := os.ReadFile(filepath.Join(out, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(evData), "\n"), "\n") {
		var rec LineRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("events.jsonl: %v", err)
		}
		if rec.Type != "event" {
			t.Errorf("events.jsonl contains a %q line, want only events", rec.Type)
		}
	}
}

func TestWriteFolderDetailsAddressableBySpanID(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{Units: []string{"grafana/0"}, ErrorsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := WriteFolder(out, r, nil); err != nil {
		t.Fatal(err)
	}

	var sawRetried bool
	for _, it := range r.Items {
		if it.Kind != "event" {
			continue
		}
		path := filepath.Join(out, "details", it.Event.SpanID+".json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("details file for span %s: %v", it.Event.SpanID, err)
		}
		var got EventDetail
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("details file for span %s not valid JSON: %v", it.Event.SpanID, err)
		}
		if got.SpanID != it.Event.SpanID {
			t.Errorf("details file %s has span_id %s", path, got.SpanID)
		}
		if it.Event.Fail == "retried" {
			sawRetried = true
			if got.FailExplanation == "" {
				t.Errorf("expected the retried event's details file to carry a failure explanation")
			}
		}
	}
	if !sawRetried {
		t.Fatal("expected the retried config-changed hook in this fixture's --errors-only scope")
	}
}

func TestWriteFolderSummaryMDIsNotJustFailures(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := WriteFolder(out, r, []Part{PartSummary}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "SUMMARY.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	// The clean prometheus/0 install hook must still be represented (via the
	// none/errored/... breakdown and per-unit counts), not just the failure —
	// SUMMARY.md describes shape, not a verdict.
	for _, want := range []string{"# Recording shape", "not a diagnosis", "## Hook outcomes", "none: ", "prometheus/0", "retried: 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("SUMMARY.md missing %q:\n%s", want, got)
		}
	}
}
