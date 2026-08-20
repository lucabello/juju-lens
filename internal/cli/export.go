package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/lucabello/juju-lens/internal/export"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

type exportFlags struct {
	format     string
	model      string
	units      []string
	since      string
	until      string
	errorsOnly bool
}

func newExportCmd() *cobra.Command {
	f := &exportFlags{}
	cmd := &cobra.Command{
		Use:   "export <recording>",
		Short: "Dump a merged, time-ordered narrative of events and logs for offline or LLM analysis",
		Long: `export distils a recording's derived narrative — hook runs and their
failure state, the statuses/databags/config they changed — merged with
every correlated log line, into one time-ordered document. Unlike the raw
SQLite index, this carries the same hook/failure classification the TUI
shows (was this unit actually broken, or did it retry and recover), so an
agent debugging an issue doesn't have to re-derive it from raw RPC spans.

--format md (the default) is meant to be read top to bottom or pasted into
an LLM chat; --format json is meant to be consumed by a script or tool.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(args[0], *f, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&f.format, "format", "md", "output format: json|md")
	cmd.Flags().StringVar(&f.model, "model", "", "focus on a single model (required if the recording has more than one)")
	cmd.Flags().StringArrayVar(&f.units, "unit", nil, "restrict to this unit or application (repeatable)")
	cmd.Flags().StringVar(&f.since, "since", "", "RFC3339 lower time bound")
	cmd.Flags().StringVar(&f.until, "until", "", "RFC3339 upper time bound")
	cmd.Flags().BoolVar(&f.errorsOnly, "errors-only", false,
		"only hook runs that ended errored/retried/rpc-warn, plus one same-unit neighbour on each side for context")
	return cmd
}

// runExport is factored out of RunE so it's directly testable without going
// through cobra, matching runIndex/runStop's convention.
func runExport(dir string, f exportFlags, out io.Writer) error {
	ok, err := recording.NewLayout(dir).Exists()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no manifest.json under %q; is this a juju-lens recording?", dir)
	}

	opts := export.Options{Model: f.model, Units: f.units, ErrorsOnly: f.errorsOnly}
	if f.since != "" {
		t, err := time.Parse(time.RFC3339, f.since)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		opts.Since = t
	}
	if f.until != "" {
		t, err := time.Parse(time.RFC3339, f.until)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		opts.Until = t
	}

	result, err := export.Build(dir, opts)
	if err != nil {
		return err
	}
	switch f.format {
	case "json":
		return export.WriteJSON(out, result)
	case "md", "markdown", "":
		return export.WriteMarkdown(out, result)
	default:
		return fmt.Errorf("unknown --format %q (want json|md)", f.format)
	}
}
