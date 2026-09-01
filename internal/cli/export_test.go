package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
