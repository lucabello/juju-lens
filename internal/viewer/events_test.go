package viewer

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

var eventsBase = time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)

// setStateSpan builds a Uniter.SetState span carrying a uniter-state blob, the
// way the wire does (uniter-state is a JSON string value).
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

// rpcSpan is a plain RPC inside a hook bracket, optionally errored.
func rpcSpan(id, unit string, dt time.Duration, errored bool) recording.SpanRow {
	sp := recording.SpanRow{
		SpanID: id, Unit: unit, Model: "default",
		Start: eventsBase.Add(dt), End: eventsBase.Add(dt),
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

func runHook(kind, opstep string) string {	return "op: run-hook\nopstep: " + opstep + "\nhook:\n  kind: " + kind + "\n"
}

const continueDone = "op: continue\nopstep: done\n"

// onlyHooks returns the hook runs for the single unit in spans, in order.
func hookRunsOf(spans []recording.SpanRow) []hookRun { return buildHookRuns(spans) }

func TestHookRunCleanCompletion(t *testing.T) {
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		rpcSpan("2", "grafana/0", 2*time.Second, false),
		setStateSpan("3", "grafana/0", 3*time.Second, continueDone),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].fail != failNone {
		t.Fatalf("clean hook classified as %v", runs[0].fail)
	}
}

func TestHookRunRetriedThenRecovered(t *testing.T) {
	// The uniter re-writes the same run-hook marker (opstep pending) when it
	// retries a failed hook, then writes `continue` once it succeeds: recovered.
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		rpcSpan("2", "grafana/0", 2*time.Second, true),
		setStateSpan("3", "grafana/0", 3*time.Second, runHook("config-changed", "pending")),
		rpcSpan("4", "grafana/0", 4*time.Second, false),
		setStateSpan("5", "grafana/0", 5*time.Second, continueDone),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].fail != failRetried {
		t.Fatalf("recovered hook classified as %v, want retried", runs[0].fail)
	}
}

func TestHookRunErroredStillFailingAtTail(t *testing.T) {
	// Retried but never reached `continue`: the unit is in error state now.
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		rpcSpan("2", "grafana/0", 2*time.Second, true),
		setStateSpan("3", "grafana/0", 3*time.Second, runHook("config-changed", "pending")),
		rpcSpan("4", "grafana/0", 4*time.Second, true),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].fail != failErrored {
		t.Fatalf("still-failing hook classified as %v, want errored", runs[0].fail)
	}
}

func TestHookRunOpstepProgressionNotRetry(t *testing.T) {
	// pending -> done for the same hook is normal progression, not a retry.
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		setStateSpan("2", "grafana/0", 2*time.Second, runHook("config-changed", "done")),
		setStateSpan("3", "grafana/0", 3*time.Second, continueDone),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 || runs[0].fail != failNone {
		t.Fatalf("opstep progression misclassified: %+v", runs)
	}
}

func TestHookRunInterrupted(t *testing.T) {
	// A different hook opens before the first one's continue: interrupted.
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("install", "pending")),
		setStateSpan("2", "grafana/0", 2*time.Second, runHook("config-changed", "pending")),
		setStateSpan("3", "grafana/0", 3*time.Second, continueDone),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(runs))
	}
	if runs[0].fail != failInterrupted {
		t.Fatalf("first hook classified as %v, want interrupted", runs[0].fail)
	}
	if runs[1].fail != failNone {
		t.Fatalf("second hook classified as %v, want none", runs[1].fail)
	}
}

func TestHookRunRPCWarn(t *testing.T) {
	// Completed hook whose only symptom is an errored RPC: soft warning.
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		rpcSpan("2", "grafana/0", 2*time.Second, true),
		setStateSpan("3", "grafana/0", 3*time.Second, continueDone),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 || runs[0].fail != failRPCWarn {
		t.Fatalf("rpc-warn misclassified: %+v", runs)
	}
}

func TestHookRunOpenAtTailNotFailed(t *testing.T) {
	// A run-hook with no continue at the tail is running, not failed.
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("update-status", "pending")),
		rpcSpan("2", "grafana/0", 2*time.Second, false),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if !runs[0].open || runs[0].fail != failNone {
		t.Fatalf("open tail hook misclassified: open=%v fail=%v", runs[0].open, runs[0].fail)
	}
}

func TestStatusAttributedToHook(t *testing.T) {
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		rpcSpan("s2", "grafana/0", 2*time.Second, false), // the status-setter span
		setStateSpan("3", "grafana/0", 3*time.Second, continueDone),
	}
	statusBySpan := map[string]hookStatus{
		"s2": {value: "blocked", message: "waiting for db"},
	}
	evs := buildEventsWithStatus(spans, nil, statusBySpan)
	var hook *event
	for i := range evs {
		if evs[i].kind == evHook {
			hook = &evs[i]
			break
		}
	}
	if hook == nil {
		t.Fatal("no hook event")
	}
	if len(hook.statuses) != 1 || hook.statuses[0].value != "blocked" {
		t.Fatalf("status not attributed to hook: %+v", hook.statuses)
	}
	if got := hook.statusSummary(false); got != statusStyleFor("blocked").Render(`→ blocked`) {
		t.Fatalf("statusSummary = %q", got)
	}
}

// statusRPCSpan builds a Uniter status-setter span (SetAgentStatus /
// SetUnitStatus) carrying a single entity's status, for settle-marker tests.
func statusRPCSpan(id, method, unit string, dt time.Duration, value string) recording.SpanRow {
	params, _ := json.Marshal(map[string]any{
		"entities": []map[string]any{{"tag": "unit-x-0", "status": value, "info": ""}},
	})
	return recording.SpanRow{
		SpanID: id, Unit: unit, Model: "default",
		Start: eventsBase.Add(dt), End: eventsBase.Add(dt),
		Name:  "Uniter." + method,
		Attrs: map[string]string{"method": method, "params": string(params)},
	}
}

func TestSettleEvents(t *testing.T) {
	spans := []recording.SpanRow{
		statusRPCSpan("w1", "SetUnitStatus", "x/0", 0, "active"),
		statusRPCSpan("a1", "SetAgentStatus", "x/0", 1*time.Second, "executing"),
		// Brief drain: idle then executing again <5s later — not a settle.
		statusRPCSpan("a2", "SetAgentStatus", "x/0", 2*time.Second, "idle"),
		statusRPCSpan("a3", "SetAgentStatus", "x/0", 3*time.Second, "executing"),
		// Real rest: idle for well over the threshold before the next executing.
		statusRPCSpan("a4", "SetAgentStatus", "x/0", 4*time.Second, "idle"),
		statusRPCSpan("a5", "SetAgentStatus", "x/0", 30*time.Second, "executing"),
		// Tail idle: still idle at the end of the recording.
		statusRPCSpan("a6", "SetAgentStatus", "x/0", 40*time.Second, "idle"),
	}
	got := settleEvents(spans)
	if len(got) != 2 {
		t.Fatalf("expected 2 settle events (long rest + tail), got %d: %+v", len(got), got)
	}
	for _, ev := range got {
		if ev.kind != evSettle {
			t.Errorf("kind = %v, want evSettle", ev.kind)
		}
		if ev.settleWorkload != "active" {
			t.Errorf("settleWorkload = %q, want active", ev.settleWorkload)
		}
		if ev.summary != "→ active / idle" {
			t.Errorf("summary = %q, want %q", ev.summary, "→ active / idle")
		}
	}
	// The two kept markers are the long-rest idle (4s) and the tail idle (40s).
	if !got[0].ts.Equal(eventsBase.Add(4*time.Second)) || !got[1].ts.Equal(eventsBase.Add(40*time.Second)) {
		t.Errorf("unexpected settle timestamps: %v, %v", got[0].ts, got[1].ts)
	}
}
