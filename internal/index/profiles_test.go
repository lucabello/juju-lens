package index

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// TestProfilesRoundTrip: a profile reads back as it was indexed.
func TestProfilesRoundTrip(t *testing.T) {
	spans, profs, err := recording.LoadOps(filepath.Join("..", "recording", "testdata", "ops", "stacks"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, sp := range spans {
		if err := db.InsertSpan(sp); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range profs {
		if err := db.InsertProfile(p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.Profiles([]string{"no-such-root", profs[0].RootSpanID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0], profs[0]) {
		t.Errorf("read back %d profiles; first differs: %v", len(got), len(got) == 1)
	}
	if host, _ := db.SpanHost(profs[0].RootSpanID); host != "otelcol-0" {
		t.Errorf("host %q", host)
	}
}

// TestMemorySamplesSplit: the memory.stat split reads back when the index
// has its columns (schema v8), and is absent, not an error, when it hasn't.
func TestMemorySamplesSplit(t *testing.T) {
	res, err := recording.LoadUnitResources(filepath.Join("..", "recording", "testdata", "ops", "res-k8s"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InsertUnitResources(res); err != nil {
		t.Fatal(err)
	}
	id, _ := db.UpsertModel(res.Samples[0].Model)
	from, to := res.Samples[0].TS.Add(-time.Second), res.Samples[len(res.Samples)-1].TS
	before, err := db.MemorySamples(id, "otelcol-0", from, to)
	if err != nil || len(before) == 0 {
		t.Fatalf("%d samples, %v", len(before), err)
	}
	for _, s := range before {
		if s.Container == recording.ProbeContainer || s.Split || s.Events {
			t.Errorf("sample %+v", s)
		}
	}
	if !db.hasColumns("resource_samples", memStatCols) {
		for _, c := range append(memStatCols, memEventCols...) {
			if _, err := db.sql.Exec("ALTER TABLE resource_samples ADD COLUMN " + c + " INTEGER"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.sql.Exec(`UPDATE resource_samples SET mem_anon_bytes = mem_bytes / 2, mem_file_bytes = mem_bytes / 4,
	    mem_kernel_bytes = 0, mem_shmem_bytes = 0,
	    mem_high_events = 0, mem_max_events = 0, mem_oom_events = 0, mem_oom_kill_events = 1`); err != nil {
		t.Fatal(err)
	}
	after, err := db.MemorySamples(id, "otelcol-0", from, to)
	if err != nil || len(after) != len(before) {
		t.Fatalf("%d samples, %v", len(after), err)
	}
	for _, s := range after {
		if !s.Split || s.Anon != s.MemBytes/2 || s.File != s.MemBytes/4 || !s.Events || s.OOMKillEvents != 1 {
			t.Errorf("sample %+v", s)
		}
	}
}
