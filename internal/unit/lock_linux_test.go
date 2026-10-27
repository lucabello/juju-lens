//go:build linux

package unit

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// holdLock starts a process that holds an flock on path, as another probe
// would, and writes its PID into the file with the given heartbeat age.
func holdLock(t *testing.T, path string, age time.Duration) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock(1) not installed")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// One process holds the lock, as a probe does: the shell takes it on a
	// descriptor and then becomes sleep, keeping the descriptor and the PID.
	cmd := exec.Command("sh", "-c", `exec 9<>"$0"; flock 9 && exec sleep 60`, path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	// Wait until the lock is actually held.
	for i := 0; ; i++ {
		f, _ := os.Open(path)
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		_ = f.Close()
		if err != nil {
			break
		}
		if i > 100 {
			t.Fatal("helper never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-age)
	if err := os.Chtimes(path, then, then); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestTakeOwnershipBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), lockName)
	h := holdLock(t, path, time.Second)
	_, err := takeOwnership(path, StaleAfter, t.Logf)
	var busy *busyError
	if !errors.As(err, &busy) || busy.pid != h.Process.Pid {
		t.Fatalf("want busyError naming pid %d, got %v", h.Process.Pid, err)
	}
	if err := unix.Kill(h.Process.Pid, 0); err != nil {
		t.Fatalf("a live holder must not be stopped: %v", err)
	}
}

func TestTakeOwnershipStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), lockName)
	h := holdLock(t, path, 2*StaleAfter)
	o, err := takeOwnership(path, StaleAfter, t.Logf)
	if err != nil {
		t.Fatalf("taking over a stale lock: %v", err)
	}
	defer o.release()
	_ = h.Wait()
	if h.ProcessState == nil || h.ProcessState.Success() {
		t.Fatalf("stale holder should have been killed, state %v", h.ProcessState)
	}
	b, _ := os.ReadFile(path)
	if string(b) != strconv.Itoa(os.Getpid())+"\n" {
		t.Fatalf("lock file holds %q, want our pid", b)
	}
}

func TestReleaseRemovesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), lockName)
	o, err := takeOwnership(path, StaleAfter, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	o.release()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("lock file left after release: %v", err)
	}
	// A new owner can take it at once.
	o, err = takeOwnership(path, StaleAfter, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	o.release()
}

func TestTakeOwnershipRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, lockName)
	if err := os.Symlink(filepath.Join(dir, "target"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := takeOwnership(path, StaleAfter, t.Logf); err == nil {
		t.Fatal("want an error for a symlinked lock path")
	}
	if _, err := os.Lstat(filepath.Join(dir, "target")); !os.IsNotExist(err) {
		t.Fatal("followed the symlink")
	}
}

func TestRemoveLeftovers(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * leftoverAge)
	write := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := write(UploadPrefix+"1-abc.part", old)
	fresh := write(UploadPrefix+"2-def.part", time.Now())
	other := write("unrelated", old)
	removeLeftovers(dir, t.TempDir(), t.Logf)
	for p, want := range map[string]bool{stale: false, fresh: true, other: true} {
		if _, err := os.Lstat(p); (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}
