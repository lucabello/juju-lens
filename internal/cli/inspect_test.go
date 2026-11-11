package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/inspect"
	"github.com/lucabello/juju-lens/internal/query"
	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/synth"
)

// opsRecording is the synthetic unit-only recording: six hooks of two otelcol
// units, profiled, one errored and one retried.
func opsRecording(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioOps, synthFlags{output: dir, model: "default"}); err != nil {
		t.Fatalf("runSynth: %v", err)
	}
	return dir
}

// runInspect runs `juju-lens inspect ...` and returns stdout and stderr.
func runInspect(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd(BuildInfo{Version: "test"})
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(append([]string{"inspect"}, args...))
	err = root.Execute()
	return out.String(), errb.String(), err
}

func TestInspectEventsFiltersAndRanks(t *testing.T) {
	dir := opsRecording(t)

	var list inspect.EventList
	out, _, err := runInspect(t, "events", dir, "--json", "--sort", "cpu")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 6 || len(list.Events) != 6 {
		t.Fatalf("total %d, %d rows; want the 6 hooks", list.Total, len(list.Events))
	}
	for i := 1; i < len(list.Events); i++ {
		if list.Events[i].CPUMS > list.Events[i-1].CPUMS {
			t.Errorf("--sort cpu: row %d has %.0f ms after %.0f ms", i, list.Events[i].CPUMS, list.Events[i-1].CPUMS)
		}
	}
	if !list.Events[0].Profiled || list.Events[0].TopFrame == "" {
		t.Errorf("the costliest hook has no cost: %+v", list.Events[0])
	}

	out, _, err = runInspect(t, "events", dir, "--json", "--fail", "problem", "--unit", "otelcol/0")
	if err != nil {
		t.Fatal(err)
	}
	list = inspect.EventList{}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || list.Events[0].Fail != "retried" || list.Events[0].Unit != "otelcol/0" {
		t.Errorf("--fail problem --unit otelcol/0: %+v, want the one retried hook", list.Events)
	}
}

// A list says how much it left out and how to see the rest, and what to run
// next, so an agent that reads only its output finds the way on.
func TestInspectEventsBoundsItsOutput(t *testing.T) {
	dir := opsRecording(t)
	out, _, err := runInspect(t, "events", dir, "--limit", "2", "--skip", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"showing 2-3 of 6 events; --skip 3 for more", "next: juju-lens inspect event " + dir, "inspect profile " + dir + " --span "} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "otelcol/"); n != 2 {
		t.Errorf("%d event rows, want 2:\n%s", n, out)
	}
}

func TestInspectRejectsValuesThatMatchNothing(t *testing.T) {
	dir := opsRecording(t)
	for _, tc := range [][]string{
		{"events", dir, "--fail", "broken"},
		{"events", dir, "--sort", "size"},
		{"event", dir, "no-such-span"},
		{"profile", dir, "--metric", "bytes"},
		{"profile", dir, "peek"},
	} {
		if _, _, err := runInspect(t, tc...); err == nil {
			t.Errorf("inspect %v: want an error", tc)
		}
	}
}

func TestInspectEventSectionsAndInnerSpans(t *testing.T) {
	dir := opsRecording(t)
	var list inspect.EventList
	out, _, _ := runInspect(t, "events", dir, "--json", "--sort", "cpu", "--limit", "1")
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	id := list.Events[0].SpanID

	full, _, err := runInspect(t, "event", dir, id)
	if err != nil {
		t.Fatal(err)
	}
	only, _, err := runInspect(t, "event", dir, id, "--section", "statuses")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "top stacks by wall time") || strings.Contains(only, "top stacks") {
		t.Errorf("--section statuses should drop the stacks:\nfull:\n%s\nonly:\n%s", full, only)
	}

	// The span id of an observer, as a profile prints it, leads to its hook.
	prof, _, err := runInspect(t, "profile", dir, "--span", id, "stacks", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var pj inspect.ProfileJSON
	if err := json.Unmarshal([]byte(prof), &pj); err != nil {
		t.Fatal(err)
	}
	inner := pj.Stacks[0].SpanID
	if inner == id {
		t.Fatalf("stack span %s is the event itself", inner)
	}
	out, _, err = runInspect(t, "event", dir, inner)
	if err != nil || !strings.Contains(out, "is inside this hook") {
		t.Errorf("inspect event of an inner span: %v\n%s", err, out)
	}
}

func TestInspectProfileViews(t *testing.T) {
	dir := opsRecording(t)

	var top inspect.ProfileJSON
	out, _, err := runInspect(t, "profile", dir, "--json", "--unit", "otelcol/0", "--order", "total")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatal(err)
	}
	if len(top.Functions) == 0 || top.Samples == 0 {
		t.Fatalf("empty profile: %+v", top)
	}
	for i := 1; i < len(top.Functions); i++ {
		if top.Functions[i].Total > top.Functions[i-1].Total {
			t.Errorf("--order total: %s (%.0f) after %s (%.0f)", top.Functions[i].Function, top.Functions[i].Total,
				top.Functions[i-1].Function, top.Functions[i-1].Total)
		}
	}

	// peek names what called a function and what it called, and a name that
	// matches several functions is refused with the candidates.
	out, _, err = runInspect(t, "profile", dir, "peek", "_reconcile", "--unit", "otelcol/0")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"called by:", "calls:", "__init__"} {
		if !strings.Contains(out, want) {
			t.Errorf("peek lacks %q:\n%s", want, out)
		}
	}
	if _, _, err := runInspect(t, "profile", dir, "peek", "readinto", "--unit", "otelcol/0"); err != nil {
		t.Errorf("peek of a function with one match: %v", err)
	}
	if _, _, err := runInspect(t, "profile", dir, "peek", "_", "--unit", "otelcol/0"); err == nil || !strings.Contains(err.Error(), "matches") {
		t.Errorf("peek of an ambiguous name: %v", err)
	}

	// The memory view says what it counts.
	out, _, err = runInspect(t, "profile", dir, "--metric", "mem", "tree", "--unit", "otelcol/0")
	if err != nil || !strings.Contains(out, "not where it was held") || !strings.Contains(out, "compress") {
		t.Errorf("memory tree: %v\n%s", err, out)
	}
}

// A hook without samples says why there is no profile, rather than printing
// an empty table.
func TestInspectProfileWithoutSamples(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir, model: "default"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := runInspect(t, "profile", dir)
	if err == nil || !strings.Contains(err.Error(), "--attach unit") {
		t.Fatalf("want an error pointing at --attach unit, got %v", err)
	}
}

// inspect reads recordings the other commands would refuse: with no index,
// or an index from an older juju-lens. It builds the index and says so.
func TestInspectIndexesWhenNeeded(t *testing.T) {
	dir := opsRecording(t)
	layout := recording.NewLayout(dir)

	if err := os.Remove(layout.IndexDB()); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runInspect(t, "events", dir, "--limit", "1")
	if err != nil {
		t.Fatalf("no index: %v", err)
	}
	if !strings.Contains(errOut, "has no index") || !strings.Contains(out, "otelcol/") {
		t.Errorf("no index: notice %q, output %q", errOut, out)
	}
	if v, ok := index.StoredSchemaVersion(layout.IndexDB()); !ok || v != index.SchemaVersion {
		t.Errorf("after indexing: schema %d, ok=%v", v, ok)
	}

	_, errOut, _ = runInspect(t, "events", dir, "--limit", "1")
	if strings.Contains(errOut, "indexing") {
		t.Errorf("a current index was rebuilt: %q", errOut)
	}

	db, err := index.Open(layout.IndexDB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().Exec(`UPDATE meta SET value = '3' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, errOut, err = runInspect(t, "events", dir, "--limit", "1"); err != nil || !strings.Contains(errOut, "index of schema 3") {
		t.Errorf("old schema: err %v, notice %q", err, errOut)
	}
}

func TestInspectLeavesALiveRecordersIndexAlone(t *testing.T) {
	dir := opsRecording(t)
	layout := recording.NewLayout(dir)
	db, err := index.Open(layout.IndexDB())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = db.SQL().Exec(`UPDATE meta SET value = '3' WHERE key = 'schema_version'`)
	db.Close()
	if err := recording.WritePidFile(layout.PidFile(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	defer recording.RemovePidFile(layout.PidFile())
	// The index is of an old schema, but a recorder is writing it: leave it.
	if _, errOut, _ := runInspect(t, "events", dir, "--limit", "1"); strings.Contains(errOut, "indexing") {
		t.Errorf("rebuilt the index of a recording that is being written: %q", errOut)
	}
}

// A recording the user can't write to, with no index, can't be indexed in
// place; the error says what to do instead of a bare permission error.
func TestInspectExplainsAnUnwritableRecording(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := opsRecording(t)
	if err := os.Remove(recording.NewLayout(dir).IndexDB()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	_, _, err := runInspect(t, "events", dir)
	if err == nil || !strings.Contains(err.Error(), "can't write there to index it") || !strings.Contains(err.Error(), "juju-lens index") {
		t.Fatalf("want an explanation, got %v", err)
	}
}

// fixtureRecording copies a lab recording of the tap's records, which has no
// manifest, and gives it one.
func fixtureRecording(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "recording", "testdata", "ops", name))); err != nil {
		t.Fatal(err)
	}
	man := recording.New("juju-lens", "test", "", name)
	man.Finalize(recording.EndReasonSynth)
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The memory of a hook says how the process and the containers moved, and
// names the observer and the code running at the biggest jump.
func TestInspectMemoryNamesTheCodeThatTookIt(t *testing.T) {
	dir := fixtureRecording(t, "cost-k8s")
	var list inspect.EventList
	out, _, err := runInspect(t, "events", dir, "--json", "--name", "config-changed")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Events) == 0 {
		t.Fatalf("events: %v %s", err, out)
	}
	out, _, err = runInspect(t, "memory", dir, list.Events[0].SpanID, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep inspect.MemoryReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	rows := map[string]inspect.MemoryRow{}
	for _, r := range rep.Rows {
		rows[r.Name] = r
	}
	if hp := rows["hook process"]; hp.HighestBytes < hp.BeforeBytes+50<<20 {
		t.Errorf("hook process %+v: the lab hook grew by about 70 MB", hp)
	}
	if _, ok := rows["charm container"]; !ok {
		t.Errorf("no charm container row: %+v", rep.Rows)
	}
	found := false
	for _, j := range rep.Jumps {
		if j.Series == "hook process" && strings.Contains(j.During, "observer") && strings.Contains(strings.Join(j.Code, " "), "compress") {
			found = true
		}
	}
	if !found {
		t.Errorf("no hook process jump in an observer, in compress: %+v", rep.Jumps)
	}
	text, _, _ := runInspect(t, "memory", dir, list.Events[0].SpanID)
	if !strings.Contains(text, "code: compress") || !strings.Contains(text, "next: juju-lens inspect profile") {
		t.Errorf("text lacks the code or the next step:\n%s", text)
	}
}

// A recording of the host side only has no memory readings; the error says
// where they come from.
func TestInspectMemoryWithoutUnitCapture(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir, model: "default"}); err != nil {
		t.Fatal(err)
	}
	out, _, _ := runInspect(t, "events", dir, "--json", "--limit", "1")
	var list inspect.EventList
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Events) == 0 {
		t.Fatalf("events: %v", err)
	}
	if _, _, err := runInspect(t, "memory", dir, list.Events[0].SpanID); err == nil || !strings.Contains(err.Error(), "--attach unit") {
		t.Errorf("want an error pointing at --attach unit, got %v", err)
	}
}

// The trace of a hook nests its spans and functions, leaves short rows out
// unless asked, and says which process of a retried hook it shows.
func TestInspectTrace(t *testing.T) {
	dir := fixtureRecording(t, "cost-k8s")
	var list inspect.EventList
	out, _, _ := runInspect(t, "events", dir, "--json", "--name", "config-changed")
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Events) == 0 {
		t.Fatalf("events: %v", err)
	}
	id := list.Events[0].SpanID

	var compact, full inspect.TraceReport
	for _, tc := range []struct {
		args []string
		into *inspect.TraceReport
	}{{[]string{"trace", dir, id, "--json", "--limit", "1000"}, &compact}, {[]string{"trace", dir, id, "--json", "--all", "--limit", "1000"}, &full}} {
		out, _, err := runInspect(t, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(out), tc.into); err != nil {
			t.Fatal(err)
		}
	}
	if len(compact.Rows) == 0 || len(full.Rows) <= len(compact.Rows) {
		t.Errorf("%d rows compact, %d with --all: --all should show more", len(compact.Rows), len(full.Rows))
	}
	var init, reconcile int
	for i, r := range compact.Rows {
		if r.Kind == "init" || strings.Contains(r.Label, "__init__") {
			init = i + 1
		}
		if strings.Contains(r.Label, "_reconcile") {
			reconcile = i + 1
			if r.Depth <= compact.Rows[max(init-1, 0)].Depth {
				t.Errorf("_reconcile at depth %d isn't under init at depth %d", r.Depth, compact.Rows[max(init-1, 0)].Depth)
			}
		}
	}
	if init == 0 || reconcile <= init {
		t.Errorf("want init, then _reconcile under it; got rows %+v", compact.Rows[:min(6, len(compact.Rows))])
	}

	out, _, _ = runInspect(t, "trace", dir, id, "--depth", "1", "--limit", "5")
	if !strings.Contains(out, "next: juju-lens inspect memory") {
		t.Errorf("no next step:\n%s", out)
	}
	if _, _, err := runInspect(t, "trace", dir, id, "--attempt", "2"); err == nil || !strings.Contains(err.Error(), "ran in 1 process") {
		t.Errorf("--attempt 2 of a one-process hook: %v", err)
	}
}

func TestInspectLogsFiltersAndCorrelates(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir, model: "default"}); err != nil {
		t.Fatal(err)
	}
	logs := func(args ...string) inspect.LogList {
		t.Helper()
		out, _, err := runInspect(t, append([]string{"logs", dir, "--json"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		var l inspect.LogList
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}

	all := logs()
	if all.Total != 13 {
		t.Fatalf("total %d, want the synthetic recording's 13 lines", all.Total)
	}
	for i := 1; i < len(all.Logs); i++ {
		if all.Logs[i].TS.Before(all.Logs[i-1].TS) {
			t.Fatalf("line %d is before line %d", i, i-1)
		}
	}
	if g := logs("--unit", "grafana"); g.Total == 0 || g.Total >= all.Total {
		t.Errorf("--unit grafana: %d of %d lines", g.Total, all.Total)
	} else {
		for _, r := range g.Logs {
			if !strings.HasPrefix(r.Unit, "grafana/") {
				t.Errorf("--unit grafana returned a line of %q", r.Unit)
			}
		}
	}
	if d := logs("--level", "debug"); d.Total == 0 || d.Logs[0].Level != "DEBUG" {
		t.Errorf("--level debug: %+v", d.Logs)
	}
	if r := logs("--grep", "RECONCILING"); r.Total == 0 || !strings.Contains(r.Logs[0].Body, "Reconciling") {
		t.Errorf("--grep ignoring case: %+v", r.Logs)
	}
	if p := logs("--limit", "2", "--skip", "1"); len(p.Logs) != 2 || p.Skip != 1 || p.Logs[0].TS != all.Logs[1].TS {
		t.Errorf("--limit 2 --skip 1: %+v", p)
	}

	// The lines about an event are those near its hook, from its unit.
	var ev inspect.EventList
	out, _, _ := runInspect(t, "events", dir, "--json", "--unit", "grafana/0", "--name", "install")
	if err := json.Unmarshal([]byte(out), &ev); err != nil || len(ev.Events) == 0 {
		t.Fatalf("events: %v", err)
	}
	about := logs("--span", ev.Events[0].SpanID)
	if about.Total == 0 || about.Event != "grafana/0 install" {
		t.Fatalf("--span: %+v", about)
	}
	for _, r := range about.Logs {
		if r.Unit != "grafana/0" || r.Matched == "" {
			t.Errorf("--span returned %+v", r)
		}
	}
	for _, bad := range [][]string{{"--source", "syslog"}, {"--since", "yesterday"}, {"--span", "nope"}} {
		if _, _, err := runInspect(t, append([]string{"logs", dir}, bad...)...); err == nil {
			t.Errorf("inspect logs %v: want an error", bad)
		}
	}
}

func TestInspectDiffNamesTheFunctionsThatChanged(t *testing.T) {
	dir := opsRecording(t)
	diff := func(args ...string) inspect.Diff {
		t.Helper()
		out, _, err := runInspect(t, append([]string{"diff", "--json"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		var d inspect.Diff
		if err := json.Unmarshal([]byte(out), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	// Two hooks of one recording: the retried config-changed took about
	// three times the time of the first, in the same functions.
	d := diff(dir, "--span-a", "7bfdb120d1995d97", "--span-b", "5ea2a76a77e2a8be")
	if d.A.Hooks != 1 || d.B.Hooks != 1 || len(d.Functions) == 0 {
		t.Fatalf("diff: %+v", d)
	}
	top := d.Functions[0]
	if top.Delta <= 0 || top.B <= top.A {
		t.Errorf("biggest change %+v: the second hook is the slower", top)
	}
	for i := 1; i < len(d.Functions); i++ {
		if abs(d.Functions[i].Delta) > abs(d.Functions[i-1].Delta) {
			t.Errorf("row %d changed more than row %d", i, i-1)
		}
	}

	// A recording against itself changes nothing, per hook, whatever the
	// scope's size.
	same := diff(dir, dir, "--unit", "otelcol/0", "--name", "config-changed")
	if same.Total != 0 || same.A.Hooks != 2 {
		t.Errorf("a recording against itself: %d functions differ, %d hooks", same.Total, same.A.Hooks)
	}
	if _, _, err := runInspect(t, "diff", dir); err == nil || !strings.Contains(err.Error(), "--span-a") {
		t.Errorf("one recording needs two spans: %v", err)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestInspectSummaryScopes(t *testing.T) {
	dir := opsRecording(t)
	all, _, err := runInspect(t, "summary", dir)
	if err != nil {
		t.Fatal(err)
	}
	one, _, err := runInspect(t, "summary", dir, "--unit", "otelcol/1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "6 events") || !strings.Contains(one, "2 events,") || strings.Contains(one, "otelcol/0") {
		t.Errorf("--unit otelcol/1 should summarize its 2 events only:\n%s", one)
	}
	for _, want := range []string{"errored 1, retried 1", "slowest events:", "most CPU in the charm's code", "next: juju-lens inspect events"} {
		if !strings.Contains(all, want) {
			t.Errorf("summary lacks %q:\n%s", want, all)
		}
	}
	// A unit-only recording has no settle markers; it must not claim its
	// units never reached idle.
	if strings.Contains(strings.ToLower(all), "idle") {
		t.Errorf("summary talks about idleness:\n%s", all)
	}
	var js inspect.Summary
	jout, _, err := runInspect(t, "summary", dir, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(jout), &js); err != nil || js.Events != 6 || len(js.Units) != 2 || js.Outcomes["retried"] != 1 {
		t.Errorf("summary --json: %v %+v", err, js)
	}
}

// Flags that can't be used together, or that mean nothing alone, are
// refused instead of being ignored.
func TestInspectRefusesFlagsThatWouldBeIgnored(t *testing.T) {
	dir := opsRecording(t)
	var list inspect.EventList
	out, _, _ := runInspect(t, "events", dir, "--json", "--limit", "1")
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatal(err)
	}
	id := list.Events[0].SpanID
	for _, tc := range [][]string{
		{"logs", dir, "--window", "5s"},
		{"logs", dir, "--span", id, "--around", "2026-07-03T14:30:20Z"},
		{"logs", dir, "--level", "loud"},
		{"trace", dir, id, "--attempt", "0"},
	} {
		if _, _, err := runInspect(t, tc...); err == nil {
			t.Errorf("inspect %v: want an error", tc)
		}
	}
	// --limit 0 shows every row of a trace.
	var tr inspect.TraceReport
	out, _, err := runInspect(t, "trace", dir, id, "--json", "--all", "--limit", "0")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(out), &tr); err != nil || len(tr.Rows) != tr.Total {
		t.Errorf("--limit 0: %d rows of %d (%v)", len(tr.Rows), tr.Total, err)
	}
}

// costs adds up what the details files of the events say, grouped as asked.
func TestInspectCostsSumTheHooks(t *testing.T) {
	dir := opsRecording(t)
	src, err := query.OpenSource(dir, "default")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var want, wantMem float64
	obs := map[string]float64{}
	for _, ev := range src.Events(query.Options{}) {
		for _, c := range src.Detail(ev).Costs {
			want += c.CPUMS
			wantMem += float64(c.StackMemGrowthBytes)
			obs[c.Name] += c.CPUMS
		}
	}
	costs := func(args ...string) inspect.CostList {
		t.Helper()
		out, _, err := runInspect(t, append([]string{"costs", dir, "--json", "--limit", "0"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		var l inspect.CostList
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	sum := func(l inspect.CostList) (cpu, mem float64) {
		for _, r := range l.Costs {
			cpu += r.CPUMS
			mem += float64(r.MemTaken)
		}
		return
	}
	for _, by := range []string{"observer", "event", "unit", "app"} {
		l := costs("--by", by)
		if cpu, mem := sum(l); abs(cpu-want) > 0.01 || abs(mem-wantMem) > 1 {
			t.Errorf("--by %s adds up to %.1f ms and %.0f bytes, the details files to %.1f and %.0f", by, cpu, mem, want, wantMem)
		}
	}
	l := costs("--by", "observer")
	for _, r := range l.Costs {
		if r.Kind != "init" && abs(r.CPUMS-obs[r.Name]) > 0.01 {
			t.Errorf("observer %s: %.1f ms, details say %.1f", r.Name, r.CPUMS, obs[r.Name])
		}
	}
	for i := 1; i < len(l.Costs); i++ {
		if l.Costs[i].CPUMS > l.Costs[i-1].CPUMS {
			t.Errorf("row %d has more CPU than row %d", i, i-1)
		}
	}
	if a := costs("--by", "app"); len(a.Costs) != 1 || a.Costs[0].Name != "otelcol" {
		t.Errorf("--by app: %+v", a.Costs)
	}
	if _, _, err := runInspect(t, "costs", dir, "--by", "span"); err == nil {
		t.Error("an unknown --by should fail")
	}
}
