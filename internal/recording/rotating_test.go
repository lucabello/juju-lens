package recording

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRotatingWriterWritesToHourlyFile(t *testing.T) {
	dir := t.TempDir()
	fakeTime := time.Date(2026, 7, 3, 14, 30, 0, 0, time.UTC)
	pathFor := func(hour string) string { return filepath.Join(dir, "out-"+hour+".jsonl") }
	w := NewRotatingWriter(pathFor, func() time.Time { return fakeTime })

	if _, err := w.WriteLine([]byte(`{"a":1}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	if _, err := w.WriteLine([]byte(`{"b":2}`)); err != nil {
		t.Fatalf("WriteLine: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "out-2026-07-03T14.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("{\"a\":1}\n{\"b\":2}\n")
	if !bytes.Equal(got, want) {
		t.Fatalf("file contents mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestRotatingWriterRotatesOnHourBoundary(t *testing.T) {
	dir := t.TempDir()
	var (
		mu  sync.Mutex
		now = time.Date(2026, 7, 3, 14, 59, 59, 0, time.UTC)
	)
	pathFor := func(hour string) string { return filepath.Join(dir, "out-"+hour+".jsonl") }
	w := NewRotatingWriter(pathFor, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	if _, err := w.WriteLine([]byte("first")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = time.Date(2026, 7, 3, 15, 0, 1, 0, time.UTC)
	mu.Unlock()
	if _, err := w.WriteLine([]byte("second")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 files after rotation, got %d: %v", len(names), names)
	}
	// Both hours represented.
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "T14.jsonl") || !strings.Contains(joined, "T15.jsonl") {
		t.Fatalf("expected T14 and T15 files, got %v", names)
	}
}

func TestRotatingWriterConcurrentSafety(t *testing.T) {
	dir := t.TempDir()
	pathFor := func(hour string) string { return filepath.Join(dir, "out-"+hour+".jsonl") }
	w := NewRotatingWriter(pathFor, func() time.Time { return time.Unix(0, 0).UTC() })
	defer w.Close()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := w.WriteLine([]byte("x")); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// 20*50 lines * (1 byte + '\n')
	if w.Total() != 20*50*2 {
		t.Fatalf("Total() = %d, want %d", w.Total(), 20*50*2)
	}
}

func TestRotatingWriterRefusesWriteAfterClose(t *testing.T) {
	w := NewRotatingWriter(func(string) string { return t.TempDir() + "/x" }, nil)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteLine([]byte("x")); err == nil {
		t.Fatal("expected error writing after Close")
	}
}
