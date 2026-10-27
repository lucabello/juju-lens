package narrative

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// opsFixture loads the tap span trees of a lab fixture (see
// internal/recording/ops_test.go for what each one holds).
func opsFixture(t *testing.T, name string) []recording.SpanRow {
	t.Helper()
	spans, _, err := recording.LoadOps(filepath.Join("..", "recording", "testdata", "ops", name))
	if err != nil {
		t.Fatal(err)
	}
	return spans
}

func opsRoots(spans []recording.SpanRow) []recording.SpanRow {
	var out []recording.SpanRow
	for _, s := range spans {
		if s.Service == recording.ServiceOps && s.Attrs["kind"] == recording.OpsRoot {
			out = append(out, s)
		}
	}
	return out
}

// runLines renders runs as "unit name fail retries processes", by start.
func runLines(runs []HookRun) string {
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartTs.Before(runs[j].StartTs) })
	var out []string
	for _, r := range runs {
		out = append(out, strings.Join([]string{
			r.Unit, hookDisplayName(r, nil), r.Fail.String(),
			"retries=" + itoa(r.Retries), "procs=" + itoa(len(r.OpsSpans)),
		}, " "))
	}
	return strings.Join(out, "\n")
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// checkEveryRootInOneRun: a run leads to its processes, and each hook or
// action process leads back to exactly one run.
func checkEveryRootInOneRun(t *testing.T, spans []recording.SpanRow, runs []HookRun) {
	t.Helper()
	claimed := map[string]int{}
	for _, r := range runs {
		for _, id := range r.OpsSpans {
			claimed[id]++
		}
	}
	for _, root := range opsRoots(spans) {
		if root.Attrs["exec"] == recording.ExecProcess {
			continue
		}
		if claimed[root.SpanID] != 1 {
			t.Errorf("process %s %q is in %d runs, want 1", root.Unit, root.Name, claimed[root.SpanID])
		}
	}
}

func TestOpsOnlyRunsFoldRetries(t *testing.T) {
	spans := opsFixture(t, "k8s")
	runs := BuildHookRuns(spans)
	checkEveryRootInOneRun(t, spans, runs)
	got := runLines(runs)
	want := strings.Join([]string{
		"otelcol/1 config-changed none retries=0 procs=1",
		// failed twice in charm init, then Juju's retry succeeded
		"otelcol/1 config-changed retried retries=2 procs=3",
		"otelcol/0 reconcile action none retries=0 procs=1",
		// killed mid-hook, then retried
		"otelcol/1 config-changed retried retries=1 procs=2",
	}, "\n")
	if got != want {
		t.Fatalf("runs:\n%s\nwant:\n%s", got, want)
	}
	for _, r := range runs {
		if r.RepSpan != r.OpsSpans[len(r.OpsSpans)-1] {
			t.Errorf("%s: RepSpan %s, want its last process %s", r.Name, r.RepSpan, r.OpsSpans[len(r.OpsSpans)-1])
		}
	}
}

func TestOpsOnlyRunStillFailing(t *testing.T) {
	spans := opsFixture(t, "machine")
	runs := BuildHookRuns(spans)
	checkEveryRootInOneRun(t, spans, runs)
	got := runLines(runs)
	want := strings.Join([]string{
		"opentelemetry-collector/0 config-changed none retries=0 procs=1",
		// the recording kept only the first failed attempt: still in error
		"opentelemetry-collector/0 config-changed errored retries=0 procs=1",
		"ubuntu/0 update-status none retries=0 procs=1",
	}, "\n")
	if got != want {
		t.Fatalf("runs:\n%s\nwant:\n%s", got, want)
	}

	// Unit-only recordings have hook rows at all.
	var hooks []string
	for _, ev := range BuildEventsWithStatus(spans, nil, nil) {
		if ev.Kind == EvHook {
			hooks = append(hooks, ev.Unit+" "+ev.Summary+" "+ev.Fail.String())
		}
	}
	if len(hooks) != 3 || hooks[1] != "opentelemetry-collector/0 config-changed errored" {
		t.Errorf("hook events: %v", hooks)
	}
}

// marker is a Uniter.SetState span carrying uniter state us, at ts.
func marker(id, unit string, ts time.Time, us string) recording.SpanRow {
	sp := setStateSpan(id, unit, 0, us)
	sp.Start, sp.End = ts, ts
	return sp
}

// TestOpsJoinRPCRuns gives each recorded hook process the run-hook and
// continue markers the uniter would have written around it, as a host,unit
// recording has: each process joins its run, and none makes a run of its own.
func TestOpsJoinRPCRuns(t *testing.T) {
	var ops []recording.SpanRow
	for _, s := range opsFixture(t, "fake") {
		if s.Model == "lab" && strings.Contains(s.RawFile, "ops-3.9.0") {
			ops = append(ops, s)
		}
	}
	spans := append([]recording.SpanRow(nil), ops...)
	byKind := map[string]string{} // hook kind -> its run-hook marker id
	var last time.Time
	for i, root := range opsRoots(ops) {
		if root.Attrs["exec"] != recording.ExecHook {
			continue // actions have no run-hook bracket
		}
		kind := root.Attrs["hook_kind"]
		us := runHook(kind, "pending")
		if kind == "relation-changed" {
			us += "  remote-application: remote\n"
		}
		id := "rh" + itoa(i)
		byKind[root.Name] = id
		spans = append(spans,
			marker(id, "fake/0", root.Start.Add(-80*time.Millisecond), us),
			marker("c"+itoa(i), "fake/0", root.End.Add(40*time.Millisecond), continueDone))
		last = root.End
	}
	// A later config-changed no process belongs to.
	spans = append(spans,
		marker("late", "fake/0", last.Add(10*time.Second), runHook("config-changed", "pending")),
		marker("late-c", "fake/0", last.Add(11*time.Second), continueDone))
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })

	runs := BuildHookRuns(spans)
	checkEveryRootInOneRun(t, ops, runs)
	rootByID := map[string]recording.SpanRow{}
	for _, r := range opsRoots(ops) {
		rootByID[r.SpanID] = r
	}
	var opsOnly []string
	for _, r := range runs {
		switch {
		case r.RepSpan == "late":
			if len(r.OpsSpans) != 0 {
				t.Errorf("the config-changed no process ran in has processes %v", r.OpsSpans)
			}
		case strings.HasPrefix(r.RepSpan, "rh"):
			if len(r.OpsSpans) != 1 {
				t.Fatalf("run %s: processes %v, want 1", r.Kind, r.OpsSpans)
			}
			root := rootByID[r.OpsSpans[0]]
			if byKind[root.Name] != r.RepSpan || r.Name != root.Name {
				t.Errorf("run %s (%s) got process %q", r.RepSpan, r.Kind, root.Name)
			}
		default:
			opsOnly = append(opsOnly, r.Name)
		}
	}
	// The relation hook reads by its endpoint without the relation keys RPC
	// spans carry; the action, with no bracket, is a run of its own.
	if strings.Join(opsOnly, ",") != "do-thing action" {
		t.Errorf("runs made from processes alone: %v, want only the action", opsOnly)
	}
}

// TestOpsJoinRetriedBracket: Juju retries a failing hook inside one run-hook
// bracket; every attempt's process joins that one run, which counts the
// attempts as retries even when the markers don't show them.
func TestOpsJoinRetriedBracket(t *testing.T) {
	ops := opsFixture(t, "k8s")
	var attempts []recording.SpanRow
	for _, r := range opsRoots(ops) {
		if r.Unit == "otelcol/1" {
			attempts = append(attempts, r)
		}
	}
	// attempts[1:4]: failed, failed, succeeded.
	at := func(i int) time.Time { return attempts[i].Start.Add(-50 * time.Millisecond) }
	end := attempts[3].End.Add(50 * time.Millisecond)
	for _, tc := range []struct {
		name    string
		markers []recording.SpanRow
	}{
		{"a pending marker per attempt", []recording.SpanRow{
			marker("rh", "otelcol/1", at(1), runHook("config-changed", "pending")),
			marker("rh2", "otelcol/1", at(2), runHook("config-changed", "pending")),
			marker("rh3", "otelcol/1", at(3), runHook("config-changed", "pending")),
			marker("c", "otelcol/1", end, continueDone),
		}},
		// What Juju 3.6 wrote on an LXD machine: pending once, done once.
		{"one marker for every attempt", []recording.SpanRow{
			marker("rh", "otelcol/1", at(1), runHook("config-changed", "pending")),
			marker("rh2", "otelcol/1", end.Add(-time.Millisecond), runHook("config-changed", "done")),
			marker("c", "otelcol/1", end, continueDone),
		}},
	} {
		spans := append(append([]recording.SpanRow(nil), ops...), tc.markers...)
		sort.SliceStable(spans, func(i, j int) bool { return spans[i].Start.Before(spans[j].Start) })
		runs := BuildHookRuns(spans)
		checkEveryRootInOneRun(t, ops, runs)
		var got *HookRun
		for i := range runs {
			if runs[i].RepSpan == "rh" {
				got = &runs[i]
			}
		}
		want := []string{attempts[1].SpanID, attempts[2].SpanID, attempts[3].SpanID}
		if got == nil || strings.Join(got.OpsSpans, ",") != strings.Join(want, ",") ||
			got.Retries != 2 || got.Fail != FailRetried {
			t.Fatalf("%s: run %+v, want processes %v, 2 retries, retried", tc.name, got, want)
		}
	}
}

// TestOpsJoinFailedOpenBracket: the hook failed and Juju hasn't retried it
// yet when the recording ends: the unit is in error.
func TestOpsJoinFailedOpenBracket(t *testing.T) {
	all := opsFixture(t, "k8s")
	var failed recording.SpanRow
	for _, r := range opsRoots(all) {
		if r.Unit == "otelcol/1" && r.StatusCode == "ERROR" {
			failed = r
			break
		}
	}
	var spans []recording.SpanRow
	for _, s := range all {
		if s.TraceID == failed.TraceID {
			spans = append(spans, s)
		}
	}
	spans = append([]recording.SpanRow{
		marker("rh", "otelcol/1", failed.Start.Add(-50*time.Millisecond), runHook("config-changed", "pending")),
	}, spans...)
	runs := BuildHookRuns(spans)
	if len(runs) != 1 || !runs[0].Open || runs[0].Fail != FailErrored || len(runs[0].OpsSpans) != 1 {
		t.Fatalf("runs %+v, want one open errored run with the failed process", runs)
	}
}
