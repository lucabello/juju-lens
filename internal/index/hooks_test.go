package index

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

func TestLabelHooks(t *testing.T) {
	base := time.Date(2026, 7, 4, 14, 0, 0, 0, time.UTC)
	setState := func(id, unit string, dt time.Duration, us string) recording.SpanRow {
		// Build params the way the wire does: uniter-state is a JSON string
		// value, so its newlines are escaped.
		params, _ := json.Marshal(map[string]any{
			"args": []map[string]any{{"tag": "unit-x-0", "uniter-state": us}},
		})
		return recording.SpanRow{
			SpanID: id, Unit: unit, Start: base.Add(dt), End: base.Add(dt),
			Name:  "Uniter.SetState",
			Attrs: map[string]string{"method": "SetState", "params": string(params)},
		}
	}
	rpc := func(id, unit string, dt time.Duration) recording.SpanRow {
		return recording.SpanRow{SpanID: id, Unit: unit, Start: base.Add(dt), End: base.Add(dt),
			Name: "Uniter.ReadSettings", Attrs: map[string]string{"method": "ReadSettings"}}
	}
	spans := []recording.SpanRow{
		rpc("1", "grafana/0", 0),
		setState("2", "grafana/0", 1*time.Second, "op: run-hook\nopstep: pending\nhook:\n  kind: config-changed\n"),
		rpc("3", "grafana/0", 2*time.Second),
		setState("4", "grafana/0", 3*time.Second, "op: continue\nopstep: pending\n"),
		rpc("5", "grafana/0", 4*time.Second),
		setState("6", "prometheus/0", 1500*time.Millisecond, "op: run-hook\nhook:\n  kind: update-status\n"),
		rpc("7", "prometheus/0", 2500*time.Millisecond),
	}
	LabelHooks(spans)
	want := map[string]string{"1": "", "2": "config-changed", "3": "config-changed", "4": "", "5": "", "6": "update-status", "7": "update-status"}
	for _, sp := range spans {
		if sp.Hook != want[sp.SpanID] {
			t.Errorf("span %s hook = %q, want %q", sp.SpanID, sp.Hook, want[sp.SpanID])
		}
	}
}
