package index

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

func TestExtractSnapshotsFromSynthShape(t *testing.T) {
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	spans := []recording.SpanRow{
		// A hook that doesn't set any status. Must be ignored.
		{
			SpanID: "01", Model: "default", Unit: "grafana/0",
			Start: base, End: base.Add(100 * time.Millisecond),
			Name: "hook: install",
			Attrs: map[string]string{
				"juju.hook": "install",
			},
		},
		// Unit-status set by grafana/0.
		{
			SpanID: "02", Model: "default", Unit: "grafana/0",
			Start: base.Add(200 * time.Millisecond), End: base.Add(205 * time.Millisecond),
			Name: "jujuc.status-set",
			Attrs: map[string]string{
				"juju.tool":                    "status-set",
				"juju.status.kind":             "workload",
				"juju.status.workload.value":   "active",
				"juju.status.workload.message": "Ready",
			},
		},
		// App-status set by grafana/0 as leader.
		{
			SpanID: "03", Model: "default", Unit: "grafana/0",
			Start: base.Add(210 * time.Millisecond), End: base.Add(215 * time.Millisecond),
			Name: "jujuc.status-set --application",
			Attrs: map[string]string{
				"juju.tool":                       "status-set",
				"juju.status.kind":                "application",
				"juju.status.application.value":   "active",
				"juju.status.application.message": "All units ready",
			},
		},
		// prometheus/0 workload status. No message.
		{
			SpanID: "04", Model: "default", Unit: "prometheus/0",
			Start: base.Add(300 * time.Millisecond), End: base.Add(305 * time.Millisecond),
			Name: "jujuc.status-set",
			Attrs: map[string]string{
				"juju.status.workload.value": "waiting",
			},
		},
		// A status-set span with no unit — must be ignored so we don't
		// invent a scope out of thin air.
		{
			SpanID: "05", Model: "default",
			Start: base.Add(400 * time.Millisecond), End: base.Add(405 * time.Millisecond),
			Name: "jujuc.status-set",
			Attrs: map[string]string{
				"juju.status.workload.value": "unknown",
			},
		},
	}

	got := ExtractSnapshots(spans)
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots, got %d: %+v", len(got), got)
	}

	// Snapshots come back time-ordered; verify shape and scope names.
	want := []struct {
		kind  SnapshotKind
		scope string
		value string
	}{
		{KindUnitStatus, "unit-status:grafana/0", "active"},
		{KindAppStatus, "app-status:grafana", "active"},
		{KindUnitStatus, "unit-status:prometheus/0", "waiting"},
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Scope != w.scope {
			t.Errorf("snap[%d] kind/scope = %s/%s, want %s/%s",
				i, got[i].Kind, got[i].Scope, w.kind, w.scope)
		}
		var body statusBody
		if err := json.Unmarshal(got[i].Body, &body); err != nil {
			t.Fatalf("snap[%d] body not JSON: %v (%s)", i, err, got[i].Body)
		}
		if body.Value != w.value {
			t.Errorf("snap[%d] value = %s, want %s", i, body.Value, w.value)
		}
		if body.Since.IsZero() {
			t.Errorf("snap[%d] since must be set", i)
		}
		if got[i].ProducingSpanID == "" {
			t.Errorf("snap[%d] must carry producing span id", i)
		}
	}
}

func TestExtractSnapshotsFromSynthEndToEnd(t *testing.T) {
	// Cross-package: run the extractor over the *actual* trivial scenario
	// via recording.LoadSpans equivalents. We hand-craft two spans that
	// mirror the synth output.
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	span := recording.SpanRow{
		SpanID: "aa", Model: "default", Unit: "grafana/0",
		Start: base, End: base,
		Attrs: map[string]string{
			"juju.status.workload.value":      "active",
			"juju.status.workload.message":    "Ready",
			"juju.status.application.value":   "active",
			"juju.status.application.message": "All units ready",
		},
	}
	got := ExtractSnapshots([]recording.SpanRow{span})
	if len(got) != 2 {
		t.Fatalf("one span should yield unit + app snapshot, got %d", len(got))
	}
}
