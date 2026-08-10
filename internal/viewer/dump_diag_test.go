package viewer

// Diagnostic harness (not a real test): point it at a recording and dump how the
// event parser interprets it, next to the raw uniter-state markers it derives
// from. Run with:  REC=/path/to/recording go test ./internal/viewer/ -run TestDumpCapture -v
// Delete once the event aggregation is settled.

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"
)

func TestDumpCapture(t *testing.T) {
	dir := os.Getenv("REC")
	if dir == "" {
		t.Skip("set REC=<recording dir> to dump a capture")
	}
	// The manifest is only informational here, and a sudo-run capture may leave it
	// unreadable to us — don't let that stop the dump.
	if _, err := recording.Load(dir); err != nil {
		fmt.Printf("(manifest unreadable: %v)\n", err)
	}
	dbPath := os.Getenv("DB")
	if dbPath == "" {
		dbPath = recording.NewLayout(dir).IndexDB()
	}
	db, err := index.Open(dbPath)
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()

	models, _ := db.Models()
	for _, mm := range models {
		spans, err := db.Spans(mm.ID)
		if err != nil {
			t.Fatalf("spans: %v", err)
		}
		dbChanged, _ := db.ProducingSpanIDs(mm.ID, string(index.KindDatabag))
		fmt.Printf("\n########## model %s — %d spans ##########\n", mm.Name, len(spans))

		dumpRawMarkers(spans)
		dumpHookRuns(spans)
		dumpEvents(spans, dbChanged)
	}
}

// dumpRawMarkers prints, per unit, every hook-relevant span in time order: the
// SetState uniter-state markers (op / hook.kind), CommitHookChanges, and the
// hook label the indexer attached. This is the ground truth the parser sees.
func dumpRawMarkers(spans []recording.SpanRow) {
	fmt.Printf("\n===== RAW hook-relevant spans (per unit, time order) =====\n")
	byUnit := map[string][]recording.SpanRow{}
	var units []string
	for _, sp := range spans {
		if sp.Unit == "" {
			continue
		}
		if _, ok := byUnit[sp.Unit]; !ok {
			units = append(units, sp.Unit)
		}
		byUnit[sp.Unit] = append(byUnit[sp.Unit], sp)
	}
	sort.Strings(units)
	for _, u := range units {
		fmt.Printf("\n-- %s --\n", u)
		for _, sp := range byUnit[u] {
			method := sp.Attrs["method"]
			marker := ""
			if method == "SetState" {
				hm := index.ParseHookMarker(sp.Attrs["params"])
				marker = fmt.Sprintf("  op=%-10s hook.kind=%q remoteApp=%q storageID=%q",
					hm.Op, hm.Kind, hm.RemoteApp, hm.StorageID)
			}
			if method == "SetState" || method == "CommitHookChanges" || sp.Hook != "" {
				fmt.Printf("  %s  %-22s label=%-40q%s\n",
					sp.Start.UTC().Format("15:04:05.000"), method, sp.Hook, marker)
			}
		}
	}
}

// dumpHookRuns prints the runs buildHookRuns pairs — one line per run, so a hook
// that splits into several runs (the suspected duplication) is visible.
func dumpHookRuns(spans []recording.SpanRow) {
	fmt.Printf("\n===== HOOK RUNS (buildHookRuns) =====\n")
	for _, r := range buildHookRuns(spans) {
		dur := r.endTs.Sub(r.startTs)
		flags := ""
		if r.fail != failNone {
			flags += " " + strings.ToUpper(r.fail.String())
		}
		if r.open {
			flags += " OPEN"
		}
		fmt.Printf("  %s  %-14s %-40s dur=%-8v rep=%s%s\n",
			r.startTs.UTC().Format("15:04:05.000"), r.unit, r.kind, dur.Round(1e6), r.repSpan, flags)
	}
}

// dumpEvents prints the final event list (default view = hooks only, then the
// verbose-only extras), i.e. exactly what the timeline pane shows.
func dumpEvents(spans []recording.SpanRow, dbChanged map[string]bool) {
	evs := buildEvents(spans, dbChanged)
	var def, verbose int
	fmt.Printf("\n===== EVENTS (default view: hooks only) =====\n")
	for _, e := range evs {
		if e.verboseOnly {
			verbose++
			continue
		}
		def++
		printEvent(e)
	}
	fmt.Printf("\n===== EVENTS (verbose-only extras) =====\n")
	for _, e := range evs {
		if e.verboseOnly {
			printEvent(e)
		}
	}
	fmt.Printf("\n>>> %d default (hook) events, %d verbose-only, %d total\n", def, verbose, def+verbose)
}

func printEvent(e event) {
	g := " "
	if e.verboseOnly {
		g = "·"
	}
	extra := ""
	switch {
	case e.fail != failNone:
		extra = "  " + strings.ToUpper(e.fail.String())
	case e.running:
		extra = "  (running)"
	case e.dur > 0:
		extra = fmt.Sprintf("  dur=%v", e.dur.Round(1e6))
	}
	if e.hasDatabag {
		extra += "  ✎db"
	}
	fmt.Printf("  %s  %-14s %s %-40s%s\n",
		e.ts.UTC().Format("15:04:05.000"), e.unit, g, e.summary, extra)
}
