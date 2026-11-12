package recording

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// packFixture is a small recording of two models: m1 has two Kubernetes units
// and a debug log, m2 one more unit.
func packFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := New("juju-lens", "test", "", "ctl")
	m.Models = []ModelInfo{{Name: "m1"}, {Name: "m2"}}
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	unitFile := func(unit string, times ...string) string {
		out := `{"kind":"hello","ts":"2026-11-11T10:00:00Z","hello":{"units":["` + unit + `"]}}` + "\n"
		for _, ts := range times {
			out += `{"kind":"resources","ts":"` + ts + `"}` + "\n"
		}
		return out
	}
	write("raw/unit/m1/grafana-0/unit-2026-11-11T10.jsonl", unitFile("grafana/0", "2026-11-11T10:01:00Z", "2026-11-11T10:05:00Z", "2026-11-11T10:09:00Z"))
	write("raw/unit/m1/loki-0/unit-2026-11-11T10.jsonl", unitFile("loki/0", "2026-11-11T10:01:00Z"))
	write("raw/unit/m2/other-0/unit-2026-11-11T10.jsonl", unitFile("other/0", "2026-11-11T10:01:00Z"))
	write("raw/k8s/m1/grafana-0/grafana/logs-2026-11-11T10.log", "2026-11-11T10:01:00.000Z 2026-11-11T10:01:00.000Z [grafana] a\n2026-11-11T10:08:00.000Z 2026-11-11T10:08:00.000Z [grafana] b\n")
	write("raw/k8s/m1/loki-0/loki/logs-2026-11-11T10.log", "2026-11-11T10:01:00.000Z 2026-11-11T10:01:00.000Z [loki] c\n")
	write("raw/juju/m1/debug-log-2026-11-11T10.log",
		"controller-0: 2026-11-11 10:01:00.000 INFO juju.api no unit\n"+
			"unit-grafana-0: 2026-11-11 10:01:00.000 INFO unit.grafana/0.juju-log early\n"+
			"unit-grafana-0: 2026-11-11 10:05:00.000 ERROR unit.grafana/0.juju-log multi\n"+
			"  continues the line before\n"+
			"unit-grafana-0: 2026-11-11 10:09:00.000 INFO unit.grafana/0.juju-log late\n"+
			"unit-loki-0: 2026-11-11 10:05:00.000 INFO unit.loki/0.juju-log other unit\n")
	write("raw/rpc/m1/calls-2026-11-11T10.jsonl",
		`{"ts":"2026-11-11T10:05:00Z","unit":"grafana/0","msg":{}}`+"\n"+
			`{"ts":"2026-11-11T10:05:00Z","unit":"loki/0","msg":{}}`+"\n"+
			`{"ts":"2026-11-11T10:05:00Z","msg":{}}`+"\n")
	write("raw/status/m1/bootstrap.json", `{"model":"m1"}`)
	write("raw/status/m1/status-log.json", `{"grafana/0":[{"status":"idle","since":"2026-11-11T10:01:00Z"},{"status":"idle","since":"2026-11-11T10:09:00Z"}],"loki/0":[{"status":"idle","since":"2026-11-11T10:05:00Z"}]}`)
	write("raw/status/m1/databags.json", `{"grafana/0":"secret-value"}`)
	write("raw/status/m1/config.json", `{"grafana":{"admin":"pw"}}`)
	write("raw/status/m2/bootstrap.json", `{"model":"m2"}`)
	write("recorder.log", "started\n")
	return dir
}

// pack runs Pack and returns the archive's files by name, without the top
// directory.
func pack(t *testing.T, dir string, o PackOptions) (map[string]string, PackResult) {
	t.Helper()
	var buf bytes.Buffer
	res, err := Pack(dir, &buf, o)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, res
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		_, name, _ := strings.Cut(h.Name, "/")
		files[name] = string(body)
	}
}

func hasFile(files map[string]string, sub string) bool {
	for n := range files {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestPackKeepsEverythingWithoutAScope(t *testing.T) {
	files, res := pack(t, packFixture(t), PackOptions{})
	if len(files) != 14 || res.Files != 14 {
		t.Errorf("%d files (result %d), want the manifest, recorder.log and 12 under raw/: %v", len(files), res.Files, files)
	}
	if strings.Contains(files["manifest.json"], `"exported"`) {
		t.Error("an unscoped export says it is partial")
	}
}

func TestPackScopesByModel(t *testing.T) {
	files, _ := pack(t, packFixture(t), PackOptions{Models: []string{"m2"}})
	for n := range files {
		if strings.Contains(n, "/m1/") {
			t.Errorf("a file of model m1 in an export of m2: %s", n)
		}
	}
	if !hasFile(files, "raw/unit/m2/other-0") || !hasFile(files, "raw/status/m2/bootstrap.json") {
		t.Errorf("m2's files missing: %v", files)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(files["manifest.json"]), &m); err != nil || m.Exported == nil || len(m.Exported.Models) != 1 || m.Exported.Models[0] != "m2" {
		t.Errorf("manifest %v: %+v", err, m.Exported)
	}
	if len(m.Models) != 1 || m.Models[0].Name != "m2" {
		t.Errorf("the manifest lists %+v: it should list only the model that is in the archive", m.Models)
	}
	if m.Exported.Since != nil || m.Exported.Until != nil {
		t.Errorf("unset bounds are set in the scope: %+v", m.Exported)
	}
}

func TestPackScopesByUnit(t *testing.T) {
	files, res := pack(t, packFixture(t), PackOptions{Units: []string{"grafana"}})
	if !hasFile(files, "raw/unit/m1/grafana-0") || !hasFile(files, "raw/k8s/m1/grafana-0") {
		t.Errorf("grafana's probe and workload logs missing: %v", files)
	}
	for _, other := range []string{"loki-0", "other-0"} {
		if hasFile(files, other) {
			t.Errorf("a file of %s in an export of grafana", other)
		}
	}
	dl := files["raw/juju/m1/debug-log-2026-11-11T10.log"]
	if strings.Contains(dl, "controller-0") || strings.Contains(dl, "unit-loki-0") || !strings.Contains(dl, "unit-grafana-0") {
		t.Errorf("debug log lines of other entities, or none of grafana's:\n%s", dl)
	}
	if !strings.Contains(dl, "continues the line before") {
		t.Errorf("a continuation line was dropped from a kept line:\n%s", dl)
	}
	rpc := files["raw/rpc/m1/calls-2026-11-11T10.jsonl"]
	if strings.Count(rpc, "\n") != 1 || !strings.Contains(rpc, "grafana/0") {
		t.Errorf("rpc lines:\n%s", rpc)
	}
	var sl map[string][]any
	if err := json.Unmarshal([]byte(files["raw/status/m1/status-log.json"]), &sl); err != nil || len(sl) != 1 || len(sl["grafana/0"]) != 2 {
		t.Errorf("status log %v: %v", err, sl)
	}
	if res.RawFiles == 0 {
		t.Error("nothing counted as matched")
	}
}

func TestPackScopesByTime(t *testing.T) {
	since := time.Date(2026, 11, 11, 10, 4, 0, 0, time.UTC)
	until := time.Date(2026, 11, 11, 10, 6, 0, 0, time.UTC)
	files, _ := pack(t, packFixture(t), PackOptions{Models: []string{"m1"}, Since: since, Until: until})

	u := files["raw/unit/m1/grafana-0/unit-2026-11-11T10.jsonl"]
	if !strings.Contains(u, `"kind":"hello"`) || !strings.Contains(u, "10:05:00") || strings.Contains(u, "10:01:00") || strings.Contains(u, "10:09:00") {
		t.Errorf("the hello stays, and only 10:05 of the readings:\n%s", u)
	}
	dl := files["raw/juju/m1/debug-log-2026-11-11T10.log"]
	if strings.Contains(dl, "early") || strings.Contains(dl, "late") || !strings.Contains(dl, "multi") || !strings.Contains(dl, "continues the line before") {
		t.Errorf("debug log window:\n%s", dl)
	}
	if hasFile(files, "raw/k8s/m1/loki-0") {
		t.Error("an empty file was packed")
	}
	if !strings.Contains(files["raw/status/m1/status-log.json"], "10:05:00") || strings.Contains(files["raw/status/m1/status-log.json"], "10:01:00") {
		t.Errorf("status log: %s", files["raw/status/m1/status-log.json"])
	}
	if files["raw/status/m1/bootstrap.json"] == "" {
		t.Error("the bootstrap snapshot is kept whole")
	}
}

func TestPackThatMatchesNothingCountsNoRawFiles(t *testing.T) {
	_, res := pack(t, packFixture(t), PackOptions{Units: []string{"nosuch"}})
	if res.RawFiles != 0 {
		t.Errorf("%d raw files for a unit that isn't there", res.RawFiles)
	}
}

// A unit probe on a machine covers the units its hello lists, not the one its
// directory name suggests.
func TestPackFindsMachineUnitsFromTheHello(t *testing.T) {
	dir := t.TempDir()
	m := New("juju-lens", "test", "", "ctl")
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "raw/unit/mm/machine-0")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "unit-2026-11-11T10.jsonl"), []byte(`{"kind":"hello","ts":"2026-11-11T10:00:00Z","hello":{"units":["ubuntu/0","otelcol/0"]}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, _ := pack(t, dir, PackOptions{Units: []string{"otelcol/0"}})
	if !hasFile(files, "raw/unit/mm/machine-0") {
		t.Errorf("the machine that hosts otelcol/0 was dropped: %v", files)
	}
	files, _ = pack(t, dir, PackOptions{Units: []string{"grafana"}})
	if hasFile(files, "machine-0") {
		t.Error("a machine that hosts no matching unit was kept")
	}
}

func TestPackWithoutValuesLeavesOutDatabagsAndConfig(t *testing.T) {
	files, _ := pack(t, packFixture(t), PackOptions{WithoutValues: true})
	for n, body := range files {
		if strings.HasSuffix(n, "databags.json") || strings.HasSuffix(n, "config.json") || strings.Contains(body, "secret-value") || strings.Contains(body, `"admin":"pw"`) {
			t.Errorf("%s: values left in the archive", n)
		}
	}
	if files["raw/status/m1/bootstrap.json"] == "" || files["raw/status/m1/status-log.json"] == "" || !hasFile(files, "raw/unit/m1/grafana-0") {
		t.Errorf("everything else should stay: %v", files)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(files["manifest.json"]), &m); err != nil || m.Exported == nil || !m.Exported.WithoutValues {
		t.Errorf("manifest says nothing: %v %+v", err, m.Exported)
	}
}
