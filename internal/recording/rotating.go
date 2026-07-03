package recording

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PathForHourFunc maps a UTC hour string ("2006-01-02T15") to a file path.
// It is injected so a single RotatingWriter can be used for traces, logs,
// or any other sink without hard-coding the file naming scheme.
type PathForHourFunc func(hour string) string

// RotatingWriter appends lines to an hourly file, rotating on wall-clock hour
// boundaries and on explicit Close. It is safe for concurrent use by multiple
// goroutines.
//
// The writer never compresses and never deletes older files. Rotation happens
// lazily on Write: if the current wall-clock hour differs from the open
// file's hour, the file is closed and the new one is opened.
//
// The zero value is not usable; construct via NewRotatingWriter.
type RotatingWriter struct {
	pathFor PathForHourFunc
	now     func() time.Time
	perm    os.FileMode

	mu     sync.Mutex
	file   *os.File
	hour   string // hour string of the currently-open file, empty if none
	bytes  int64  // bytes written to the currently-open file
	total  int64  // bytes written across the writer's lifetime
	closed bool
}

// NewRotatingWriter returns a writer whose files are named by pathFor(hour).
// nowFn is used to obtain the current time; pass nil to use time.Now.
func NewRotatingWriter(pathFor PathForHourFunc, nowFn func() time.Time) *RotatingWriter {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &RotatingWriter{pathFor: pathFor, now: nowFn, perm: 0o644}
}

// HourKey formats t (converted to UTC) as the hour key we use for rotation.
func HourKey(t time.Time) string { return t.UTC().Format("2006-01-02T15") }

// Write appends p (verbatim, followed by no newline) to the current hourly
// file. Callers are expected to include their own terminator when they want
// one; see WriteLine.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeLocked(p)
}

// WriteLine appends p followed by a single newline. It is the natural entry
// point for JSONL sinks.
func (w *RotatingWriter) WriteLine(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writeLocked(p)
	if err != nil {
		return n, err
	}
	nn, err := w.writeLocked([]byte{'\n'})
	return n + nn, err
}

func (w *RotatingWriter) writeLocked(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("rotating writer: closed")
	}
	now := w.now()
	hour := HourKey(now)
	if w.file == nil || hour != w.hour {
		if err := w.rotateLocked(hour); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.bytes += int64(n)
	w.total += int64(n)
	return n, err
}

func (w *RotatingWriter) rotateLocked(newHour string) error {
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			return fmt.Errorf("syncing before rotate: %w", err)
		}
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("closing before rotate: %w", err)
		}
		w.file = nil
	}
	path := w.pathFor(newHour)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating parent dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, w.perm)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	w.file = f
	w.hour = newHour
	w.bytes = 0
	return nil
}

// Total returns the byte count written across all rotations. Useful for size
// budgeting.
func (w *RotatingWriter) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// Close flushes and releases the underlying file. Subsequent writes fail.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
