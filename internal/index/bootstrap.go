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
}

type statusField struct {
	Current string `json:"current"`
	Message string `json:"message"`
	Since   string `json:"since"`
}

// BootstrapSnapshots turns one `juju status --format=json` document into the
// baseline app-status and unit-status snapshots it describes, all stamped at ts
// (the recording's start instant). The model name is taken from the argument
// when non-empty, else from the document itself, so the snapshots are attributed
// even for a status file whose directory name was sanitised.
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
		}
	}
	return out
}

// LoadBootstrapSnapshots reads every raw/status/<model>/bootstrap.json under a
// recording and returns the baseline snapshots they describe, all stamped at ts.
// Missing or unreadable files are skipped — bootstrap status is best-effort
// context, never a hard dependency.
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
		raw, err := os.ReadFile(filepath.Join(statusRoot, e.Name(), "bootstrap.json"))
		if err != nil {
			continue
		}
		// Model name comes from the document; the dir name is only a fallback
		// for documents that omit it.
		out = append(out, BootstrapSnapshots("", raw, ts)...)
	}
	return out
}
