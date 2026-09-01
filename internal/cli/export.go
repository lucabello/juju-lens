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
	out        string
	parts      []string
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
		Short: "Write a recording's derived narrative as a folder of agent-oriented artifacts",
		Long: `export distils a recording's derived narrative — hook runs and their
failure state, the statuses/databags/config they changed — merged with
every correlated log line, and writes it as a folder meant for an agent to
read incrementally instead of ingesting all at once:

  SUMMARY.md            orientation: time range, units, hook-outcome counts
  timeline.jsonl         events + logs merged, chronological, one JSON object
                         per line — grep -C around a line for real context
  events.jsonl           the same event lines, without the log volume
  details/<span_id>.json full detail for one event: statuses set, databag/
                         config diffs, failure explanation
  report.md              the same data as one narrated document, for a human
  AGENTS.md               legend naming whichever of the above is present

--parts restricts which of those get written (default: all); it is
independent of the scoping flags below, which restrict what content is in
scope regardless of which files it lands in.

Unlike the raw SQLite index, this carries the same hook/failure
classification the TUI shows (was this unit actually broken, or did it
retry and recover), so an agent debugging an issue doesn't have to
re-derive it from raw RPC spans.

--format, if set, instead writes a single merged document to stdout (json
or md) and skips the folder — a legacy/scripting path for piping into
another tool.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(args[0], *f, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&f.format, "format", "", "write a single merged document to stdout instead of a folder: json|md")
	cmd.Flags().StringVar(&f.out, "out", "", "folder to write the export into (default: <recording>/derived/export)")
	cmd.Flags().StringSliceVar(&f.parts, "parts", nil,
		"comma-separated subset of parts to write: summary,timeline,events,details,report (default: all)")
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

	if f.format != "" {
		switch f.format {
		case "json":
			return export.WriteJSON(out, result)
		case "md", "markdown":
			return export.WriteMarkdown(out, result)
		default:
			return fmt.Errorf("unknown --format %q (want json|md)", f.format)
		}
	}

	parts, err := parseParts(f.parts)
	if err != nil {
		return err
	}
	outDir := f.out
	if outDir == "" {
		outDir = recording.NewLayout(dir).ExportDir()
	}
	if err := export.WriteFolder(outDir, result, parts); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote export to %s\n", outDir)
	return nil
}

// parseParts validates --parts against the known Part values. An empty list
// means "all parts", handled the same way WriteFolder itself treats nil.
func parseParts(names []string) ([]export.Part, error) {
	if len(names) == 0 {
		return nil, nil
	}
	valid := make(map[export.Part]bool, len(export.AllParts))
	for _, p := range export.AllParts {
		valid[p] = true
	}
	out := make([]export.Part, 0, len(names))
	for _, n := range names {
		p := export.Part(n)
		if !valid[p] {
			return nil, fmt.Errorf("unknown --parts value %q (want one of summary,timeline,events,details,report)", n)
		}
		out = append(out, p)
	}
	return out, nil
}
