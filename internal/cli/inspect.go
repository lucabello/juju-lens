package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/lucabello/juju-lens/internal/export"
	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/inspect"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/spf13/cobra"
)

// newInspectCmd is `juju-lens inspect`: questions about a recording, answered
// as bounded text or JSON. It reads the same index as `view` and `export`,
// and builds it first when the recording has none.
func newInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Ask a recording questions: events, one event in full, profiles",
		Long: `inspect answers questions about a recording as short text, or as JSON with
--json, so that an agent (or a person at a shell) can ask for what it needs
instead of reading every file of an export. Every list is bounded, says how
much it left out, and ends with the command to go on with.

  juju-lens inspect events <recording>            list and rank events
  juju-lens inspect event <recording> <span-id>   one event in full
  juju-lens inspect profile <recording> [view]    where the charm's time and memory went

A recording without an index.db, or with one from an older juju-lens, is
indexed first.`,
	}
	cmd.AddCommand(newInspectEventsCmd(), newInspectEventCmd(), newInspectProfileCmd())
	return cmd
}

// scopeFlags are the flags that pick events, shared by the inspect commands.
type scopeFlags struct {
	model string
	units []string
	names []string
	kinds []string
	fail  []string
	since string
	until string
}

func (f *scopeFlags) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.model, "model", "", "the model to read (required if the recording has more than one)")
	fl.StringArrayVar(&f.units, "unit", nil, "only this unit or application (repeatable)")
	fl.StringArrayVar(&f.names, "name", nil, "only events whose name contains this, such as config-changed (repeatable)")
	fl.StringArrayVar(&f.kinds, "kind", nil, "only this kind of event: "+joinValues(inspect.EventKinds)+" (repeatable)")
	fl.StringArrayVar(&f.fail, "fail", nil, "only events that ended this way: problem (any but none), "+joinValues(inspect.FailStates)+" (repeatable)")
	fl.StringVar(&f.since, "since", "", "RFC3339 lower time bound")
	fl.StringVar(&f.until, "until", "", "RFC3339 upper time bound")
}

func joinValues(v []string) string {
	s := ""
	for i, x := range v {
		if i > 0 {
			s += ", "
		}
		s += x
	}
	return s
}

func (f *scopeFlags) scope() (inspect.Scope, error) {
	sc := inspect.Scope{Units: f.units, Names: f.names, Kinds: f.kinds, Fail: f.fail}
	var err error
	if f.since != "" {
		if sc.Since, err = time.Parse(time.RFC3339, f.since); err != nil {
			return sc, fmt.Errorf("--since: %w", err)
		}
	}
	if f.until != "" {
		if sc.Until, err = time.Parse(time.RFC3339, f.until); err != nil {
			return sc, fmt.Errorf("--until: %w", err)
		}
	}
	return sc, sc.Validate()
}

// openInspect indexes the recording at dir if it needs it, and opens it.
func openInspect(dir, model string, notice io.Writer) (*export.Source, error) {
	if err := ensureIndex(dir, notice); err != nil {
		return nil, err
	}
	return export.OpenSource(dir, model)
}

// ensureIndex builds the index of the recording at dir when it has none or
// has one of an older schema, which the other commands would stumble over. A
// recording a recorder is writing keeps the index the recorder maintains.
func ensureIndex(dir string, notice io.Writer) error {
	layout := recording.NewLayout(dir)
	ok, err := layout.Exists()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no manifest.json under %q; is this a juju-lens recording?", dir)
	}
	v, have := index.StoredSchemaVersion(layout.IndexDB())
	if have && v == index.SchemaVersion {
		return nil
	}
	if _, statErr := os.Stat(layout.IndexDB()); statErr == nil {
		if pid, err := recording.ReadPidFile(layout.PidFile()); err == nil && recording.PidAlive(pid) {
			return nil
		}
	}
	why := "has no index"
	if have {
		why = fmt.Sprintf("has an index of schema %d, and this juju-lens reads %d", v, index.SchemaVersion)
	}
	fmt.Fprintf(notice, "juju-lens: %s %s; indexing it first\n", dir, why)
	return runIndex(dir, nil)
}

// ----- inspect events

func newInspectEventsCmd() *cobra.Command {
	var (
		sf     scopeFlags
		sortBy string
		limit  int
		skip   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "events <recording>",
		Short: "List events, filtered and ranked",
		Long: `events lists the hooks and other events of a recording, one per line, with
how each ended and, for hooks recorded with --attach unit and profiled, the
CPU and memory the charm's own code used. Filter with --unit, --name, --kind,
--fail and the time bounds; rank with --sort. The list shows --limit lines
and says how to see more.`,
		Example: `  juju-lens inspect events ./rec --unit otelcol --name config-changed --sort cpu --limit 5
  juju-lens inspect events ./rec --fail problem`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := sf.scope()
			if err != nil {
				return err
			}
			src, err := openInspect(args[0], sf.model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			p := inspect.Page{Limit: limit, Skip: skip}
			list, err := inspect.ListEvents(src, inspect.EventsQuery{Scope: sc, Page: p, Sort: sortBy})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), list)
			}
			list.WriteText(cmd.OutOrStdout(), args[0], p)
			return nil
		},
	}
	sf.add(cmd)
	cmd.Flags().StringVar(&sortBy, "sort", "time", "order: time (oldest first), duration, cpu or mem (largest first)")
	cmd.Flags().IntVar(&limit, "limit", 20, "show at most this many events (0 = all)")
	cmd.Flags().IntVar(&skip, "skip", 0, "leave out this many events first")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// ----- inspect event

func newInspectEventCmd() *cobra.Command {
	var (
		model    string
		sections []string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "event <recording> <span-id>",
		Short: "Show one event in full",
		Long: `event prints one event as the export's report narrates it: why it failed,
the statuses it set, what the charm did in it (events, observers, hook tool and
Pebble calls), what it cost, its heaviest stacks, and the databag and
configuration changes. <span-id> is the one in an events line, or the id of a
span inside the hook, such as an observer in a profile.

--section keeps only some parts: ` + joinValues(inspect.EventSections) + `.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := openInspect(args[0], model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			ev, inside, err := inspect.FindEvent(src, args[0], args[1])
			if err != nil {
				return err
			}
			if inside && !asJSON {
				fmt.Fprintf(cmd.OutOrStdout(), "span %s is inside this hook:\n\n", args[1])
			}
			return inspect.WriteEvent(cmd.OutOrStdout(), src, ev, args[0], sections, asJSON)
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "the model to read (required if the recording has more than one)")
	cmd.Flags().StringSliceVar(&sections, "section", nil, "only these parts: "+joinValues(inspect.EventSections))
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the event's details as JSON")
	return cmd
}

// ----- inspect profile

func newInspectProfileCmd() *cobra.Command {
	var (
		sf     scopeFlags
		span   string
		metric string
		detail string
		order  string
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "profile <recording> [top|peek|tree|stacks] [function]",
		Short: "Where the charm's time and memory went",
		Long: `profile reads the stacks that hooks recorded with --attach unit sampled
every 10 ms, and shows them as pprof does:

  top      functions by the time in their own code, with what they called
  peek     one function: what called it and what it called (give part of its name)
  tree     the call tree, outermost function first, without the small branches
  stacks   the heaviest stacks, outermost frame first, as folded stacks

Scope it to one hook or one observer with --span (an id from events or from a
profile), or to many hooks with --unit, --name, --kind and --fail. --metric
chooses wall time, cpu time, or memory taken. --detail chooses how much code
shows: the charm's own code and what it calls directly (callee, the default),
only the charm (charm), the charm and ops (ops), or all of it.`,
		Example: `  juju-lens inspect profile ./rec --unit otelcol --name config-changed
  juju-lens inspect profile ./rec --span 7bfdb120d1995d97 --metric mem
  juju-lens inspect profile ./rec --span 7bfdb120d1995d97 peek _reconcile`,
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := inspect.ProfileQuery{Span: span, Order: order, Limit: limit}
			if len(args) > 1 {
				q.View = args[1]
			}
			if len(args) > 2 {
				q.Function = args[2]
			}
			var err error
			if q.Scope, err = sf.scope(); err != nil {
				return err
			}
			if q.Metric, err = inspect.ParseMetric(metric); err != nil {
				return err
			}
			if q.Detail, err = inspect.ParseDetail(detail); err != nil {
				return err
			}
			src, err := openInspect(args[0], sf.model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			v, err := inspect.OpenProfile(src, args[0], q)
			if err != nil {
				return err
			}
			if asJSON {
				return v.WriteJSON(cmd.OutOrStdout())
			}
			return v.WriteText(cmd.OutOrStdout())
		},
	}
	sf.add(cmd)
	cmd.Flags().StringVar(&span, "span", "", "one hook or one span in it, by span id; replaces --unit, --name and the other filters")
	cmd.Flags().StringVar(&metric, "metric", "wall", "what to weigh by: wall, cpu or mem (memory taken)")
	cmd.Flags().StringVar(&detail, "detail", "callee", "how much code to show: charm, callee, ops or all")
	cmd.Flags().StringVar(&order, "order", "self", "for top: order by self or total (what the function called counts)")
	cmd.Flags().IntVar(&limit, "limit", 0, "show at most this many rows (the tree: lines); 0 picks a size for the view")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
