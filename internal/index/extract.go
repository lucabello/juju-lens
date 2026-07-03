package index

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// SnapshotKind names the derived state we know how to extract in M2.
type SnapshotKind string

const (
	KindAppStatus  SnapshotKind = "app-status"
	KindUnitStatus SnapshotKind = "unit-status"
)

// Snapshot is a rebuildable point-in-time observation of Juju state derived
// from a single span. The viewer never re-derives; it queries whatever the
// last extract run wrote to snapshots. Body is the JSON payload persisted
// verbatim to the snapshots table.
type Snapshot struct {
	Model           string
	Kind            SnapshotKind
	Scope           string    // e.g. "app-status:grafana" or "unit-status:grafana/0"
	Body            []byte    // JSON object
	Ts              time.Time // wall-clock; usually the span's start time
	ProducingSpanID string
}

// statusBody is the JSON shape written to snapshots.body_json for both
// application and unit statuses. Keeping the two identical means the viewer
// treats them the same way when rendering.
type statusBody struct {
	Value   string    `json:"value"`
	Message string    `json:"message,omitempty"`
	Since   time.Time `json:"since"`
}

// ExtractSnapshots scans spans for status-set-style attributes and returns
// the derived snapshots in wall-clock order. Extraction is intentionally
// forgiving: spans that don't match any known extractor are ignored, and
// missing optional fields never fail the pipeline.
//
// Recognised attribute conventions (kept in one place on purpose):
//
//   - Unit status:  juju.status.workload.value    (required)
//     juju.status.workload.message  (optional)
//     The unit is taken from juju.unit / executor.unit / the resource attrs.
//
//   - App status:   juju.status.application.value    (required)
//     juju.status.application.message  (optional)
//     Emitted by the leader; scoped to the unit's app.
//
// These names match what synth.trivial produces. Real Juju spans will get
// their own extractor as we validate against real recordings.
func ExtractSnapshots(spans []recording.SpanRow) []Snapshot {
	var out []Snapshot
	for _, sp := range spans {
		if s, ok := extractUnitStatus(sp); ok {
			out = append(out, s)
		}
		if s, ok := extractAppStatus(sp); ok {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	return out
}

func extractUnitStatus(sp recording.SpanRow) (Snapshot, bool) {
	value := sp.Attrs["juju.status.workload.value"]
	if value == "" {
		return Snapshot{}, false
	}
	unit := sp.Unit
	if unit == "" {
		return Snapshot{}, false
	}
	body := statusBody{
		Value:   value,
		Message: sp.Attrs["juju.status.workload.message"],
		Since:   sp.Start,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Snapshot{}, false
	}
	return Snapshot{
		Model:           sp.Model,
		Kind:            KindUnitStatus,
		Scope:           "unit-status:" + unit,
		Body:            b,
		Ts:              sp.Start,
		ProducingSpanID: sp.SpanID,
	}, true
}

func extractAppStatus(sp recording.SpanRow) (Snapshot, bool) {
	value := sp.Attrs["juju.status.application.value"]
	if value == "" {
		return Snapshot{}, false
	}
	unit := sp.Unit
	if unit == "" {
		return Snapshot{}, false
	}
	app := appOf(unit)
	if app == "" {
		return Snapshot{}, false
	}
	body := statusBody{
		Value:   value,
		Message: sp.Attrs["juju.status.application.message"],
		Since:   sp.Start,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Snapshot{}, false
	}
	return Snapshot{
		Model:           sp.Model,
		Kind:            KindAppStatus,
		Scope:           "app-status:" + app,
		Body:            b,
		Ts:              sp.Start,
		ProducingSpanID: sp.SpanID,
	}, true
}
