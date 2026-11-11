package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/inspect"
	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/synth"
)

// runExportCmd runs `juju-lens export ...` and returns its output.
func runExportCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd(BuildInfo{Version: "test"})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"export"}, args...))
	err := root.Execute()
	return out.String(), err
}

// untar extracts a gzipped tar archive into dir and lists its entries.
func untar(t *testing.T, archive, dir string) []string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		dst := filepath.Join(dir, h.Name)
		if h.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(dst)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			t.Fatal(err)
		}
		out.Close()
	}
}

// An exported recording holds what can't be rebuilt, leaves the index out,
// and reads back as the same recording after it is extracted.
func TestExportPacksARecordingThatReadsBack(t *testing.T) {
	src := filepath.Join(t.TempDir(), "my-capture")
	if err := runSynth(context.Background(), synth.ScenarioOps, synthFlags{output: src, model: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "recorder.log"), []byte("juju-lens: started\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recording.NewLayout(src).IndexDB()); err != nil {
		t.Fatalf("the synthetic recording has no index to leave out: %v", err)
	}

	archive := filepath.Join(t.TempDir(), "out.tar.gz")
	msg, err := runExportCmd(t, src, "-o", archive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "wrote "+archive) {
		t.Errorf("output %q", msg)
	}

	dst := t.TempDir()
	names := untar(t, archive, dst)
	var haveManifest, haveRaw, haveLog bool
	for _, n := range names {
		switch {
		case n == "my-capture/manifest.json":
			haveManifest = true
		case strings.HasPrefix(n, "my-capture/raw/"):
			haveRaw = true
		case n == "my-capture/recorder.log":
			haveLog = true
		}
		if strings.Contains(n, "index.db") || !strings.HasPrefix(n, "my-capture/") {
			t.Errorf("unexpected entry %q", n)
		}
	}
	if !haveManifest || !haveRaw || !haveLog {
		t.Errorf("manifest %v, raw %v, recorder.log %v in %v", haveManifest, haveRaw, haveLog, names)
	}

	// Extracted, it indexes itself and answers as the original does.
	events := func(dir string) inspect.EventList {
		out, _, err := runInspect(t, "events", dir, "--json", "--limit", "0")
		if err != nil {
			t.Fatal(err)
		}
		var l inspect.EventList
		if err := json.Unmarshal([]byte(out), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	want, got := events(src), events(filepath.Join(dst, "my-capture"))
	if got.Total != want.Total || got.Total == 0 {
		t.Fatalf("%d events after export, %d before", got.Total, want.Total)
	}
	for i := range want.Events {
		if got.Events[i].SpanID != want.Events[i].SpanID || got.Events[i].CPUMS != want.Events[i].CPUMS {
			t.Errorf("event %d differs: %+v vs %+v", i, got.Events[i], want.Events[i])
		}
	}
}

func TestExportRefusesWhatItCannotPackWell(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "x.tar.gz")
	if _, err := runExportCmd(t, dir, "-o", out); err == nil || !strings.Contains(err.Error(), "is this a juju-lens recording") {
		t.Errorf("a directory that isn't a recording: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a failed export left %s behind", out)
	}

	rec := opsRecording(t)
	layout := recording.NewLayout(rec)
	if err := recording.WritePidFile(layout.PidFile(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	defer recording.RemovePidFile(layout.PidFile())
	if _, err := runExportCmd(t, rec, "-o", out); err == nil || !strings.Contains(err.Error(), "still writing") {
		t.Errorf("a recording being written: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a refused export left %s behind", out)
	}
}

// A scoped export of a real-shaped recording holds the scope, says so in the
// manifest, and inspect answers from it.
func TestExportScopedToAUnitReadsBack(t *testing.T) {
	src := opsRecording(t)
	archive := filepath.Join(t.TempDir(), "unit.tar.gz")
	if _, err := runExportCmd(t, src, "--unit", "otelcol/1", "-o", archive); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	untar(t, archive, dst)
	var dir string
	ents, _ := os.ReadDir(dst)
	for _, e := range ents {
		dir = filepath.Join(dst, e.Name())
	}
	out, _, err := runInspect(t, "summary", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a part of the recording, exported with --unit otelcol/1") || !strings.Contains(out, "2 events,") || strings.Contains(out, "otelcol/0") {
		t.Errorf("summary of the unit's export:\n%s", out)
	}

	if _, err := runExportCmd(t, src, "--unit", "nosuch", "-o", filepath.Join(t.TempDir(), "none.tar.gz")); err == nil || !strings.Contains(err.Error(), "nothing in the recording matches") {
		t.Errorf("a scope that matches nothing: %v", err)
	}
	if _, err := runExportCmd(t, src, "--model", "nosuch"); err == nil || !strings.Contains(err.Error(), "no model") {
		t.Errorf("an unknown model: %v", err)
	}
	if _, err := runExportCmd(t, src, "--since", "2026-07-03T15:00:00Z", "--until", "2026-07-03T14:00:00Z"); err == nil {
		t.Error("--until before --since should fail")
	}
}
