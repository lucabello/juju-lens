package index

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// statusSpan builds a synthesised span the way recording.LoadSpans would for a
// Uniter status-setter RPC: facade/method/params live in Attrs.
func statusSpan(id, method, unit string, start time.Time, params string) recording.SpanRow {
	return recording.SpanRow{
		SpanID: id, Model: "default", Unit: unit,
		Start: start, End: start,
		Name: "Uniter." + method,
		Attrs: map[string]string{
			"facade": "Uniter",
			"method": method,
			"params": params,
		},
	}
}

func TestExtractSnapshotsFromStatusRPCs(t *testing.T) {
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	spans := []recording.SpanRow{
		// A hook commit that sets no status. Must be ignored.
		{
			SpanID: "01", Model: "default", Unit: "grafana/0", Start: base, End: base,
			Name:  "Uniter.CommitHookChanges",
			Attrs: map[string]string{"facade": "Uniter", "method": "CommitHookChanges", "params": `{"changes":[]}`},
		},
		statusSpan("02", "SetStatus", "grafana/0", base.Add(200*time.Millisecond),
			`{"entities":[{"tag":"unit-grafana-0","status":"active","info":"Ready"}]}`),
		statusSpan("03", "SetApplicationStatus", "grafana/0", base.Add(210*time.Millisecond),
			`{"entities":[{"tag":"application-grafana","status":"active","info":"All units ready"}]}`),
		// prometheus workload status, no message.
		statusSpan("04", "SetStatus", "prometheus/0", base.Add(300*time.Millisecond),
			`{"entities":[{"tag":"unit-prometheus-0","status":"waiting"}]}`),
		// A non-Uniter facade must be ignored.
		{
			SpanID: "05", Model: "default", Start: base.Add(400 * time.Millisecond), End: base.Add(400 * time.Millisecond),
			Name:  "Client.FullStatus",
			Attrs: map[string]string{"facade": "Client", "method": "FullStatus"},
		},
	}

	got := ExtractSnapshots(spans)
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots, got %d: %+v", len(got), got)
	}

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
			t.Errorf("snap[%d] kind/scope = %s/%s, want %s/%s", i, got[i].Kind, got[i].Scope, w.kind, w.scope)
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

// SetAgentStatus (executing/idle) must extract into its own agent-status scope,
// distinct from the workload unit-status scope, so the sidebar and Status pane
// draw from different axes. This guards against the recorder/extractor ever
// dropping agent status (it is not recoverable from a recording that omits it).
func TestExtractAgentStatusScope(t *testing.T) {
	base := time.Date(2026, 7, 3, 14, 30, 12, 0, time.UTC)
	spans := []recording.SpanRow{
		statusSpan("01", "SetAgentStatus", "grafana/0", base,
			`{"entities":[{"tag":"unit-grafana-0","status":"executing","info":"running config-changed hook"}]}`),
		statusSpan("02", "SetUnitStatus", "grafana/0", base.Add(time.Second),
			`{"entities":[{"tag":"unit-grafana-0","status":"active","info":"Ready"}]}`),
		statusSpan("03", "SetAgentStatus", "grafana/0", base.Add(2*time.Second),
			`{"entities":[{"tag":"unit-grafana-0","status":"idle"}]}`),
	}
	got := ExtractSnapshots(spans)
	if len(got) != 3 {
		t.Fatalf("expected 3 snapshots, got %d: %+v", len(got), got)
	}
	want := []struct {
		kind  SnapshotKind
		scope string
		value string
	}{
		{KindAgentStatus, "agent-status:grafana/0", "executing"},
		{KindUnitStatus, "unit-status:grafana/0", "active"},
		{KindAgentStatus, "agent-status:grafana/0", "idle"},
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Scope != w.scope {
			t.Errorf("snap[%d] kind/scope = %s/%s, want %s/%s", i, got[i].Kind, got[i].Scope, w.kind, w.scope)
		}
		var body statusBody
		if err := json.Unmarshal(got[i].Body, &body); err != nil {
			t.Fatalf("snap[%d] body not JSON: %v", i, err)
		}
		if body.Value != w.value {
			t.Errorf("snap[%d] value = %s, want %s", i, body.Value, w.value)
		}
	}
}

func TestEntityName(t *testing.T) {
	cases := map[string]string{
		"unit-grafana-0":         "grafana/0",
		"unit-nova-compute-3":    "nova-compute/3",
		"application-prometheus": "prometheus",
		"machine-0":              "",
	}
	for tag, want := range cases {
		if got := entityName(tag); got != want {
			t.Errorf("entityName(%q) = %q, want %q", tag, got, want)
		}
	}
}
