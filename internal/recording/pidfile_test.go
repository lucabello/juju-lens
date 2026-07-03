package recording

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPidFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recorder.pid")
	if err := WritePidFile(path, 12345); err != nil {
		t.Fatalf("WritePidFile: %v", err)
	}
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if pid != 12345 {
		t.Fatalf("PID = %d, want 12345", pid)
	}
	if err := RemovePidFile(path); err != nil {
		t.Fatalf("RemovePidFile: %v", err)
	}
	// Second remove is a no-op.
	if err := RemovePidFile(path); err != nil {
		t.Fatalf("RemovePidFile after remove: %v", err)
	}
}

func TestReadPidFileRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recorder.pid")
	if err := os.WriteFile(path, []byte("not a number"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPidFile(path); err == nil {
		t.Fatal("expected an error for non-numeric PID")
	}
}

func TestPidAliveForCurrentProcess(t *testing.T) {
	if !PidAlive(os.Getpid()) {
		t.Fatal("current process must be alive")
	}
	if PidAlive(0) {
		t.Fatal("PID 0 is never a live target")
	}
	if PidAlive(-1) {
		t.Fatal("negative PID is never a live target")
	}
}
