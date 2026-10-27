package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/export"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/synth"
)

func TestExportRejectsNonRecordingDir(t *testing.T) {
	dir := t.TempDir() // empty; no manifest
	err := runExport(dir, exportFlags{format: "md", model: "default"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "is this a juju-lens recording") {
		t.Fatalf("want 'is this a juju-lens recording' error, got %v", err)
	}
}

func TestExportJSONAndMarkdownEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir}); err != nil {
		t.Fatalf("runSynth: %v", err)
	}

	var md strings.Builder
	if err := runExport(dir, exportFlags{format: "md", model: "default"}, &md); err != nil {
		t.Fatalf("runExport md: %v", err)
	}
	if !strings.Contains(md.String(), "## Timeline") {
		t.Errorf("markdown export missing timeline section:\n%s", md.String())
	}

	var js strings.Builder
	if err := runExport(dir, exportFlags{format: "json", model: "default"}, &js); err != nil {
		t.Fatalf("runExport json: %v", err)
	}
	if !strings.Contains(js.String(), `"summary"`) {
		t.Errorf("json export missing summary field:\n%s", js.String())
	}
}

func TestExportRejectsUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir}); err != nil {
		t.Fatalf("runSynth: %v", err)
	}
	err := runExport(dir, exportFlags{format: "yaml", model: "default"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "unknown --format") {
		t.Fatalf("want 'unknown --format' error, got %v", err)
	}
}

// Without --format, export writes the folder shape (AGENTS.md, SUMMARY.md,
// timeline.jsonl, events.jsonl, details/, report.md) under
// <recording>/derived/export by default, rather than printing a single
// document to stdout.
func TestExportWritesFolderByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir}); err != nil {
		t.Fatalf("runSynth: %v", err)
	}
	var stdout strings.Builder
	if err := runExport(dir, exportFlags{model: "default"}, &stdout); err != nil {
		t.Fatalf("runExport: %v", err)
	}

	wantDir := recording.NewLayout(dir).ExportDir()
	if !strings.Contains(stdout.String(), wantDir) {
		t.Errorf("expected stdout to mention %s, got %q", wantDir, stdout.String())
	}
	for _, name := range []string{"AGENTS.md", "SUMMARY.md", "timeline.jsonl", "events.jsonl", "report.md"} {
		if _, err := os.Stat(filepath.Join(wantDir, name)); err != nil {
			t.Errorf("expected %s under %s: %v", name, wantDir, err)
		}
	}
	if _, err := os.Stat(filepath.Join(wantDir, "details")); err != nil {
		t.Errorf("expected details/ under %s: %v", wantDir, err)
	}
}

// --out and --parts respectively redirect the folder and restrict which
// artifacts land in it.
func TestExportOutAndParts(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir}); err != nil {
		t.Fatalf("runSynth: %v", err)
	}
	out := t.TempDir()
	var stdout strings.Builder
	f := exportFlags{model: "default", out: out, parts: []string{"summary", "timeline"}}
	if err := runExport(dir, f, &stdout); err != nil {
		t.Fatalf("runExport: %v", err)
	}
	for _, name := range []string{"SUMMARY.md", "timeline.jsonl", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("expected %s under --out dir: %v", name, err)
		}
	}
	for _, name := range []string{"events.jsonl", "report.md", "details"} {
		if _, err := os.Stat(filepath.Join(out, name)); !os.IsNotExist(err) {
			t.Errorf("expected %s NOT to exist for --parts summary,timeline, got err=%v", name, err)
		}
	}
}

func TestExportRejectsUnknownPart(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioTrivial, synthFlags{output: dir}); err != nil {
		t.Fatalf("runSynth: %v", err)
	}
	err := runExport(dir, exportFlags{model: "default", parts: []string{"bogus"}}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "unknown --parts value") {
		t.Fatalf("want 'unknown --parts value' error, got %v", err)
	}
}

// A unit-only recording exports each hook's charm-side trees: in full in
// JSON, folded in Markdown.
func TestExportIncludesOpsTrees(t *testing.T) {
	dir := t.TempDir()
	if err := runSynth(context.Background(), synth.ScenarioOps, synthFlags{output: dir, model: "default"}); err != nil {
		t.Fatalf("runSynth: %v", err)
	}
	var js strings.Builder
	if err := runExport(dir, exportFlags{format: "json", model: "default"}, &js); err != nil {
		t.Fatalf("runExport json: %v", err)
	}
	var res export.Result
	if err := json.Unmarshal([]byte(js.String()), &res); err != nil {
		t.Fatal(err)
	}
	var count func(n *narrative.OpsNode) int
	count = func(n *narrative.OpsNode) int {
		c := 1
		for _, ch := range n.Children {
			c += count(ch)
		}
		return c
	}
	hooks, jsonSpans := 0, 0
	var failedObservers []string
	for _, it := range res.Items {
		if it.Event == nil || it.Event.Kind != "hook" {
			continue
		}
		hooks++
		if len(it.Event.Ops) == 0 {
			t.Errorf("%s %s: no ops trees", it.Event.Unit, it.Event.Summary)
		}
		for _, tr := range it.Event.Ops {
			jsonSpans += count(tr)
			for _, ev := range tr.Children {
				for _, obs := range ev.Children {
					if obs.Kind == recording.OpsObserver && obs.Error != "" {
						failedObservers = append(failedObservers, obs.Name)
					}
				}
			}
		}
	}
	if hooks != 6 || len(failedObservers) != 2 {
		t.Errorf("%d hooks, failed observers %v; want 6 hooks and the 2 failed attempts", hooks, failedObservers)
	}
	spans, _, err := recording.LoadOps(dir)
	if err != nil {
		t.Fatal(err)
	}
	if jsonSpans != len(spans) {
		t.Errorf("JSON trees hold %d spans, the recording %d", jsonSpans, len(spans))
	}

	var md strings.Builder
	if err := runExport(dir, exportFlags{format: "md", model: "default"}, &md); err != nil {
		t.Fatalf("runExport md: %v", err)
	}
	if n := strings.Count(md.String(), "KubernetesComputeResourcesPatch._on_config_changed"); n < 4 {
		t.Errorf("markdown names the resource patch observer %d times, want one per config-changed attempt", n)
	}
	if n := opsTreeLines(md.String()); n >= len(spans) {
		t.Errorf("markdown trees have %d lines for %d spans: calls not folded", n, len(spans))
	}
}

// opsTreeLines counts the lines of the charm-side trees in a Markdown
// export.
func opsTreeLines(md string) int {
	n, in := 0, false
	for _, l := range strings.Split(md, "\n") {
		switch {
		case strings.HasPrefix(l, "in the charm"):
			in = true
		case in && strings.TrimSpace(l) == "```text":
		case in && strings.TrimSpace(l) == "```":
			in = false
		case in:
			n++
		}
	}
	return n
}

// A profiled hook exports what charm init and each observer cost, and the
// stacks it spent the most time and memory in (the lab recording "stacks").
func TestExportIncludesCostsAndTopStacks(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "recording", "testdata", "ops", "stacks"))); err != nil {
		t.Fatal(err)
	}
	man := recording.New("juju-lens", "test", "", "stacks")
	man.Finalize(recording.EndReasonSynth)
	if err := man.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := runIndex(dir, nil); err != nil {
		t.Fatal(err)
	}
	var js strings.Builder
	if err := runExport(dir, exportFlags{format: "json"}, &js); err != nil {
		t.Fatalf("runExport json: %v", err)
	}
	var res export.Result
	if err := json.Unmarshal([]byte(js.String()), &res); err != nil {
		t.Fatal(err)
	}
	var ev *export.EventDetail
	for _, it := range res.Items {
		if it.Event != nil && it.Event.Kind == "hook" {
			ev = it.Event
		}
	}
	if ev == nil {
		t.Fatal("no hook exported")
	}
	kinds := map[string]int{}
	for _, c := range ev.Costs {
		kinds[c.Kind]++
		if !c.Profiled {
			t.Errorf("%s %s: not profiled", c.Kind, c.Name)
		}
	}
	if kinds[recording.OpsInit] != 1 || kinds[recording.OpsObserver] < 3 {
		t.Errorf("costs for %v, want init and every observer", kinds)
	}
	if len(ev.TopStacks) != 5 || ev.TopStacks[0].Span != "charm __init__" || len(ev.TopStacks[0].Frames) == 0 {
		t.Errorf("top stacks %+v", ev.TopStacks)
	}
	if len(ev.TopMemStacks) == 0 || !strings.HasPrefix(ev.TopMemStacks[0].Frames[0], "compress ") {
		t.Errorf("top memory stacks %+v", ev.TopMemStacks)
	}

	var md strings.Builder
	if err := runExport(dir, exportFlags{format: "md"}, &md); err != nil {
		t.Fatalf("runExport md: %v", err)
	}
	for _, want := range []string{"wall      cpu     rss Δ     peak", "top stacks by wall time", "top stacks by memory growth", "refresh_certs (src/charm.py:46)"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
}
