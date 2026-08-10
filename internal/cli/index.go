package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

func newIndexCmd() *cobra.Command {
	var renames []string
	cmd := &cobra.Command{
		Use:   "index <recording>",
		Short: "Rebuild the SQLite index for a recording from its raw/ tree",
		Long: `index reads every captured RPC under raw/rpc/, pairs requests with
responses into synthesised spans, extracts the derived snapshots
(application/unit status today; databags and more in later milestones), and
writes a fresh index.db at the recording root.

It is safe to run repeatedly. The DB is entirely rebuildable from raw/, so
deleting it and re-indexing is the recommended way to pick up schema
changes.

Use --rename to unify entries that were captured under different identities
for the same model. This happens when a model is created mid-recording and its
RPCs are attributed to the raw model UUID while its logs use the model name;
--rename OLD=NEW merges OLD's data into NEW so the viewer shows one model.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := parseRenames(renames)
			if err != nil {
				return err
			}
			return runIndex(args[0], m)
		},
	}
	cmd.Flags().StringArrayVar(&renames, "rename", nil,
		"remap a model during rebuild, as OLD=NEW (repeatable); merges OLD's spans, snapshots and logs into NEW")
	return cmd
}

// parseRenames turns "OLD=NEW" flag values into a model-rename map. An entry
// with an empty side is rejected so a typo does not silently drop a model's
// data onto the empty name.
func parseRenames(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		old, newName, ok := strings.Cut(p, "=")
		if !ok || old == "" || newName == "" {
			return nil, fmt.Errorf("invalid --rename %q; want OLD=NEW", p)
		}
		m[old] = newName
	}
	return m, nil
}

// runIndex is factored out so `record` can call it at the end of a
// recording without going through cobra. renames optionally merges models
// captured under different identities (see rebuildIndex).
func runIndex(dir string, renames map[string]string) error {
	layout := recording.NewLayout(dir)
	ok, err := layout.Exists()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no manifest.json under %q; is this a juju-lens recording?", dir)
	}

	// A fresh index avoids surprises when re-indexing after schema changes.
	// The file is derived; removing it is safe.
	if err := os.Remove(layout.IndexDB()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing old index: %w", err)
	}

	db, err := index.Open(layout.IndexDB())
	if err != nil {
		return err
	}
	defer db.Close()
	return rebuildIndex(db, dir, renames, true)
}

// rebuildIndex performs a full, authoritative (re)build of an open index from a
// recording's raw/ tree: it resets the DB data then loads, enriches, and writes
// every span, snapshot and log record. Because it resets rather than recreating
// the file, it can refresh an index other processes have open (a live
// `view --follow`). Set verbose to log a summary.
//
// renames optionally remaps the model each span/snapshot/log is attributed to,
// applied before insertion. It unifies data captured under different identities
// for the same model (e.g. RPCs keyed by a raw model UUID and logs keyed by the
// model name, which happens when a model is created mid-recording before its
// UUID could be resolved to a name).
func rebuildIndex(db *index.DB, dir string, renames map[string]string, verbose bool) error {
	if err := db.Reset(); err != nil {
		return err
	}
	spans, err := recording.LoadSpans(dir)
	if err != nil {
		return err
	}
	// Attribute each RPC to the hook the unit was running (from its SetState
	// operations) so the timeline reads as hooks, not bare method names.
	index.LabelHooks(spans)
	for i := range spans {
		spans[i].Model = renameModel(renames, spans[i].Model)
		// Enrich the span with the relation it concerns (from its params) so the
		// timeline and relations pane can pivot on it.
		if spans[i].Relation == "" {
			spans[i].Relation = index.RelationForSpan(spans[i])
		}
		if err := db.InsertSpan(spans[i]); err != nil {
			return fmt.Errorf("insert span %s: %w", spans[i].SpanID, err)
		}
	}
	// Ground-truth status bootstrap (M8): seed baseline app/unit status from the
	// `juju status` captured at recording start, stamped just before the first
	// captured RPC so every later delta sorts after it (and dedups against it).
	// These MUST be inserted before the RPC-derived snapshots below so the
	// content-hash dedup baseline is the bootstrap value, not a later delta.
	boots := index.LoadBootstrapSnapshots(dir, bootstrapTs(dir, spans))
	for _, s := range boots {
		if err := db.InsertBootstrapSnapshot(renameModel(renames, s.Model), s.Ts, string(s.Kind), s.Scope, string(s.Body)); err != nil {
			return fmt.Errorf("insert bootstrap snapshot %s: %w", s.Scope, err)
		}
	}
	snaps := index.ExtractSnapshots(spans)
	for _, s := range snaps {
		if err := db.InsertSnapshot(renameModel(renames, s.Model), s.Ts, string(s.Kind), s.Scope,
			string(s.Body), s.ProducingSpanID); err != nil {
			return fmt.Errorf("insert snapshot %s: %w", s.Scope, err)
		}
	}
	logs, err := recording.LoadLogs(dir)
	if err != nil {
		return err
	}
	for _, rec := range logs {
		rec.Model = renameModel(renames, rec.Model)
		if err := db.InsertLog(rec); err != nil {
			return fmt.Errorf("insert log record: %w", err)
		}
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "juju-lens: indexed %d spans, %d snapshots (%d bootstrap), %d log records\n",
			len(spans), len(snaps), len(boots), len(logs))
	}
	return nil
}

// renameModel maps a model name through the rename table, returning it
// unchanged when there is no entry. A nil table is a no-op.
func renameModel(renames map[string]string, model string) string {
	if to, ok := renames[model]; ok {
		return to
	}
	return model
}

// bootstrapTs is the instant to stamp ground-truth status snapshots at: just
// before the earliest captured RPC, so they precede (and form the dedup
// baseline for) every RPC-derived snapshot. Falls back to the manifest's start
// time, then to now, when there are no spans. The 1ms backdate keeps the
// bootstrap strictly before the first span so point-in-time queries never
// return a bootstrap row tied with a real delta.
func bootstrapTs(dir string, spans []recording.SpanRow) time.Time {
	t := time.Now().UTC()
	if man, err := recording.Load(dir); err == nil && !man.Started.IsZero() {
		t = man.Started
	}
	if len(spans) > 0 && spans[0].Start.Before(t) {
		t = spans[0].Start
	}
	return t.Add(-time.Millisecond)
}
