package recording

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSuggestedDirName(t *testing.T) {
	ts := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	got := SuggestedDirName("mycontroller", ts)
	want := "2026-07-03T14-30-12--mycontroller"
	if got != want {
		t.Fatalf("SuggestedDirName = %q, want %q", got, want)
	}
	if SuggestedDirName("", ts) != "2026-07-03T14-30-12--unknown" {
		t.Fatalf("empty controller name should fall back to 'unknown'")
	}
}

func TestManifestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m := New("juju-lens", "0.1.0", "abc123", "mycontroller")
	m.AddSource(SourceStatus{Name: "rpc", Kind: "rpc", Started: time.Now().UTC()})
	m.FinishSource("rpc", nil)
	m.Finalize(EndReasonSynth)
	if err := m.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Tool.Name != "juju-lens" || got.Tool.Version != "0.1.0" {
		t.Fatalf("tool identity lost: %+v", got.Tool)
	}
	if got.Controller.Name != "mycontroller" {
		t.Fatalf("controller lost: %+v", got.Controller)
	}
	if got.EndReason != EndReasonSynth {
		t.Fatalf("end reason lost: %s", got.EndReason)
	}
	if len(got.Sources) != 1 || got.Sources[0].Name != "rpc" || got.Sources[0].Stopped.IsZero() {
		t.Fatalf("sources lost/incomplete: %+v", got.Sources)
	}
}

func TestManifestFinishSourceUnknownAppends(t *testing.T) {
	m := New("juju-lens", "0.0.0", "", "c")
	m.FinishSource("ghost", nil)
	if len(m.Sources) != 1 || m.Sources[0].Name != "ghost" || m.Sources[0].Stopped.IsZero() {
		t.Fatalf("FinishSource should append when name is unknown: %+v", m.Sources)
	}
}

func TestManifestFileIsIndentedJSON(t *testing.T) {
	dir := t.TempDir()
	m := New("juju-lens", "0.0.0", "", "c")
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\n  \"tool\"") {
		t.Fatalf("expected indented manifest, got:\n%s", raw)
	}
	// Roundtrip through the standard library to prove it parses.
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutPathsAreDeterministic(t *testing.T) {
	l := NewLayout("/tmp/rec")
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"manifest", l.Manifest(), "/tmp/rec/manifest.json"},
		{"raw", l.RawDir(), "/tmp/rec/raw"},
		{"rpc", l.RPCDir("default"), "/tmp/rec/raw/rpc/default"},
		{"rpc-sanitised", l.RPCDir("evil/../name"), "/tmp/rec/raw/rpc/evil_.._name"},
		{"juju", l.JujuDir("default"), "/tmp/rec/raw/juju/default"},
		{"juju-sanitised", l.JujuDir("evil/../name"), "/tmp/rec/raw/juju/evil_.._name"},
		{"k8s", l.K8sDir("m1"), "/tmp/rec/raw/k8s/m1"},
		{"machine", l.MachineDir("controller", "0"), "/tmp/rec/raw/machine/controller/0"},
		{"snap", l.SnapDir("controller", "0"), "/tmp/rec/raw/snap/controller/0"},
		{"derived", l.DerivedDir(), "/tmp/rec/derived"},
		{"index", l.IndexDB(), "/tmp/rec/index.db"},
		{"calls", l.RPCFileFor("default")("2026-07-03T14"), "/tmp/rec/raw/rpc/default/calls-2026-07-03T14.jsonl"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

func TestLayoutInitAndExists(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(filepath.Join(dir, "rec"))
	if err := l.Init(); err != nil {
		t.Fatalf("Init: %v", err)
	}
	for _, d := range []string{l.Root, l.RawDir(), l.DerivedDir()} {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			t.Errorf("expected directory at %s (err=%v)", d, err)
		}
	}
	ex, err := l.Exists()
	if err != nil || ex {
		t.Fatalf("Exists before manifest: got (%v, %v), want (false, nil)", ex, err)
	}
	if err := (&Manifest{}).Save(l.Root); err != nil {
		t.Fatal(err)
	}
	ex, err = l.Exists()
	if err != nil || !ex {
		t.Fatalf("Exists after manifest: got (%v, %v), want (true, nil)", ex, err)
	}
}
