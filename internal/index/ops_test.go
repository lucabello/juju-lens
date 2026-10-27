package index

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/recording"
)

// TestIndexerOpsMatchesRebuild tails the lab's unit files as they grow, in
// chunks that cut lines in half, and checks the live index ends up with the
// same ops spans and profiles as a rebuild from the finished files.
func TestIndexerOpsMatchesRebuild(t *testing.T) {
	for _, fx := range []string{"k8s", "machine", "res-k8s", "res-machine", "stacks"} {
		src := filepath.Join("..", "recording", "testdata", "ops", fx)
		var files []string
		_ = filepath.WalkDir(filepath.Join(src, "raw", "unit"), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files = append(files, p)
			}
			return nil
		})
		if len(files) == 0 {
			t.Fatalf("%s: no unit files", fx)
		}

		dir := t.TempDir()
		if err := recording.NewLayout(dir).Init(); err != nil {
			t.Fatal(err)
		}
		live := openTempDB(t)
		ix := NewIndexer(live)
		data := map[string][]byte{}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(src, f)
			data[rel] = b
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		const chunk = 7919 // bytes: lands mid-line
		for done := false; !done; {
			done = true
			for rel, b := range data {
				if len(b) == 0 {
					continue
				}
				n := min(chunk, len(b))
				f, err := os.OpenFile(filepath.Join(dir, rel), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.Write(b[:n])
				f.Close()
				data[rel] = b[n:]
				done = false
			}
			if err := ix.Sync(dir); err != nil {
				t.Fatal(err)
			}
		}

		want, profiles, err := recording.LoadOps(dir)
		if err != nil {
			t.Fatal(err)
		}
		rebuilt := openTempDB(t)
		for _, sp := range want {
			if err := rebuilt.InsertSpan(sp); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range profiles {
			if err := rebuilt.InsertProfile(p); err != nil {
				t.Fatal(err)
			}
		}
		if fx == "stacks" && len(profiles) == 0 {
			t.Fatalf("%s: no profiles", fx)
		}
		if g, e := profileDump(t, live), profileDump(t, rebuilt); g != e {
			t.Fatalf("%s: live index profiles differ from the rebuild's:\nlive    %.400s\nrebuild %.400s", fx, g, e)
		}
		got, _ := live.Spans(0)
		exp, _ := rebuilt.Spans(0)
		if len(exp) != len(want) || len(got) != len(exp) {
			t.Fatalf("%s: live index has %d spans, rebuild %d (from %d)", fx, len(got), len(exp), len(want))
		}
		if !reflect.DeepEqual(got, exp) {
			for i := range got {
				if !reflect.DeepEqual(got[i], exp[i]) {
					t.Fatalf("%s: span %d differs:\nlive    %+v\nrebuild %+v", fx, i, got[i], exp[i])
				}
			}
		}

		res, err := recording.LoadUnitResources(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := rebuilt.InsertUnitResources(res); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			`SELECT host, unit, container, ts, cpu_usec, mem_bytes, mem_peak_bytes, mem_max_bytes, io_read_bytes, cpu_stall_usec FROM resource_samples ORDER BY host, container, ts`,
			`SELECT host, unit, ts, path, used_bytes FROM disk_samples ORDER BY host, path, ts`,
			`SELECT host, unit, container, change_id, kind, status, ts_spawn, ts_ready FROM pebble_changes ORDER BY host, container, change_id`,
		} {
			l, r := dumpRows(t, live, q), dumpRows(t, rebuilt, q)
			if !reflect.DeepEqual(l, r) {
				t.Errorf("%s: %s\nlive    %d rows\nrebuild %d rows", fx, q, len(l), len(r))
			}
			// Pebble changes only exist on Kubernetes.
			if strings.HasPrefix(fx, "res-") && len(l) == 0 && !(fx == "res-machine" && strings.Contains(q, "pebble_changes")) {
				t.Errorf("%s: no rows for %s", fx, q)
			}
		}
	}
}

// profileDump renders every row of the profile tables, in key order.
func profileDump(t *testing.T, db *DB) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT * FROM profiles ORDER BY root_span_id`,
		`SELECT * FROM profile_frames ORDER BY root_span_id, frame_id`,
		`SELECT * FROM profile_stacks ORDER BY root_span_id, stack_id`,
		`SELECT * FROM profile_samples ORDER BY root_span_id, seq`,
	} {
		rows := dumpRows(t, db, q)
		fmt.Fprintf(&b, "%s\n-- %d rows\n", strings.Join(rows, "\n"), len(rows))
	}
	return b.String()
}

// dumpRows renders a query's rows as strings, for comparing two indexes.
func dumpRows(t *testing.T, db *DB, q string) []string {
	t.Helper()
	rows, err := db.SQL().Query(q)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals...))
	}
	return out
}
