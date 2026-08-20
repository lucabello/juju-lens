package narrative

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

func runHook(kind, opstep string) string {
	return "op: run-hook\nopstep: " + opstep + "\nhook:\n  kind: " + kind + "\n"
}

const continueDone = "op: continue\nopstep: done\n"

// hookRunsOf returns the hook runs for the spans in order.
func hookRunsOf(spans []recording.SpanRow) []HookRun { return BuildHookRuns(spans) }

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
	if runs[0].Fail != FailNone {
		t.Fatalf("clean hook classified as %v", runs[0].Fail)
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
	if runs[0].Fail != FailRetried {
		t.Fatalf("recovered hook classified as %v, want retried", runs[0].Fail)
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
	if runs[0].Fail != FailErrored {
		t.Fatalf("still-failing hook classified as %v, want errored", runs[0].Fail)
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
	if len(runs) != 1 || runs[0].Fail != FailNone {
		t.Fatalf("opstep progression misclassified: %+v", runs)
	}
}

func TestHookRunQueuedThenPendingNotRetry(t *testing.T) {
	// The normal, error-free lifecycle for a single hook execution walks
	// opstep queued -> pending -> done. The first `pending` following a
	// `queued` is routine, not a retry loop (M13 regression: an earlier
	// version counted it as one, flagging every healthy hook as "retried").
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("install", "queued")),
		setStateSpan("2", "grafana/0", 2*time.Second, runHook("install", "pending")),
		setStateSpan("3", "grafana/0", 3*time.Second, runHook("install", "done")),
		setStateSpan("4", "grafana/0", 4*time.Second, continueDone),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 || runs[0].Fail != FailNone || runs[0].Retries != 0 {
		t.Fatalf("queued->pending->done misclassified as a retry: %+v", runs)
	}
}

func TestHookRunQueuedPendingPendingIsRetry(t *testing.T) {
	// A *second* `pending` after queued->pending is a real retry loop: the
	// hook errored (opstep never advanced to `done`) and the uniter
	// re-entered `pending` to try again.
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("install", "queued")),
		setStateSpan("2", "grafana/0", 2*time.Second, runHook("install", "pending")),
		rpcSpan("3", "grafana/0", 3*time.Second, true),
		setStateSpan("4", "grafana/0", 4*time.Second, runHook("install", "pending")),
		setStateSpan("5", "grafana/0", 5*time.Second, runHook("install", "done")),
		setStateSpan("6", "grafana/0", 6*time.Second, continueDone),
	}
	runs := hookRunsOf(spans)
	if len(runs) != 1 || runs[0].Fail != FailRetried || runs[0].Retries != 1 {
		t.Fatalf("queued->pending->pending->done not classified as one retry: %+v", runs)
	}
}

func TestHookRunLostContinueVsInterrupted(t *testing.T) {
	// A different hook opens before the first one closes in both cases, but
	// the first hook reached `done` in the first (it actually finished — only
	// its own `continue` is missing, a capture gap) and never did in the
	// second (it was genuinely abandoned mid-run) (M13).
	finished := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		setStateSpan("2", "grafana/0", 2*time.Second, runHook("config-changed", "done")),
		setStateSpan("3", "grafana/0", 3*time.Second, runHook("start", "queued")), // no continue for config-changed
	}
	runs := hookRunsOf(finished)
	if len(runs) != 2 || runs[0].Fail != FailLostContinue {
		t.Fatalf("finished-but-uncontinued hook not classified as lost-continue: %+v", runs)
	}

	abandoned := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("relation-changed", "pending")),
		setStateSpan("2", "grafana/0", 2*time.Second, runHook("stop", "pending")), // never reached done
	}
	runs = hookRunsOf(abandoned)
	if len(runs) != 2 || runs[0].Fail != FailInterrupted {
		t.Fatalf("abandoned hook not classified as interrupted: %+v", runs)
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
	if runs[0].Fail != FailInterrupted {
		t.Fatalf("first hook classified as %v, want interrupted", runs[0].Fail)
	}
	if runs[1].Fail != FailNone {
		t.Fatalf("second hook classified as %v, want none", runs[1].Fail)
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
	if len(runs) != 1 || runs[0].Fail != FailRPCWarn {
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
	if !runs[0].Open || runs[0].Fail != FailNone {
		t.Fatalf("open tail hook misclassified: open=%v fail=%v", runs[0].Open, runs[0].Fail)
	}
}

// TestStatusAttributedToHook checks that a status a hook set becomes its own
// EvStatus marker (M12) — positioned at the setter RPC's own instant, not the
// hook's start, and naming the hook in Cause — rather than living on the
// hook event's own Statuses. See BuildEventsWithStatus's doc comment for why:
// a row claiming an outcome at the hook's start would be misleading once
// another unit's hook is concurrently open across the same window.
func TestStatusAttributedToHook(t *testing.T) {
	spans := []recording.SpanRow{
		setStateSpan("1", "grafana/0", 1*time.Second, runHook("config-changed", "pending")),
		rpcSpan("s2", "grafana/0", 2*time.Second, false), // the status-setter span
		setStateSpan("3", "grafana/0", 3*time.Second, continueDone),
	}
	statusBySpan := map[string]HookStatus{
		"s2": {Value: "blocked", Message: "waiting for db"},
	}
	evs := BuildEventsWithStatus(spans, nil, statusBySpan)
	var hook, marker *Event
	for i := range evs {
		switch evs[i].Kind {
		case EvHook:
			hook = &evs[i]
		case EvStatus:
			marker = &evs[i]
		}
	}
	if hook == nil {
		t.Fatal("no hook event")
	}
	if len(hook.Statuses) != 0 {
		t.Fatalf("status should have moved off the hook event, still there: %+v", hook.Statuses)
	}
	if marker == nil {
		t.Fatal("no status marker event")
	}
	if len(marker.Statuses) != 1 || marker.Statuses[0].Value != "blocked" {
		t.Fatalf("status not attributed to its marker: %+v", marker.Statuses)
	}
	if marker.Cause != "config-changed" {
		t.Fatalf("marker missing its causing hook: got %q", marker.Cause)
	}
	if !marker.TS.Equal(eventsBase.Add(2 * time.Second)) {
		t.Fatalf("marker should sit at the setter RPC's own instant, got %v", marker.TS)
	}
	if marker.VerboseOnly {
		t.Fatalf("a first-time status change should not be demoted to verbose-only")
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
		// Brief drain: idle then executing again <3s later — not a settle.
		statusRPCSpan("a2", "SetAgentStatus", "x/0", 2*time.Second, "idle"),
		statusRPCSpan("a3", "SetAgentStatus", "x/0", 3*time.Second, "executing"),
		// Real rest: idle for well over the threshold before the next executing.
		statusRPCSpan("a4", "SetAgentStatus", "x/0", 4*time.Second, "idle"),
		statusRPCSpan("a5", "SetAgentStatus", "x/0", 30*time.Second, "executing"),
		// Tail idle, with the recording actually capturing well over the
		// threshold afterward (another unit's span) — real evidence of rest,
		// not just the recording happening to stop.
		statusRPCSpan("a6", "SetAgentStatus", "x/0", 40*time.Second, "idle"),
		statusRPCSpan("a7", "SetAgentStatus", "y/0", 45*time.Second, "executing"),
	}
	got := settleEvents(spans)
	var xSettles []Event
	for _, ev := range got {
		if ev.Unit == "x/0" {
			xSettles = append(xSettles, ev)
		}
	}
	if len(xSettles) != 2 {
		t.Fatalf("expected 2 settle events for x/0 (long rest + tail), got %d: %+v", len(xSettles), xSettles)
	}
	for _, ev := range xSettles {
		if ev.Kind != EvSettle {
			t.Errorf("kind = %v, want EvSettle", ev.Kind)
		}
		if ev.SettleWorkload != "active" {
			t.Errorf("settleWorkload = %q, want active", ev.SettleWorkload)
		}
		if ev.Summary != "→ active / idle" {
			t.Errorf("summary = %q, want %q", ev.Summary, "→ active / idle")
		}
	}
	// The two kept markers are the long-rest idle (4s) and the tail idle (40s).
	if !xSettles[0].TS.Equal(eventsBase.Add(4*time.Second)) || !xSettles[1].TS.Equal(eventsBase.Add(40*time.Second)) {
		t.Errorf("unexpected settle timestamps: %v, %v", xSettles[0].TS, xSettles[1].TS)
	}
}

// TestSettleEventsTailNeedsRealElapsedTime checks that idle at the very tail
// of a recording, with nothing captured afterward, does not count as
// "settled" — the recording simply stopped, which is not evidence the unit
// was actually at rest for settleQuiescence.
func TestSettleEventsTailNeedsRealElapsedTime(t *testing.T) {
	spans := []recording.SpanRow{
		statusRPCSpan("w1", "SetUnitStatus", "x/0", 0, "active"),
		statusRPCSpan("a1", "SetAgentStatus", "x/0", 1*time.Second, "executing"),
		// Idle right at the tail: no time elapses after it in this capture.
		statusRPCSpan("a2", "SetAgentStatus", "x/0", 2*time.Second, "idle"),
	}
	if got := settleEvents(spans); len(got) != 0 {
		t.Fatalf("expected no settle events, got %d: %+v", len(got), got)
	}
}
