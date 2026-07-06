package recording

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Layout knows every path in a recording directory. All accessors are pure
// functions of the recording root; they never touch the filesystem unless
// noted (only Ensure*/Init do). This keeps the layout easy to reason about
// and easy to test.
type Layout struct {
	Root string
}

// New returns a Layout rooted at dir.
func NewLayout(dir string) Layout {
	return Layout{Root: dir}
}

// Manifest returns the path to the manifest file.
func (l Layout) Manifest() string { return filepath.Join(l.Root, ManifestFilename) }

// RawDir returns the path to the raw/ directory.
func (l Layout) RawDir() string { return filepath.Join(l.Root, "raw") }

// RPCDir returns raw/rpc/<model>/, holding captured Juju API RPC envelopes
// (one CapturedMessage per line) for a single model.
func (l Layout) RPCDir(model string) string {
	return filepath.Join(l.RawDir(), "rpc", sanitizeSegment(model))
}

// JujuDir returns raw/juju/<model>/, holding juju debug-log output per model.
func (l Layout) JujuDir(model string) string {
	return filepath.Join(l.RawDir(), "juju", sanitizeSegment(model))
}

// JujuLogFileFor returns a PathForHourFunc naming the per-hour debug-log file
// for a single model: raw/juju/<model>/debug-log-<hour>.log. The files hold the
// raw `juju debug-log` text lines verbatim (greppable; the index parses them).
func (l Layout) JujuLogFileFor(model string) func(hour string) string {
	dir := l.JujuDir(model)
	return func(hour string) string {
		return filepath.Join(dir, fmt.Sprintf("debug-log-%s.log", hour))
	}
}

// K8sDir returns raw/k8s/<model>/, holding pod logs per model.
func (l Layout) K8sDir(model string) string {
	return filepath.Join(l.RawDir(), "k8s", sanitizeSegment(model))
}

// K8sLogFileFor names the per-hour workload-log file for one pod container:
// raw/k8s/<model>/<pod>/<container>/logs-<hour>.log. The pod and container form
// the two path segments ParseLogLine reads back to attribute the line to a unit
// and container (they are DNS labels, so they never need sanitising).
func (l Layout) K8sLogFileFor(model, pod, container string) func(hour string) string {
	dir := filepath.Join(l.K8sDir(model), sanitizeSegment(pod), sanitizeSegment(container))
	return func(hour string) string {
		return filepath.Join(dir, fmt.Sprintf("logs-%s.log", hour))
	}
}

// MachineDir returns raw/machine/<model>/<host>/, holding host journals.
func (l Layout) MachineDir(model, host string) string {
	return filepath.Join(l.RawDir(), "machine", sanitizeSegment(model), sanitizeSegment(host))
}

// MachineLogFileFor names the per-hour journald file for one host:
// raw/machine/<model>/<host>/journal-<hour>.log. The host is the path segment
// ParseLogLine reads back as the log Entity.
func (l Layout) MachineLogFileFor(model, host string) func(hour string) string {
	dir := l.MachineDir(model, host)
	return func(hour string) string {
		return filepath.Join(dir, fmt.Sprintf("journal-%s.log", hour))
	}
}

// SnapDir returns raw/snap/<model>/<host>/, holding snap logs.
func (l Layout) SnapDir(model, host string) string {
	return filepath.Join(l.RawDir(), "snap", sanitizeSegment(model), sanitizeSegment(host))
}

// StatusDir returns raw/status/<model>/, holding the ground-truth `juju status`
// snapshot captured once at recording start (M8).
func (l Layout) StatusDir(model string) string {
	return filepath.Join(l.RawDir(), "status", sanitizeSegment(model))
}

// StatusBootstrapFile names the ground-truth status file for a model:
// raw/status/<model>/bootstrap.json. It holds the verbatim `juju status
// --format=json` output captured at recording start; the indexer seeds baseline
// app/unit status snapshots from it so the Status pane is populated from t0
// instead of showing every scope as "unknown".
func (l Layout) StatusBootstrapFile(model string) string {
	return filepath.Join(l.StatusDir(model), "bootstrap.json")
}

// DerivedDir returns the derived/ directory where cached artifacts live.
func (l Layout) DerivedDir() string { return filepath.Join(l.Root, "derived") }

// IndexDB returns the path where the SQLite index would live. M1 does not
// build one, but the path is stable so later milestones can find it.
func (l Layout) IndexDB() string { return filepath.Join(l.Root, "index.db") }

// PidFile is where the active recorder writes its PID. `juju-lens stop`
// reads this file to send SIGTERM without needing shell job control.
func (l Layout) PidFile() string { return filepath.Join(l.Root, "recorder.pid") }

// RPCFileFor returns a PathForHourFunc that names the per-hour calls file for
// a single model: raw/rpc/<model>/calls-<hour>.jsonl. Pass the result to a
// RotatingWriter so one writer exists per model.
func (l Layout) RPCFileFor(model string) func(hour string) string {
	dir := l.RPCDir(model)
	return func(hour string) string {
		return filepath.Join(dir, fmt.Sprintf("calls-%s.jsonl", hour))
	}
}

// Init creates the directory skeleton for a fresh recording. It is idempotent:
// re-running against an existing recording is fine and does not clobber files.
// Per-model raw/rpc/<model>/ directories are created lazily by the writers as
// models are discovered, so Init only lays down the always-present roots.
func (l Layout) Init() error {
	if l.Root == "" {
		return errors.New("recording root is empty")
	}
	for _, d := range []string{l.Root, l.RawDir(), l.DerivedDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}
	return nil
}

// Exists reports whether the recording root contains a manifest file. It is
// the check used by `view` to refuse to open random directories.
func (l Layout) Exists() (bool, error) {
	_, err := os.Stat(l.Manifest())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// sanitizeSegment turns arbitrary user-supplied strings (model names, host
// names) into safe path segments. It is intentionally aggressive: keep only
// letters, digits, dot, dash, and underscore; collapse everything else to '_'.
// This never returns an empty string.
func sanitizeSegment(s string) string {
	if s == "" {
		return "_"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
