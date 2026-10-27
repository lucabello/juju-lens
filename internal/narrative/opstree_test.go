package narrative

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// runNamed returns the n-th run (from 0, by start) of hook name.
func runNamed(t *testing.T, runs []HookRun, name string, n int) HookRun {
	t.Helper()
	for _, r := range runs {
		if r.Name == name {
			if n == 0 {
				return r
			}
			n--
		}
	}
	t.Fatalf("no run %q", name)
	return HookRun{}
}

// row is the structure of one OpsLine, without durations.
func row(l OpsLine) string {
	s := fmt.Sprintf("%d %s %s", l.Depth, l.Kind, l.Label)
	if len(l.Marks) > 0 {
		s += " [" + strings.Join(l.Marks, ",") + "]"
	}
	return s
}

func rows(lines []OpsLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = row(l)
	}
	return out
}

func TestOpsTreeDeferredAndReemitted(t *testing.T) {
	spans := opsFixture(t, "fake")
	x := NewOpsIndex(spans)
	runs := BuildHookRuns(spans)

	// config-changed: the observer defers the event.
	got := rows(OpsLines(x.Trees(runNamed(t, runs, "config-changed", 0).OpsSpans), false))
	for _, want := range []string{
		"1 event config_changed [deferred]",
		"2 observer FakeCharm._on_config_changed [deferred]",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("config-changed: no row %q in\n%s", want, strings.Join(got, "\n"))
		}
	}

	// The next hook re-emits it from the backlog, before its own event; the
	// observer reached through the backlog isn't marked a second time, and
	// the custom event it emits nests under it.
	got = rows(OpsLines(x.Trees(runNamed(t, runs, "db-relation-changed", 0).OpsSpans), false))
	want := []string{
		"1 backlog deferred events (1)",
		"2 event config_changed [re-emitted]",
		"3 observer FakeCharm._on_config_changed",
		"4 event ready",
		"5 observer FakeCharm._on_ready",
	}
	i := slices.Index(got, want[0])
	if i < 0 || len(got) < i+len(want) || !slices.Equal(got[i:i+len(want)], want) {
		t.Errorf("db-relation-changed rows:\n%s\nwant in order:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if j := slices.Index(got, "1 event db_relation_changed"); j < i {
		t.Errorf("hook's own event at row %d, before the backlog at %d", j, i)
	}
}

func TestOpsTreeErrorOnInnermostSpan(t *testing.T) {
	spans := opsFixture(t, "fake")
	x := NewOpsIndex(spans)
	r := runNamed(t, BuildHookRuns(spans), "start", 0)
	var failed []string
	for _, l := range OpsLines(x.Trees(r.OpsSpans), false) {
		if l.Failed {
			failed = append(failed, row(l)+" error="+strings.SplitN(l.Error, ":", 2)[0])
		}
	}
	want := []string{
		"0 root start error=",
		"1 event start error=",
		"2 observer FakeCharm._on_start error=RuntimeError",
	}
	if !slices.Equal(failed, want) {
		t.Errorf("failed rows:\n%s\nwant:\n%s", strings.Join(failed, "\n"), strings.Join(want, "\n"))
	}
}

// TestOpsTreeCompactKeepsEveryCall: folding drops no call, and a folded
// call is a Pebble call, or a quick hook tool that didn't fail or set a
// status.
func TestOpsTreeCompactKeepsEveryCall(t *testing.T) {
	spans := opsFixture(t, "k8s")
	x := NewOpsIndex(spans)
	for _, r := range BuildHookRuns(spans) {
		trees := x.Trees(r.OpsSpans)
		all, compact := 0, 0
		for _, l := range OpsLines(trees, true) {
			if l.Kind == recording.OpsTool || l.Kind == recording.OpsPebble {
				all++
			}
		}
		for _, l := range OpsLines(trees, false) {
			switch l.Kind {
			case recording.OpsTool, recording.OpsPebble:
				compact++
			case OpsLineCalls:
				var n int
				if _, err := fmt.Sscanf(l.Label, "%d", &n); err != nil {
					t.Fatalf("calls row %q: %v", l.Label, err)
				}
				compact += n
			}
		}
		if all == 0 || compact != all {
			t.Errorf("%s %s: %d calls compact, %d in full", r.Unit, r.Name, compact, all)
		}
	}

	var folded []*OpsNode
	var walk func(n *OpsNode)
	walk = func(n *OpsNode) {
		for _, c := range n.Children {
			if isCall(c) && !keepCall(c) {
				folded = append(folded, c)
			}
			walk(c)
		}
	}
	for _, tr := range x.Trees(BuildHookRuns(spans)[0].OpsSpans) {
		walk(tr)
	}
	for _, c := range folded {
		if c.Kind == recording.OpsTool && (c.Error != "" || c.Dur >= slowCall || c.Name == "status-set") {
			t.Errorf("folded a call worth its own row: %s %s %v", c.Kind, c.Name, c.Dur)
		}
	}
}

// TestOpsTreeNestsSubprocess: a subprocess sits under the observer that was
// running when it started, not at the top of its parent process.
func TestOpsTreeNestsSubprocess(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 19, 0, 0, 0, time.UTC)
	span := func(id, parent, kind, name string, from, to time.Duration) recording.SpanRow {
		return recording.SpanRow{
			SpanID: id, ParentSpanID: parent, TraceID: "t", Service: recording.ServiceOps, Name: name,
			Start: t0.Add(from), End: t0.Add(to), Attrs: map[string]string{"kind": kind},
		}
	}
	x := NewOpsIndex([]recording.SpanRow{
		span("root", "", recording.OpsRoot, "update-status", 0, 900*time.Millisecond),
		span("init", "root", recording.OpsInit, "charm init", 0, 100*time.Millisecond),
		span("ev", "root", recording.OpsEvent, "update_status", 200*time.Millisecond, 800*time.Millisecond),
		span("obs", "ev", recording.OpsObserver, "charm.Charm._on_update_status", 210*time.Millisecond, 790*time.Millisecond),
		span("tool", "obs", recording.OpsTool, "is-leader", 220*time.Millisecond, 230*time.Millisecond),
		span("sub", "root", recording.OpsRoot, "python render.py", 300*time.Millisecond, 500*time.Millisecond),
		span("pebble", "obs", recording.OpsPebble, "POST /v1/files", 600*time.Millisecond, 610*time.Millisecond),
	})
	got := rows(OpsLines(x.Trees([]string{"root"}), true))
	want := []string{
		"0 root update-status",
		"1 init charm __init__",
		"1 event update_status",
		"2 observer Charm._on_update_status",
		"3 tool is-leader",
		"3 root python render.py",
		"3 pebble POST /v1/files",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestOpsNames(t *testing.T) {
	spans := opsFixture(t, "fake")
	x := NewOpsIndex(spans)
	names := x.Names(runNamed(t, BuildHookRuns(spans), "config-changed", 0).OpsSpans)
	for _, want := range []string{"config_changed", "ConfigChangedEvent", "__main__.FakeCharm._on_config_changed", "commit"} {
		if !slices.Contains(names, want) {
			t.Errorf("names %v lack %q", names, want)
		}
	}
	if slices.Contains(names, "juju-log") || slices.Contains(names, "charm init") {
		t.Errorf("names %v hold calls or init", names)
	}
}

// TestUnitOnlyHookEventsCarryTheirProcesses: a hook row from a unit-only
// recording leads to the processes that ran it, like its run does.
func TestUnitOnlyHookEventsCarryTheirProcesses(t *testing.T) {
	spans := opsFixture(t, "k8s")
	runs := BuildHookRuns(spans)
	var hooks []Event
	for _, ev := range BuildEventsWithStatus(spans, nil, nil) {
		if ev.Kind == EvHook {
			hooks = append(hooks, ev)
		}
	}
	if len(hooks) != len(runs) {
		t.Fatalf("%d hook events, %d runs", len(hooks), len(runs))
	}
	for _, ev := range hooks {
		if len(ev.OpsSpans) == 0 || ev.Dur <= 0 || !Inspectable(ev) {
			t.Errorf("%s %s: ops spans %v, duration %v, inspectable %v", ev.Unit, ev.Summary, ev.OpsSpans, ev.Dur, Inspectable(ev))
		}
	}
}
