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

// OTLPDir returns raw/otlp/, holding OTLP payloads captured verbatim.
func (l Layout) OTLPDir() string { return filepath.Join(l.RawDir(), "otlp") }

// JujuDir returns raw/juju/<model>/, holding juju debug-log output per model.
func (l Layout) JujuDir(model string) string {
	return filepath.Join(l.RawDir(), "juju", sanitizeSegment(model))
}

// K8sDir returns raw/k8s/<model>/, holding pod logs per model.
func (l Layout) K8sDir(model string) string {
	return filepath.Join(l.RawDir(), "k8s", sanitizeSegment(model))
}

// MachineDir returns raw/machine/<model>/<host>/, holding host journals.
func (l Layout) MachineDir(model, host string) string {
	return filepath.Join(l.RawDir(), "machine", sanitizeSegment(model), sanitizeSegment(host))
}

// SnapDir returns raw/snap/<model>/<host>/, holding snap logs.
func (l Layout) SnapDir(model, host string) string {
	return filepath.Join(l.RawDir(), "snap", sanitizeSegment(model), sanitizeSegment(host))
}

// DerivedDir returns the derived/ directory where cached artifacts live.
func (l Layout) DerivedDir() string { return filepath.Join(l.Root, "derived") }

// IndexDB returns the path where the SQLite index would live. M1 does not
// build one, but the path is stable so later milestones can find it.
func (l Layout) IndexDB() string { return filepath.Join(l.Root, "index.db") }

// PidFile is where the active recorder writes its PID. `juju-lens stop`
// reads this file to send SIGTERM without needing shell job control.
func (l Layout) PidFile() string { return filepath.Join(l.Root, "recorder.pid") }

// OTLPTracesFile returns the JSONL file that OTLP trace requests are appended
// to during hour h. h is a UTC "2006-01-02T15" string.
func (l Layout) OTLPTracesFile(hour string) string {
	return filepath.Join(l.OTLPDir(), fmt.Sprintf("traces-%s.jsonl", hour))
}

// OTLPLogsFile returns the JSONL file for OTLP log requests during hour h.
func (l Layout) OTLPLogsFile(hour string) string {
	return filepath.Join(l.OTLPDir(), fmt.Sprintf("logs-%s.jsonl", hour))
}

// OTLPMetricsFile returns the JSONL file for OTLP metric requests during hour h.
func (l Layout) OTLPMetricsFile(hour string) string {
	return filepath.Join(l.OTLPDir(), fmt.Sprintf("metrics-%s.jsonl", hour))
}

// Init creates the directory skeleton for a fresh recording. It is idempotent:
// re-running against an existing recording is fine and does not clobber files.
func (l Layout) Init() error {
	if l.Root == "" {
		return errors.New("recording root is empty")
	}
	for _, d := range []string{l.Root, l.RawDir(), l.OTLPDir(), l.DerivedDir()} {
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
