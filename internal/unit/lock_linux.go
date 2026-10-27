//go:build linux

package unit

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// lockName is the probe's ownership lock in tmpDir. One probe owns a unit
// (or machine) at a time: everything it installs later, the charm loader and
// its spool, is shared by every hook on the host, so two recordings can't
// both own it.
const lockName = "juju-lens-unit.lock"

// owner is a held ownership lock. Its file holds the owner's PID; its mtime
// is the owner's last successful heartbeat.
type owner struct {
	f    *os.File
	path string
}

// busyError reports a live probe that owns the unit.
type busyError struct {
	pid int
	age time.Duration
}

func (e *busyError) Error() string {
	return fmt.Sprintf("another juju-lens recording owns this host: probe pid %d, last heartbeat %s ago", e.pid, e.age.Round(time.Second))
}

// takeOwnership acquires the lock at path. A holder whose heartbeat is older
// than stale was left behind by a recorder that went away without the probe
// noticing; it is killed and the lock taken over. A fresher holder belongs to
// a running recording, and takeOwnership returns a *busyError.
func takeOwnership(path string, stale time.Duration, logf func(string, ...any)) (*owner, error) {
	deadline := time.Now().Add(5 * time.Second)
	killed := 0
	for {
		f, err := openLock(path)
		if err != nil {
			return nil, err
		}
		if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			// The holder we waited for may have unlinked the file on its
			// way out; a lock on an unlinked inode excludes nobody.
			if sameFile(f, path) {
				o := &owner{f: f, path: path}
				if err := o.writePID(); err != nil {
					o.release()
					return nil, err
				}
				return o, nil
			}
			_ = f.Close()
			continue
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		pid, age := holder(f)
		_ = f.Close()
		if age < stale && killed == 0 {
			return nil, &busyError{pid: pid, age: age}
		}
		if pid > 0 && killed != pid {
			logf("unit probe: stopping probe pid %d, left behind (last heartbeat %s ago)", pid, age.Round(time.Second))
			_ = syscall.Kill(pid, syscall.SIGKILL)
			killed = pid
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("probe pid %d still holds %s after being stopped", pid, path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openLock opens the lock file without following a symlink planted in a
// world-writable directory, and refuses a file someone else owns.
func openLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not a regular file owned by uid %d; remove it", path, os.Geteuid())
	}
	return f, nil
}

func sameFile(f *os.File, path string) bool {
	a, err1 := f.Stat()
	b, err2 := os.Lstat(path)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

// holder reads the PID and heartbeat age of the lock's current holder.
func holder(f *os.File) (int, time.Duration) {
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	st, err := f.Stat()
	if err != nil {
		return pid, 0
	}
	return pid, time.Since(st.ModTime())
}

func (o *owner) writePID() error {
	if err := o.f.Truncate(0); err != nil {
		return err
	}
	_, err := o.f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return err
}

// touch records a successful heartbeat.
func (o *owner) touch() {
	now := time.Now()
	_ = os.Chtimes(o.path, now, now)
}

// release removes the lock file and drops the lock. Unlinking first, while
// still holding it, means a waiter that opened the old file sees it is gone
// (sameFile) instead of locking a file nobody else can see.
func (o *owner) release() {
	if sameFile(o.f, o.path) {
		_ = os.Remove(o.path)
	}
	_ = o.f.Close()
}
