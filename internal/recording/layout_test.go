package recording

import (
	"os"
	"path/filepath"
	"testing"
)

// RawSize is what --max-size caps, so it has to see every source under raw/,
// not only the RPC files, and nothing outside it.
func TestRawSizeCountsEverySourceUnderRaw(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Init(); err != nil {
		t.Fatal(err)
	}
	if got := l.RawSize(); got != 0 {
		t.Fatalf("empty raw/ = %d bytes, want 0", got)
	}
	for path, n := range map[string]int{
		filepath.Join(l.RawDir(), "rpc", "m", "calls-h.jsonl"):            100,
		filepath.Join(l.RawDir(), "unit", "m", "app-0", "unit-h.jsonl"):   2000,
		filepath.Join(l.RawDir(), "k8s", "m", "app-0", "c", "logs-h.log"): 30000,
		filepath.Join(l.RawDir(), "status", "m", "bootstrap.json"):        400,
		filepath.Join(l.Root, "index.db"):                                 99999,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := l.RawSize(), int64(100+2000+30000+400); got != want {
		t.Fatalf("RawSize = %d, want %d (index.db is not raw/)", got, want)
	}
}

func TestRawSizeOfMissingRawDirIsZero(t *testing.T) {
	if got := NewLayout(filepath.Join(t.TempDir(), "nothing")).RawSize(); got != 0 {
		t.Fatalf("RawSize = %d, want 0", got)
	}
}
