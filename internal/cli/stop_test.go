package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// TestStopSignalsRunningRecorder starts a real subprocess that mimics a
// running recorder (sleeps until it receives SIGTERM), writes a valid pid
// file and manifest for it, then verifies `stop` signals it cleanly.
func TestStopSignalsRunningRecorder(t *testing.T) {
	dir := t.TempDir()
	layout := recording.NewLayout(dir)
	if err := layout.Init(); err != nil {
		t.Fatal(err)
	}
	man := recording.New("juju-lens", "test", "", "fake")
	if err := man.Save(layout.Root); err != nil {
		t.Fatal(err)
	}

	// exec replaces the shell with sleep so SIGTERM is delivered to
	// something that stops on it (sleep exits with 143 on SIGTERM). Using
	// bash's own trap is unreliable across shell versions.
	cmd := exec.Command("bash", "-c", `exec sleep 30`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting stub: %v", err)
	}
	// Reap the child in a goroutine so PidAlive (which uses signal(0))
	// stops seeing the zombie once SIGTERM is delivered.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	defer func() {
		select {
		case <-done:
			return
		default:
			_ = cmd.Process.Kill()
			<-done
		}
	}()
	if err := recording.WritePidFile(layout.PidFile(), cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}

	if err := runStop(dir, stopFlags{timeout: 5 * time.Second}); err != nil {
		t.Fatalf("runStop: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not exit after stop returned")
	}
}

func TestStopRejectsNonRecordingDir(t *testing.T) {
	dir := t.TempDir() // empty; no manifest
	err := runStop(dir, stopFlags{})
	if err == nil || !strings.Contains(err.Error(), "not a juju-lens recording") {
		t.Fatalf("want 'not a juju-lens recording' error, got %v", err)
	}
}

func TestStopReportsMissingPid(t *testing.T) {
	dir := t.TempDir()
	layout := recording.NewLayout(dir)
	if err := layout.Init(); err != nil {
		t.Fatal(err)
	}
	if err := recording.New("juju-lens", "test", "", "fake").Save(layout.Root); err != nil {
		t.Fatal(err)
	}
	err := runStop(dir, stopFlags{})
	if err == nil || !strings.Contains(err.Error(), "no recorder is running") {
		t.Fatalf("want 'no recorder is running' error, got %v", err)
	}
}

func TestStopClearsStalePid(t *testing.T) {
	dir := t.TempDir()
	layout := recording.NewLayout(dir)
	if err := layout.Init(); err != nil {
		t.Fatal(err)
	}
	if err := recording.New("juju-lens", "test", "", "fake").Save(layout.Root); err != nil {
		t.Fatal(err)
	}
	// PID 1 always exists on unix, so use a spawned+reaped process
	// instead. bash exits fast; by the time runStop reads the pid file
	// the PID is gone.
	cmd := exec.Command("bash", "-c", "true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := recording.WritePidFile(layout.PidFile(), cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	err := runStop(dir, stopFlags{})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want 'not running' error, got %v", err)
	}
	if _, statErr := os.Stat(layout.PidFile()); !os.IsNotExist(statErr) {
		t.Fatalf("stale pid file should have been removed, got stat err %v", statErr)
	}
	_ = filepath.Base
}
