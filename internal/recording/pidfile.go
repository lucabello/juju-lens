package recording

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// WritePidFile drops the current PID into the recording so `juju-lens stop`
// can find it. Atomic write to survive crashes mid-record.
func WritePidFile(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", pid)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadPidFile parses the PID out of a recorder.pid. Returns 0 (with a
// wrapped error) on missing or malformed files.
func ReadPidFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid PID %q in %s: %w", s, path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("non-positive PID %d in %s", pid, path)
	}
	return pid, nil
}

// RemovePidFile deletes the pid file, ignoring "does not exist" errors so
// double-cleanup is safe.
func RemovePidFile(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PidAlive reports whether the given PID currently refers to a running
// process. It uses signal 0 which never delivers but does validate that
// the PID exists and is signal-reachable.
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists but we can't signal it. Treat that
	// as "alive" — it is; the operator likely started the recorder as
	// another user.
	return errors.Is(err, syscall.EPERM)
}
