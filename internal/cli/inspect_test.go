package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/inspect"
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
		{"events", dir, "--kind", "hooks"},
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
