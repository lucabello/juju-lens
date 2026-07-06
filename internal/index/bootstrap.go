package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// jujuStatusDoc is the subset of `juju status --format=json` we read to seed
// ground-truth status at recording start (M8). Juju emits application status
// (set by the leader) and, per unit, the workload status — the two independent
// axes the Status pane shows. We ignore everything else; unknown fields are
// dropped by encoding/json.
type jujuStatusDoc struct {
	Model struct {
		Name string `json:"name"`
	} `json:"model"`
	Applications map[string]struct {
		ApplicationStatus statusField            `json:"application-status"`
		Units             map[string]unitStatus `json:"units"`
	} `json:"applications"`
}

type unitStatus struct {
	WorkloadStatus statusField `json:"workload-status"`
	AgentStatus    statusField `json:"juju-status"` // the agent (idle/executing) axis
}

type statusField struct {
	Current string `json:"current"`
	Message string `json:"message"`
	Since   string `json:"since"`
}

// BootstrapSnapshots turns one `juju status --format=json` document into the
// baseline app-status, unit workload-status, and unit agent-status snapshots it
// describes, all stamped at ts (the recording's start instant). The model name
// is taken from the argument when non-empty, else from the document itself, so
// the snapshots are attributed even for a status file whose directory name was
// sanitised.
func BootstrapSnapshots(model string, raw []byte, ts time.Time) []Snapshot {
	var doc jujuStatusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	if model == "" {
		model = doc.Model.Name
	}
	var out []Snapshot
	add := func(kind SnapshotKind, scope string, sf statusField) {
		if sf.Current == "" {
			return
		}
		body, err := json.Marshal(statusBody{Value: sf.Current, Message: sf.Message, Since: ts})
		if err != nil {
			return
		}
		out = append(out, Snapshot{Model: model, Kind: kind, Scope: scope, Body: body, Ts: ts})
	}
	// Sort app names so the emitted order is deterministic (nice for tests and
	// for the ascending-ts insertion invariant the dedup relies on — all
	// bootstrap rows share ts, so relative order only needs to be stable).
	apps := make([]string, 0, len(doc.Applications))
	for app := range doc.Applications {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		a := doc.Applications[app]
		add(KindAppStatus, string(KindAppStatus)+":"+app, a.ApplicationStatus)
		units := make([]string, 0, len(a.Units))
		for u := range a.Units {
			units = append(units, u)
		}
		sort.Strings(units)
		for _, u := range units {
			add(KindUnitStatus, string(KindUnitStatus)+":"+u, a.Units[u].WorkloadStatus)
			add(KindAgentStatus, string(KindAgentStatus)+":"+u, a.Units[u].AgentStatus)
		}
	}
	return out
}

// showUnitDoc is the subset of `juju show-unit --format=json` we read to seed
// baseline relation databags: a map from unit name to that unit's relations,
// each carrying the application databag and every in-scope unit's data. The
// field names have been stable across juju 3.6 and 4.x; unknown fields are
// dropped by encoding/json so a version that adds more is still parsed.
type showUnitDoc map[string]struct {
	RelationInfo []relationInfo `json:"relation-info"`
}

type relationInfo struct {
	RelationID      int             `json:"relation-id"`
	Endpoint        string          `json:"endpoint"`
	RelatedEndpoint string          `json:"related-endpoint"`
	ApplicationData json.RawMessage `json:"application-data"`
	LocalUnit       struct {
		Data json.RawMessage `json:"data"`
	} `json:"local-unit"`
	RelatedUnits map[string]struct {
		Data json.RawMessage `json:"data"`
	} `json:"related-units"`
}

// DatabagBootstrapSnapshots turns one `juju show-unit --format=json` document
// into the baseline relation-databag snapshots it describes, stamped at ts.
// Every databag it can attribute — the unit's own data, the application data,
// and each related unit's data — is emitted under the same canonical relation
// scope the RPC extractor uses ("databag:<sorted-key>:<entity>"), so a relation
// that already existed at recording start dedups cleanly against the first RPC
// that touches it.
//
// Peer relations are detected by relation id: a first pass records which
// applications participate in each relation id, so a relation only its own
// application takes part in is a peer (single-segment key) even when the unit is
// the app's only one and has no related units to reveal the remote app.
func DatabagBootstrapSnapshots(model string, raw []byte, ts time.Time) []Snapshot {
	var doc showUnitDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}

	// Pass 1: applications participating in each relation id.
	appsByRel := map[int]map[string]bool{}
	for u, su := range doc {
		for _, ri := range su.RelationInfo {
			if appsByRel[ri.RelationID] == nil {
				appsByRel[ri.RelationID] = map[string]bool{}
			}
			appsByRel[ri.RelationID][appName(u)] = true
			for ru := range ri.RelatedUnits {
				appsByRel[ri.RelationID][appName(ru)] = true
			}
		}
	}

	var out []Snapshot
	seen := map[string]bool{}
	add := func(key, entity string, body json.RawMessage) {
		if key == "" || entity == "" || !hasContent(body) {
			return
		}
		scope := "databag:" + key + ":" + entity
		if seen[scope] {
			return
		}
		seen[scope] = true
		out = append(out, Snapshot{
			Model: model, Kind: KindDatabag, Scope: scope,
			Body: append([]byte(nil), body...), Ts: ts,
		})
	}
	// Deterministic unit order keeps the emitted order stable (all bootstrap
	// rows share ts, so only relative order needs to be stable).
	units := make([]string, 0, len(doc))
	for u := range doc {
		units = append(units, u)
	}
	sort.Strings(units)
	for _, u := range units {
		appU := appName(u)
		for _, ri := range doc[u].RelationInfo {
			rep := ri.RelatedEndpoint
			if rep == "" {
				rep = ri.Endpoint
			}
			key := relationKeyForRel(appU, ri.Endpoint, rep, appsByRel[ri.RelationID])
			add(key, u, ri.LocalUnit.Data)
			add(key, appU, ri.ApplicationData)
			for ru, rud := range ri.RelatedUnits {
				add(key, ru, rud.Data)
			}
		}
	}
	return out
}

// relationKeyForRel builds the canonical relation key for a relation whose
// participating applications are apps. When the local application is the only
// participant it is a peer relation (single-segment key); otherwise the other
// application is the remote end.
func relationKeyForRel(localApp, localEp, remoteEp string, apps map[string]bool) string {
	remoteApp := ""
	for a := range apps {
		if a != localApp {
			remoteApp = a
			break
		}
	}
	if remoteApp == "" {
		return localApp + "." + localEp // peer: only the local app participates
	}
	return canonicalRelationKey(localApp + "." + localEp + "#" + remoteApp + "." + remoteEp)
}

// ConfigBootstrapSnapshots turns the combined `juju config` document (a map from
// application to that app's `juju config --format=json` output) into baseline
// charm-config snapshots stamped at ts. Each app yields one "config:<app>"
// snapshot whose body is the flat {key: value} map — matching the shape the RPC
// ConfigSettings extractor writes — so a later config read dedups against it and
// a config-changed event has a "before" to diff from t0.
func ConfigBootstrapSnapshots(model string, raw []byte, ts time.Time) []Snapshot {
	var doc map[string]struct {
		Application string `json:"application"`
		Settings    map[string]struct {
			Value json.RawMessage `json:"value"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	apps := make([]string, 0, len(doc))
	for app := range doc {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	var out []Snapshot
	for _, app := range apps {
		d := doc[app]
		if len(d.Settings) == 0 {
			continue
		}
		name := d.Application
		if name == "" {
			name = app
		}
		flat := make(map[string]json.RawMessage, len(d.Settings))
		for k, s := range d.Settings {
			if len(s.Value) > 0 {
				flat[k] = s.Value
			} else {
				flat[k] = json.RawMessage("null")
			}
		}
		body, err := json.Marshal(flat)
		if err != nil {
			continue
		}
		out = append(out, Snapshot{Model: model, Kind: KindConfig, Scope: "config:" + name, Body: body, Ts: ts})
	}
	return out
}

// LoadBootstrapSnapshots reads the baseline snapshots captured at recording
// start under raw/status/<model>/: bootstrap.json (`juju status` → app/unit
// status), databags.json (`juju show-unit` → relation databags), and config.json
// (`juju config` → charm config), all stamped at ts. Missing or unreadable files
// are skipped — bootstrap data is best-effort context, never a hard dependency.
func LoadBootstrapSnapshots(root string, ts time.Time) []Snapshot {
	statusRoot := filepath.Join(recording.NewLayout(root).RawDir(), "status")
	entries, err := os.ReadDir(statusRoot)
	if err != nil {
		return nil
	}
	var out []Snapshot
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(statusRoot, e.Name())
		// Model name comes from the status document; the dir name is only a
		// fallback for documents that omit it. show-unit carries no model name,
		// so its snapshots reuse whatever the status document reported.
		model := ""
		if raw, err := os.ReadFile(filepath.Join(dir, "bootstrap.json")); err == nil {
			out = append(out, BootstrapSnapshots("", raw, ts)...)
			model = modelNameOf(raw)
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "databags.json")); err == nil {
			out = append(out, DatabagBootstrapSnapshots(model, raw, ts)...)
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
			out = append(out, ConfigBootstrapSnapshots(model, raw, ts)...)
		}
	}
	return out
}

// modelNameOf extracts the model name from a `juju status` document, or "".
func modelNameOf(statusJSON []byte) string {
	var doc jujuStatusDoc
	if err := json.Unmarshal(statusJSON, &doc); err != nil {
		return ""
	}
	return doc.Model.Name
}
