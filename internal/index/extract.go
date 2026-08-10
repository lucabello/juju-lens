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
	KindDatabag     SnapshotKind = "databag"    // relation databag (M4)
	KindConfig      SnapshotKind = "config"     // charm application config (M9)
	KindLeadership  SnapshotKind = "leadership" // which unit leads an application (M8)
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
		out = append(out, SnapshotsForSpan(sp)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts.Before(out[j].Ts) })
	return out
}

// SnapshotsForSpan derives the snapshots a single span produces. It is the unit
// of extraction shared by the batch ExtractSnapshots and the incremental
// indexer, so both paths stay identical.
func SnapshotsForSpan(sp recording.SpanRow) []Snapshot {
	if sp.Attrs["facade"] != "Uniter" {
		return nil
	}
	method := sp.Attrs["method"]
	params := sp.Attrs["params"]
	switch {
	case unitStatusMethods[method]:
		return statusSnapshots(sp, params, KindUnitStatus)
	case agentStatusMethods[method]:
		return statusSnapshots(sp, params, KindAgentStatus)
	case appStatusMethods[method]:
		return statusSnapshots(sp, params, KindAppStatus)
	case method == "CommitHookChanges":
		return databagSnapshots(sp, params)
	case method == "ConfigSettings":
		return configSnapshots(sp)
	}
	return nil
}

// configSnapshots turns a Uniter.ConfigSettings RPC into a charm-config
// snapshot. Unlike the databag/status extractors the value lives in the
// *response* (params only names the unit), so we read Attrs["response"], whose
// shape is {"results":[{"settings":{...}}]}. The settings object is the charm
// config verbatim; the scope is application-wide ("config:<app>") since every
// unit of an application reads the same config. Empty settings (subordinate or
// config-less charms) yield nothing.
func configSnapshots(sp recording.SpanRow) []Snapshot {
	resp := sp.Attrs["response"]
	if resp == "" {
		return nil
	}
	var r struct {
		Results []struct {
			Settings json.RawMessage `json:"settings"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(resp), &r); err != nil || len(r.Results) == 0 {
		return nil
	}
	settings := r.Results[0].Settings
	if !hasContent(settings) {
		return nil
	}
	app := appName(sp.Unit)
	if app == "" {
		return nil
	}
	return []Snapshot{{
		Model:           sp.Model,
		Kind:            KindConfig,
		Scope:           "config:" + app,
		Body:            append([]byte(nil), settings...),
		Ts:              sp.Start,
		ProducingSpanID: sp.SpanID,
	}}
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

// RelationForSpan best-effort derives the relation a Uniter RPC concerns, from
// the common param shapes: a top-level "relation", "relation-unit-pairs"
// (ReadRemoteSettings), or the relation-unit-settings inside CommitHookChanges.
// It returns "" when the span has no relation context. The result is stored on
// the span so the timeline and relations pane can pivot on it.
func RelationForSpan(sp recording.SpanRow) string {
	params := sp.Attrs["params"]
	if params == "" {
		return ""
	}
	var probe struct {
		Relation          string `json:"relation"`
		RelationUnitPairs []struct {
			Relation string `json:"relation"`
		} `json:"relation-unit-pairs"`
		Args []struct {
			RelationUnitSettings []struct {
				Relation string `json:"relation"`
			} `json:"relation-unit-settings"`
		} `json:"args"`
	}
	if err := json.Unmarshal([]byte(params), &probe); err != nil {
		return ""
	}
	if probe.Relation != "" {
		return relationKey(probe.Relation)
	}
	for _, p := range probe.RelationUnitPairs {
		if p.Relation != "" {
			return relationKey(p.Relation)
		}
	}
	for _, a := range probe.Args {
		for _, r := range a.RelationUnitSettings {
			if r.Relation != "" {
				return relationKey(r.Relation)
			}
		}
	}
	return ""
}

// commitHookChanges mirrors the parts of Uniter.CommitHookChanges we snapshot:
// the relation databags a hook wrote. Each relation-unit-setting carries the
// unit's own databag (settings) and, when the unit is the leader, the
// application databag (application-settings).
type commitHookChanges struct {
	Args []struct {
		Tag                  string `json:"tag"`
		RelationUnitSettings []struct {
			Relation            string          `json:"relation"`
			Unit                string          `json:"unit"`
			Settings            json.RawMessage `json:"settings"`
			ApplicationSettings json.RawMessage `json:"application-settings"`
		} `json:"relation-unit-settings"`
	} `json:"args"`
}

// databagSnapshots turns a CommitHookChanges into one databag snapshot per
// relation endpoint written: scope "databag:<relation>:<entity>" where entity
// is a unit name (unit databag) or an application name (application databag).
// The body is the settings object verbatim, so the viewer can render and diff
// it. Empty/null settings are skipped so a hook that touched nothing does not
// emit a snapshot.
func databagSnapshots(sp recording.SpanRow, params string) []Snapshot {
	if params == "" {
		return nil
	}
	var chc commitHookChanges
	if err := json.Unmarshal([]byte(params), &chc); err != nil {
		return nil
	}
	var out []Snapshot
	for _, arg := range chc.Args {
		for _, rus := range arg.RelationUnitSettings {
			rel := relationKey(rus.Relation)
			if rel == "" {
				continue
			}
			if unit := entityName(rus.Unit); unit != "" && hasContent(rus.Settings) {
				out = append(out, databagSnap(sp, rel, unit, rus.Settings))
			}
			// Application databag is set by the leader; attribute it to the app.
			if hasContent(rus.ApplicationSettings) {
				if app := appName(entityName(rus.Unit)); app != "" {
					out = append(out, databagSnap(sp, rel, app, rus.ApplicationSettings))
				}
			}
		}
	}
	return out
}

func databagSnap(sp recording.SpanRow, rel, entity string, body json.RawMessage) Snapshot {
	return Snapshot{
		Model:           sp.Model,
		Kind:            KindDatabag,
		Scope:           "databag:" + rel + ":" + entity,
		Body:            append([]byte(nil), body...),
		Ts:              sp.Start,
		ProducingSpanID: sp.SpanID,
	}
}

// hasContent reports whether a raw JSON value is a non-empty object.
func hasContent(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "{}"
}

// relationKey normalises a relation tag into a stable, canonical display key:
//
//	"relation-loki.certificates#ca.certificates" -> "ca.certificates#loki.certificates"
//
// The two "app.endpoint" segments are sorted so a relation yields the same key
// no matter which side Juju (or a bootstrap `juju show-unit`) lists first — that
// is what lets RPC-derived and bootstrap-derived databags share one scope.
// Numeric relation ids (from EnterScope-style params) are returned as-is.
func relationKey(relation string) string {
	return canonicalRelationKey(strings.TrimPrefix(relation, "relation-"))
}

// canonicalRelationKey sorts the two segments of an "a.ep#b.ep" relation key.
// Peer relations (a single segment) and any non-"#" form are returned unchanged.
func canonicalRelationKey(key string) string {
	a, b, ok := strings.Cut(key, "#")
	if !ok {
		return key
	}
	if a > b {
		a, b = b, a
	}
	return a + "#" + b
}

// appName turns "grafana/0" into "grafana"; returns the input unchanged when it
// is not a unit name.
func appName(unit string) string {
	if i := strings.IndexByte(unit, '/'); i >= 0 {
		return unit[:i]
	}
	return unit
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
