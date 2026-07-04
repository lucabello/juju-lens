package cli

import (
	"fmt"
	"os"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

func newIndexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "index <recording>",
		Short: "Rebuild the SQLite index for a recording from its raw/ tree",
		Long: `index reads every captured RPC under raw/rpc/, pairs requests with
responses into synthesised spans, extracts the derived snapshots
(application/unit status today; databags and more in later milestones), and
writes a fresh index.db at the recording root.

It is safe to run repeatedly. The DB is entirely rebuildable from raw/, so
deleting it and re-indexing is the recommended way to pick up schema
changes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIndex(args[0])
		},
	}
}

// runIndex is factored out so `record` can call it at the end of a
// recording without going through cobra.
func runIndex(dir string) error {
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

	spans, err := recording.LoadSpans(dir)
	if err != nil {
		return err
	}
	// Attribute each RPC to the hook the unit was running (from its SetState
	// operations) so the timeline reads as hooks, not bare method names.
	index.LabelHooks(spans)
	for i := range spans {
		// Enrich the span with the relation it concerns (from its params) so the
		// timeline and relations pane can pivot on it.
		if spans[i].Relation == "" {
			spans[i].Relation = index.RelationForSpan(spans[i])
		}
		if err := db.InsertSpan(spans[i]); err != nil {
			return fmt.Errorf("insert span %s: %w", spans[i].SpanID, err)
		}
	}
	snaps := index.ExtractSnapshots(spans)
	for _, s := range snaps {
		if err := db.InsertSnapshot(s.Model, s.Ts, string(s.Kind), s.Scope,
			string(s.Body), s.ProducingSpanID); err != nil {
			return fmt.Errorf("insert snapshot %s: %w", s.Scope, err)
		}
	}

	logs, err := recording.LoadLogs(dir)
	if err != nil {
		return err
	}
	for _, rec := range logs {
		if err := db.InsertLog(rec); err != nil {
			return fmt.Errorf("insert log record: %w", err)
		}
	}

	fmt.Fprintf(os.Stderr, "juju-lens: indexed %d spans, %d snapshots, %d log records into %s\n",
		len(spans), len(snaps), len(logs), layout.IndexDB())
	return nil
}
