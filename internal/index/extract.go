package index

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// SnapshotKind names the derived state we know how to extract in M2.
type SnapshotKind string

const (
	KindAppStatus   SnapshotKind = "app-status"
	KindUnitStatus  SnapshotKind = "unit-status"
	KindAgentStatus SnapshotKind = "agent-status"
)

// Snapshot is a rebuildable point-in-time observation of Juju state derived
// from a single RPC. The viewer never re-derives; it queries whatever the last
// extract run wrote to snapshots. Body is the JSON payload persisted verbatim
// to the snapshots table.
type Snapshot struct {
	Model           string
	Kind            SnapshotKind
	Scope           string    // e.g. "app-status:grafana" or "unit-status:grafana/0"
	Body            []byte    // JSON object
	Ts              time.Time // wall-clock; the RPC's request time
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

// entityStatusArgs mirrors the on-wire params of the Uniter status setters:
// params.SetStatus{Entities: []EntityStatusArgs{...}} in Juju's apiserver
// params package. We decode only the fields we snapshot; DiscardUnknown-style
// tolerance comes for free from encoding/json ignoring extra keys.
type entityStatusArgs struct {
	Entities []struct {
		Tag    string `json:"tag"`
		Status string `json:"status"`
		Info   string `json:"info"`
	} `json:"entities"`
}

// Status-setting methods on the Uniter facade, confirmed against real Juju 3.6
// traffic. SetUnitStatus carries the *workload* status (active/blocked/…) shown
// in the Status pane; SetAgentStatus carries the *agent* status
// (executing/idle/…) shown next to each unit in the Applications sidebar — a
// separate axis, so it gets its own agent-status scope. Both carry unit tags;
// SetApplicationStatus carries an application tag.
var (
	unitStatusMethods = map[string]bool{
		"SetStatus":     true, // legacy alias kept for older controllers
		"SetUnitStatus": true, // workload status (the one the Status pane wants)
	}
	agentStatusMethods = map[string]bool{
		"SetAgentStatus": true, // agent status: executing while a hook runs, else idle
	}
	appStatusMethods = map[string]bool{
		"SetApplicationStatus": true,
	}
)

// ExtractSnapshots scans synthesised spans for Uniter status-set RPCs and
// returns the derived app/unit status snapshots in wall-clock order.
// Extraction is intentionally forgiving: spans that don't match a known
// extractor, or whose params don't decode, are ignored rather than failing the
// pipeline.
func ExtractSnapshots(spans []recording.SpanRow) []Snapshot {
	var out []Snapshot
	for _, sp := range spans {
		facade := sp.Attrs["facade"]
		if facade != "Uniter" {
			continue
		}
		method := sp.Attrs["method"]
		params := sp.Attrs["params"]
		switch {
		case unitStatusMethods[method]:
			out = append(out, statusSnapshots(sp, params, KindUnitStatus)...)
		case agentStatusMethods[method]:
			out = append(out, statusSnapshots(sp, params, KindAgentStatus)...)
		case appStatusMethods[method]:
			out = append(out, statusSnapshots(sp, params, KindAppStatus)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	return out
}

// statusSnapshots decodes a status-setter's params into one snapshot per
// entity. The scope is derived from the entity tag: unit tags map to
// "unit-status:<app>/<n>", application tags to "app-status:<app>".
func statusSnapshots(sp recording.SpanRow, params string, kind SnapshotKind) []Snapshot {
	if params == "" {
		return nil
	}
	var args entityStatusArgs
	if err := json.Unmarshal([]byte(params), &args); err != nil {
		return nil
	}
	var out []Snapshot
	for _, e := range args.Entities {
		name := entityName(e.Tag)
		if name == "" || e.Status == "" {
			continue
		}
		body, err := json.Marshal(statusBody{Value: e.Status, Message: e.Info, Since: sp.Start})
		if err != nil {
			continue
		}
		scope := string(kind) + ":" + name
		out = append(out, Snapshot{
			Model:           sp.Model,
			Kind:            kind,
			Scope:           scope,
			Body:            body,
			Ts:              sp.Start,
			ProducingSpanID: sp.SpanID,
		})
	}
	return out
}

// entityName turns a Juju entity tag into a display name:
//
//	"unit-grafana-0"        -> "grafana/0"
//	"application-grafana"   -> "grafana"
//
// Unknown tag kinds return "".
func entityName(tag string) string {
	switch {
	case strings.HasPrefix(tag, "unit-"):
		rest := tag[len("unit-"):]
		i := strings.LastIndexByte(rest, '-')
		if i < 0 {
			return rest
		}
		return rest[:i] + "/" + rest[i+1:]
	case strings.HasPrefix(tag, "application-"):
		return tag[len("application-"):]
	default:
		return ""
	}
}
