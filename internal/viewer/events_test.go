package viewer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// The hook-run/failure/settle derivation itself is tested in
// internal/narrative/events_test.go now that the logic lives there (M11).
// What's left here is the viewer-local wrapper boundary: buildEventsWithStatus
// still returns the viewer's local event shape, and statusSummary still
// renders with the viewer's lipgloss styles.

var eventsBase = time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)

func setStateSpan(id, unit string, dt time.Duration, us string) recording.SpanRow {
	params, _ := json.Marshal(map[string]any{
		"args": []map[string]any{{"tag": "unit-x-0", "uniter-state": us}},
	})
	return recording.SpanRow{
		SpanID: id, Unit: unit, Model: "default",
		Start: eventsBase.Add(dt), End: eventsBase.Add(dt),
		Name:  "Uniter.SetState",
		Attrs: map[string]string{"method": "SetState", "params": string(params)},
	}
}

func rpcSpan(id, unit string, dt time.Duration) recording.SpanRow {
	return recording.SpanRow{
		SpanID: id, Unit: unit, Model: "default",
		Start: eventsBase.Add(dt), End: eventsBase.Add(dt),
		Name:       "Uniter.ReadSettings",
		StatusCode: "OK",
		Attrs:      map[string]string{"method": "ReadSettings"},
	}
}

func runHook(kind, opstep string) string {
	return "op: run-hook\nopstep: " + opstep + "\nhook:\n  kind: " + kind + "\n"
}

const continueDone = "op: continue\nopstep: done\n"

func TestBuildEventsWithStatusAndSummary(t *testing.T) {
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		rpcSpan("s2", "grafana/0", 2*time.Second), // the status-setter span
		setStateSpan("3", "grafana/0", 3*time.Second, continueDone),
	}
	statusBySpan := map[string]narrative.HookStatus{
		"s2": {Value: "blocked", Message: "waiting for db"},
	}
	evs := buildEventsWithStatus(spans, nil, statusBySpan)
	var hook, marker *event
	for i := range evs {
		switch evs[i].kind {
		case evHook:
			hook = &evs[i]
		case evStatus:
			marker = &evs[i]
		}
	}
	if hook == nil {
		t.Fatal("no hook event")
	}
	if len(hook.statuses) != 0 {
		t.Fatalf("status should have moved off the hook event, still there: %+v", hook.statuses)
	}
	if marker == nil {
		t.Fatal("no status marker event")
	}
	if len(marker.statuses) != 1 || marker.statuses[0].Value != "blocked" {
		t.Fatalf("status not attributed to its marker: %+v", marker.statuses)
	}
	if marker.cause != "config-changed" {
		t.Fatalf("marker missing its causing hook: got %q", marker.cause)
	}
	if got := marker.statusSummary(false); got != statusStyleFor("blocked").Render(`→ blocked`) {
		t.Fatalf("statusSummary = %q", got)
	}
}

func TestEventGlyphAndFailLabel(t *testing.T) {
	e := event{fail: failErrored}
	if !e.isErrored() {
		t.Fatal("want isErrored")
	}
	if got := e.failLabel(); got != "error" {
		t.Fatalf("failLabel = %q, want error", got)
	}
	e.verboseOnly = true
	if got := e.glyph(); got != "·" {
		t.Fatalf("glyph = %q, want ·", got)
	}
}

// TestCaptureGapVerboseOnly checks that failLostContinue ("capture gap") is
// hidden from the default timeline row — it's a statement about the
// recording, not the charm, so it shouldn't read as a deployment problem —
// but still shows once verbose mode is on, and the row falls back to the
// hook's plain duration either way (M13).
func TestCaptureGapVerboseOnly(t *testing.T) {
	ev := event{kind: evHook, fail: failLostContinue, dur: 3 * time.Second}

	m := &model{}
	if got := m.eventSuffix(ev); strings.Contains(got, "capture gap") {
		t.Fatalf("default view leaked capture gap: %q", got)
	} else if !strings.Contains(got, "3.0s") {
		t.Fatalf("default view should fall back to duration: %q", got)
	}

	m.verbose = true
	if got := m.eventSuffix(ev); !strings.Contains(got, "capture gap") {
		t.Fatalf("verbose view missing capture gap: %q", got)
	}
}

// TestInterruptedVerboseOnly checks that failInterrupted, like failLostContinue,
// is hidden from the default timeline row and only shows once verbose mode is
// on — a hook flagged this way still ran, and the label reads as a deployment
// problem more often than it is one (M14).
func TestInterruptedVerboseOnly(t *testing.T) {
	ev := event{kind: evHook, fail: failInterrupted, dur: 500 * time.Millisecond}

	m := &model{}
	if got := m.eventSuffix(ev); strings.Contains(got, "interrupted") {
		t.Fatalf("default view leaked interrupted: %q", got)
	} else if !strings.Contains(got, "500ms") {
		t.Fatalf("default view should fall back to duration: %q", got)
	}

	m.verbose = true
	if got := m.eventSuffix(ev); !strings.Contains(got, "interrupted") {
		t.Fatalf("verbose view missing interrupted: %q", got)
	}
}
