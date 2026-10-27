package synth

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// opsSpans assembles the ops scenario's unit records into span trees, as
// indexing its raw/unit files does.
func opsSpans(t *testing.T) []recording.SpanRow {
	t.Helper()
	recs, err := UnitRecords(ScenarioOps, Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := recording.NewOpsAssembler()
	var spans []recording.SpanRow
	for i, ur := range recs {
		line, err := json.Marshal(ur.Rec)
		if err != nil {
			t.Fatal(err)
		}
		spans = append(spans, a.Feed("default", line, "raw/unit/default/"+ur.Host+"/unit-2026-07-03T14.jsonl", int64(i))...)
	}
	return append(spans, a.Pending()...)
}

func TestUnitRecordsOpsIsDeterministic(t *testing.T) {
	a, err := UnitRecords(ScenarioOps, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := UnitRecords(ScenarioOps, Options{})
	if !reflect.DeepEqual(a, b) {
		t.Fatal("unit records are not deterministic for default options")
	}
	if _, err := UnitRecords(ScenarioTrivial, Options{}); err == nil {
		t.Error("trivial has unit records")
	}
}

// TestOpsScenarioShape: the scenario's hooks come out of the same assembly
// and run derivation a live unit-only recording goes through, with the
// tree features the viewer shows.
func TestOpsScenarioShape(t *testing.T) {
	spans := opsSpans(t)
	runs := narrative.BuildHookRuns(spans)
	var got []string
	for _, r := range runs {
		got = append(got, fmt.Sprintf("%s %s %s procs=%d", r.Unit, r.Name, r.Fail, len(r.OpsSpans)))
	}
	want := []string{
		"otelcol/0 config-changed none procs=1",
		"otelcol/0 receive-loki-logs-relation-changed none procs=1",
		"otelcol/0 update-status none procs=1",
		"otelcol/0 config-changed retried procs=3",
		"otelcol/1 config-changed none procs=1",
		"otelcol/1 update-status errored procs=1",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("runs:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	x := narrative.NewOpsIndex(spans)
	marks := map[string]int{}
	var walk func(n *narrative.OpsNode, parent string)
	walk = func(n *narrative.OpsNode, parent string) {
		for _, m := range n.Marks {
			marks[m]++
		}
		if n.Kind == recording.OpsRoot && parent != "" {
			marks["subprocess under "+parent]++
		}
		if n.Kind == recording.OpsObserver && n.Error != "" {
			marks["failed observer"]++
		}
		for _, c := range n.Children {
			walk(c, n.Kind)
		}
	}
	for _, r := range runs {
		for _, tr := range x.Trees(r.OpsSpans) {
			walk(tr, "")
		}
	}
	for _, want := range []string{
		narrative.MarkDeferred, narrative.MarkReemitted, narrative.MarkKilled, narrative.MarkUnfinished,
		"subprocess under observer", "failed observer",
	} {
		if marks[want] == 0 {
			t.Errorf("no %q in the trees: %v", want, marks)
		}
	}
}

// TestOpsScenarioMeasurements: every hook process has a profile whose
// stacks hang off its spans, and the probe's resource samples show the
// collector's memory rising after the hook whose replan restarted it.
func TestOpsScenarioMeasurements(t *testing.T) {
	recs, err := UnitRecords(ScenarioOps, Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := recording.NewOpsAssembler()
	rr := recording.NewResourceReader()
	var (
		spans []recording.SpanRow
		res   recording.UnitResources
	)
	for i, ur := range recs {
		line, _ := json.Marshal(ur.Rec)
		rel := "raw/unit/default/" + ur.Host + "/unit-2026-07-03T14.jsonl"
		spans = append(spans, a.Feed("default", line, rel, int64(i))...)
		rr.Feed("default", line, rel, &res)
	}
	spans = append(spans, a.Pending()...)
	profs := a.TakeProfiles()
	byID := map[string]recording.SpanRow{}
	hooks := 0
	for _, sp := range spans {
		byID[sp.SpanID] = sp
		if sp.Attrs["kind"] == recording.OpsRoot && sp.ParentSpanID == "" {
			hooks++
		}
	}
	if len(profs) != hooks {
		t.Fatalf("%d profiles for %d hook processes", len(profs), hooks)
	}
	for _, p := range profs {
		kinds := map[string]bool{}
		for _, s := range p.Stacks {
			kinds[byID[s.SpanID].Attrs["kind"]] = true
		}
		if !kinds[recording.OpsInit] || len(p.Timeline) == 0 {
			t.Errorf("profile of %s: stacks in %v, %d samples", byID[p.RootSpanID].Name, kinds, len(p.Timeline))
		}
	}
	if len(res.Changes) == 0 {
		t.Fatal("no Pebble changes")
	}
	replan := res.Changes[0]
	var atReady, later int64
	for _, s := range res.Samples {
		if s.Container != "otelcol" || s.Host != "otelcol-0" {
			continue
		}
		switch d := s.TS.Sub(replan.Spawn); {
		case d > 0 && atReady == 0 && d < 3*time.Second && s.TS.After(replan.Spawn.Add(1300*time.Millisecond)):
			atReady = int64(s.MemBytes)
		case d > 10*time.Second && later == 0:
			later = int64(s.MemBytes)
		}
	}
	if atReady == 0 || later <= atReady*2 {
		t.Errorf("collector memory %d just after the restart, %d 10s later: want it to climb", atReady, later)
	}
}
