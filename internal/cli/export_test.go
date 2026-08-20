package cli

import (
	"context"
	"strings"
	"testing"

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
