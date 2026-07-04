// Package recording defines the on-disk layout of a juju-lens recording and
// the helpers to create, open, and describe one.
//
// A recording is a directory. Every stream is captured as plain text (usually
// JSONL). A rebuildable SQLite index and derived caches live alongside the raw
// data. This package is the single source of truth for the directory layout;
// every other component asks a Layout for the exact path it should read or
// write.
package recording

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ManifestFilename is the name of the manifest at the recording root.
const ManifestFilename = "manifest.json"

// EndReason describes why a recording stopped.
type EndReason string

const (
	EndReasonUnknown       EndReason = ""
	EndReasonSignal        EndReason = "signal"
	EndReasonMaxDuration   EndReason = "max-duration"
	EndReasonMaxSize       EndReason = "max-size"
	EndReasonError         EndReason = "error"
	EndReasonSynth         EndReason = "synth"
	EndReasonUserRequested EndReason = "user-requested"
)

// SourceStatus captures whether an ingester ran successfully.
type SourceStatus struct {
	Name    string    `json:"name"`
	Kind    string    `json:"kind"` // "rpc", "juju-debug-log", "k8s", "journal", "snap", "synth"
	Started time.Time `json:"started"`
	Stopped time.Time `json:"stopped,omitempty"`
	Records int64     `json:"records,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// ModelInfo identifies a Juju model that appears in the recording.
type ModelInfo struct {
	Name string `json:"name"`
	UUID string `json:"uuid,omitempty"`
	Type string `json:"type,omitempty"` // "iaas" or "caas"
}

// Manifest is the authoritative description of a recording. It is written
// once at the start (with best-effort fields) and rewritten atomically on
// finalization with the true end time, source stats, and end reason.
type Manifest struct {
	// SchemaVersion of this manifest layout. Bump when the manifest itself
	// changes shape. Not to be confused with the Juju schema version.
	SchemaVersion int `json:"schema_version"`

	Tool struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Commit  string `json:"commit,omitempty"`
	} `json:"tool"`

	// Controller is the human-friendly name used to build the recording
	// directory. It is not authoritative; a real controller UUID will be
	// filled in once the recorder connects.
	Controller struct {
		Name    string `json:"name"`
		UUID    string `json:"uuid,omitempty"`
		Version string `json:"version,omitempty"`
	} `json:"controller"`

	Models []ModelInfo `json:"models,omitempty"`

	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitempty"`
	EndReason EndReason `json:"end_reason,omitempty"`

	// Sources records every ingester that ran. The list is append-only.
	Sources []SourceStatus `json:"sources,omitempty"`

	// AttachMode is how the recorder reached the target processes' TLS
	// boundary: "ssh" (machine controllers), "kubectl-debug" or "daemonset"
	// (k8s), or "local" when the probe ran on this host. The viewer surfaces
	// it so an operator can tell how a recording was produced. The recorder
	// never mutates the controller, so there is nothing to restore.
	AttachMode string `json:"attach_mode,omitempty"`

	// AttachTargets lists the hosts/nodes the probe was attached on, for
	// operator visibility. Empty for synthetic recordings.
	AttachTargets []string `json:"attach_targets,omitempty"`
}

// New builds a manifest with the given tool and controller identity. Started
// is set to the current wall-clock time.
func New(toolName, toolVersion, toolCommit, controllerName string) *Manifest {
	m := &Manifest{
		SchemaVersion: 1,
		Started:       time.Now().UTC(),
	}
	m.Tool.Name = toolName
	m.Tool.Version = toolVersion
	m.Tool.Commit = toolCommit
	m.Controller.Name = controllerName
	return m
}

// SuggestedDirName returns a "<YYYY-MM-DDTHH-MM-SS>--<controller>" string
// suitable for creating a recording directory.
func SuggestedDirName(controller string, t time.Time) string {
	if controller == "" {
		controller = "unknown"
	}
	return fmt.Sprintf("%s--%s", t.UTC().Format("2006-01-02T15-04-05"), controller)
}

// AddSource records an ingester that started running.
func (m *Manifest) AddSource(s SourceStatus) {
	m.Sources = append(m.Sources, s)
}

// FinishSource updates the last matching source with a stop time and any
// terminal error. If no match is found, a new record is appended.
func (m *Manifest) FinishSource(name string, err error) {
	for i := len(m.Sources) - 1; i >= 0; i-- {
		if m.Sources[i].Name == name && m.Sources[i].Stopped.IsZero() {
			m.Sources[i].Stopped = time.Now().UTC()
			if err != nil {
				m.Sources[i].Error = err.Error()
			}
			return
		}
	}
	end := time.Now().UTC()
	src := SourceStatus{Name: name, Stopped: end}
	if err != nil {
		src.Error = err.Error()
	}
	m.Sources = append(m.Sources, src)
}

// Finalize stamps the end time and end reason. It does not write the file;
// callers should call Save afterwards.
func (m *Manifest) Finalize(reason EndReason) {
	m.Ended = time.Now().UTC()
	m.EndReason = reason
}

// Save writes the manifest to <dir>/manifest.json atomically.
func (m *Manifest) Save(dir string) error {
	if dir == "" {
		return errors.New("recording dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, ManifestFilename)
	tmp, err := os.CreateTemp(dir, ".manifest-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Load reads and parses <dir>/manifest.json.
func Load(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestFilename))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return &m, nil
}
