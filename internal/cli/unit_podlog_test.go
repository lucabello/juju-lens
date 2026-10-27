package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// TestWritePodLog: a workload log line from a unit probe lands where `kubectl
// logs` lines do, in the shape ParseK8sLogLine reads, attributed to the unit.
func TestWritePodLog(t *testing.T) {
	layout := recording.NewLayout(t.TempDir())
	ui := newUnitIngester(layout, "k8s", false, unitScope{}, map[string]string{"lens": "caas"}, 0, true, 0, true, nil)
	st := &unitStream{name: "unit lens/otelcol-0"}
	ts := time.Date(2026, 10, 26, 19, 46, 59, 52_000_000, time.UTC)
	ui.writePodLog(st, []byte(`{"kind":"pebble_log","ts":"2026-10-26T19:47:00Z","pebble_log":{"container":"otelcol","time":"`+
		ts.Format(time.RFC3339Nano)+`","service":"otelcol","message":"level=info msg=\"two\nlines\""}}`))
	ui.closeLogs()

	files, _ := filepath.Glob(filepath.Join(layout.Root, "raw", "k8s", "lens", "otelcol-0", "otelcol", "logs-*.log"))
	if len(files) != 1 {
		t.Fatalf("log files %v", files)
	}
	data, _ := os.ReadFile(files[0])
	if n := strings.Count(string(data), "\n"); n != 1 {
		t.Fatalf("%d lines, want one: %q", n, data)
	}
	rec, ok := recording.ParseK8sLogLine(string(data), "lens", "otelcol-0", "otelcol")
	if !ok || rec.Unit != "otelcol/0" || rec.Module != "otelcol" || !rec.Ts.Equal(ts) ||
		!strings.HasPrefix(rec.Message, "[otelcol] level=info") {
		t.Errorf("parsed %+v (ok %v)", rec, ok)
	}
	if ui.podLogCount() != 1 {
		t.Errorf("counted %d lines", ui.podLogCount())
	}
}
