package export

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

var base = time.Date(2026, 8, 18, 14, 0, 0, 0, time.UTC)

// setStateSpan and rpcSpan mirror the fixtures in
// internal/narrative/events_test.go — small enough to duplicate rather than
// export a test-only helper across package boundaries.
func setStateSpan(id, unit string, dt time.Duration, us string) recording.SpanRow {
	params, _ := json.Marshal(map[string]any{
		"args": []map[string]any{{"tag": "unit-x-0", "uniter-state": us}},
	})
	return recording.SpanRow{
		SpanID: id, Unit: unit, Model: "default",
		Start: base.Add(dt), End: base.Add(dt),
		Name:  "Uniter.SetState",
		Attrs: map[string]string{"method": "SetState", "params": string(params)},
	}
}

func rpcSpan(id, unit string, dt time.Duration, errored bool) recording.SpanRow {
	sp := recording.SpanRow{
		SpanID: id, Unit: unit, Model: "default",
		Start: base.Add(dt), End: base.Add(dt),
		Name:       "Uniter.ReadSettings",
		StatusCode: "OK",
		Attrs:      map[string]string{"method": "ReadSettings"},
	}
	if errored {
		sp.StatusCode = "ERROR"
		sp.StatusMsg = "boom"
	}
	return sp
}

func runHook(kind, opstep string) string {
	return "op: run-hook\nopstep: " + opstep + "\nhook:\n  kind: " + kind + "\n"
}

const continueDone = "op: continue\nopstep: done\n"

// buildFixture writes a recording directory with:
//   - grafana/0: a clean "install" hook, then a "config-changed" hook that
//     errors, retries, and recovers (FailRetried) — with a log line beside it
//   - prometheus/0: a clean "install" hook, with a log line beside it
//
// and returns its directory.
func buildFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	layout := recording.NewLayout(dir)
	if err := layout.Init(); err != nil {
		t.Fatal(err)
	}
	man := recording.New("juju-lens", "test", "", "test-controller")
	man.Models = []recording.ModelInfo{{Name: "default"}}
	man.Started = base
	man.Ended = base.Add(time.Hour)
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}

	db, err := index.Open(layout.IndexDB())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertModel("default"); err != nil {
		t.Fatal(err)
	}

	spans := []recording.SpanRow{
		// grafana/0: clean install hook.
		setStateSpan("g1", "grafana/0", 1*time.Second, runHook("install", "pending")),
		rpcSpan("g2", "grafana/0", 2*time.Second, false),
		setStateSpan("g3", "grafana/0", 3*time.Second, continueDone),
		// grafana/0: config-changed errors, retries, then recovers.
		setStateSpan("g4", "grafana/0", 10*time.Second, runHook("config-changed", "pending")),
		rpcSpan("g5", "grafana/0", 11*time.Second, true),
		setStateSpan("g6", "grafana/0", 12*time.Second, runHook("config-changed", "pending")),
		rpcSpan("g7", "grafana/0", 13*time.Second, false),
		setStateSpan("g8", "grafana/0", 14*time.Second, continueDone),
		// prometheus/0: clean install hook.
		setStateSpan("p1", "prometheus/0", 1*time.Second, runHook("install", "pending")),
		rpcSpan("p2", "prometheus/0", 2*time.Second, false),
		setStateSpan("p3", "prometheus/0", 3*time.Second, continueDone),
	}
	index.LabelHooks(spans)
	for _, sp := range spans {
		if err := db.InsertSpan(sp); err != nil {
			t.Fatal(err)
		}
	}

	logs := []recording.LogRecord{
		{Ts: base.Add(1500 * time.Millisecond), Model: "default", Source: "debug-log",
			Entity: "unit-grafana-0", Unit: "grafana/0", Level: "INFO", Module: "juju.worker.uniter.operation",
			Message: `ran "install" hook`},
		{Ts: base.Add(11500 * time.Millisecond), Model: "default", Source: "k8s",
			Entity: "grafana-0", Unit: "grafana/0", Level: "ERROR", Module: "",
			Message: "connection refused: db:5432"},
		{Ts: base.Add(1500 * time.Millisecond), Model: "default", Source: "debug-log",
			Entity: "unit-prometheus-0", Unit: "prometheus/0", Level: "INFO", Module: "juju.worker.uniter.operation",
			Message: `ran "install" hook`},
	}
	for _, lg := range logs {
		if err := db.InsertLog(lg); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildNoFilters(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary.TotalEvents != 3 { // grafana install, grafana config-changed, prometheus install
		t.Fatalf("total events = %d, want 3: %+v", r.Summary.TotalEvents, r.Summary)
	}
	if r.Summary.RetriedEvents != 1 {
		t.Fatalf("retried events = %d, want 1", r.Summary.RetriedEvents)
	}
	if r.Summary.TotalLogs != 3 {
		t.Fatalf("total logs = %d, want 3", r.Summary.TotalLogs)
	}
	// Items must be strictly time-ordered.
	for i := 1; i < len(r.Items); i++ {
		if r.Items[i].TS.Before(r.Items[i-1].TS) {
			t.Fatalf("items not time-ordered at %d: %v before %v", i, r.Items[i].TS, r.Items[i-1].TS)
		}
	}
}

func TestBuildUnitScoping(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{Units: []string{"prometheus/0"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary.TotalEvents != 1 {
		t.Fatalf("total events = %d, want 1 (prometheus install only)", r.Summary.TotalEvents)
	}
	for _, it := range r.Items {
		if it.Kind == "event" && it.Event.Unit != "prometheus/0" {
			t.Errorf("unexpected unit in scoped export: %s", it.Event.Unit)
		}
		if it.Kind == "log" && it.Log.Unit != "" && it.Log.Unit != "prometheus/0" {
			t.Errorf("unexpected log unit in scoped export: %s", it.Log.Unit)
		}
	}
}

func TestBuildErrorsOnlyIncludesContext(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{Units: []string{"grafana/0"}, ErrorsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawFailure, sawContext bool
	for _, it := range r.Items {
		if it.Kind != "event" {
			continue
		}
		if it.Event.Fail == "retried" {
			sawFailure = true
		}
		if it.Event.Context {
			sawContext = true
		}
	}
	if !sawFailure {
		t.Error("expected the retried config-changed hook in --errors-only output")
	}
	if !sawContext {
		t.Error("expected the clean install hook to be pulled in as neighbour context")
	}
}

func TestBuildRequiresModelWhenAmbiguous(t *testing.T) {
	dir := buildFixture(t)
	layout := recording.NewLayout(dir)
	db, err := index.Open(layout.IndexDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertModel("second"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Build(dir, Options{})
	if err == nil || !strings.Contains(err.Error(), "multiple models") {
		t.Fatalf("want 'multiple models' error, got %v", err)
	}
	if _, err := Build(dir, Options{Model: "bogus"}); err == nil {
		t.Fatal("want error for unknown --model")
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := WriteJSON(&b, r); err != nil {
		t.Fatal(err)
	}
	var got Result
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("output did not round-trip through json.Unmarshal: %v", err)
	}
	if got.Summary.TotalEvents != r.Summary.TotalEvents {
		t.Fatalf("round-tripped total_events = %d, want %d", got.Summary.TotalEvents, r.Summary.TotalEvents)
	}
}

func TestWriteMarkdownNoANSI(t *testing.T) {
	dir := buildFixture(t)
	r, err := Build(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := WriteMarkdown(&b, r); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Contains(out, "\x1b[") {
		t.Error("Markdown output contains raw ANSI escapes")
	}
	for _, want := range []string{"grafana/0", "config-changed", "RETRIED", "## Summary", "## Timeline"} {
		if !strings.Contains(out, want) {
			t.Errorf("Markdown output missing %q\n---\n%s", want, out)
		}
	}
}
