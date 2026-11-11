package cli

import (
	"encoding/json"
	"errors"
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

  juju-lens inspect summary <recording>           the shape of a recording: outcomes, units, slowest and costliest hooks
  juju-lens inspect events <recording>            list and rank events
  juju-lens inspect event <recording> <span-id>   one event in full
  juju-lens inspect profile <recording> [view]    where the charm's time and memory went
  juju-lens inspect diff <recording> [<recording>]  which functions cost more or less between two hooks or recordings
  juju-lens inspect memory <recording> <span-id>  memory around a hook, and the code that took it
  juju-lens inspect trace <recording> <span-id>   what a hook did along time: events, observers, calls, functions
  juju-lens inspect logs <recording>              log lines, by unit, level, text, time or event
  juju-lens inspect state <recording>             status, config, leadership and databags at a time

A recording without an index.db, or with one from an older juju-lens, is
indexed first.`,
	}
	cmd.AddCommand(newInspectEventsCmd(), newInspectEventCmd(), newInspectProfileCmd(), newInspectMemoryCmd(), newInspectTraceCmd(), newInspectLogsCmd(), newInspectStateCmd(), newInspectSummaryCmd(), newInspectDiffCmd())
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
	if f, err := os.CreateTemp(dir, ".juju-lens-write-*"); err != nil {
		msg := fmt.Sprintf("%s %s, and juju-lens can't write there to index it: %v\nrun `juju-lens index %s` as the user that owns the recording, or copy the recording somewhere you can write", dir, why, err, dir)
		if hint := recording.OwnerHint(dir); hint != "" {
			msg += "\n" + hint
		}
		return errors.New(msg)
	} else {
		f.Close()
		os.Remove(f.Name())
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

// ----- inspect memory

func newInspectMemoryCmd() *cobra.Command {
	var (
		model  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "memory <recording> <span-id>",
		Short: "Memory around a hook, and the code that took it",
		Long: `memory says what happened to memory around one hook of a recording made with
--attach unit: how the hook's process, the charm container and the workload
containers moved, the biggest jumps with the observer and the code running at
each, and the Pebble change a workload's jump followed. <span-id> is the one in
an events line, or the id of a span inside the hook.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := openInspect(args[0], model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			ev, _, err := inspect.FindEvent(src, args[0], args[1])
			if err != nil {
				return err
			}
			rep, err := inspect.MemoryOf(src, args[0], ev)
			if err != nil {
				return err
			}
			if asJSON {
				return rep.WriteJSON(cmd.OutOrStdout())
			}
			rep.WriteText(cmd.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "the model to read (required if the recording has more than one)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// ----- inspect trace

func newInspectTraceCmd() *cobra.Command {
	var (
		model   string
		attempt int
		detail  string
		all     bool
		depth   int
		minDur  time.Duration
		limit   int
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "trace <recording> <span-id>",
		Short: "What a hook did along time",
		Long: `trace lays one process of a hook, recorded with --attach unit, out along
time: charm init, the events it emitted, the observers they reached, the hook
tool and Pebble calls that took long, and under each the functions of the
charm's code that ran, from the stack samples. Each row has when it started
(from the start of the process), how long it took, the CPU and the memory it
took. A hook that was retried ran in more than one process; --attempt picks one.
Short rows are left out unless --all.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := inspect.ParseDetail(detail)
			if err != nil {
				return err
			}
			src, err := openInspect(args[0], model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			ev, _, err := inspect.FindEvent(src, args[0], args[1])
			if err != nil {
				return err
			}
			rep, err := inspect.TraceOf(src, args[0], ev, inspect.TraceQuery{Attempt: attempt, Detail: d, All: all, Depth: depth, MinDuration: minDur, Limit: limit})
			if err != nil {
				return err
			}
			if asJSON {
				return rep.WriteJSON(cmd.OutOrStdout())
			}
			rep.WriteText(cmd.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().StringVar(&model, "model", "", "the model to read (required if the recording has more than one)")
	cmd.Flags().IntVar(&attempt, "attempt", 1, "which process of the hook, from 1")
	cmd.Flags().StringVar(&detail, "detail", "callee", "how much code the function rows show: charm, callee, ops or all")
	cmd.Flags().BoolVar(&all, "all", false, "list every span, call and function, however short")
	cmd.Flags().IntVar(&depth, "depth", 0, "only this many levels below the process (0 = all)")
	cmd.Flags().DurationVar(&minDur, "min-duration", 0, "leave out rows shorter than this, such as 50ms")
	cmd.Flags().IntVar(&limit, "limit", 80, "show at most this many rows (0 = 80)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// ----- inspect logs

func newInspectLogsCmd() *cobra.Command {
	var (
		model  string
		units  []string
		levels []string
		grep   []string
		source string
		since  string
		until  string
		around string
		window time.Duration
		span   string
		width  int
		limit  int
		skip   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "logs <recording>",
		Short: "Log lines, by unit, level, text, time or event",
		Long: `logs lists the log lines of a recording: 'juju debug-log', the workload
containers' logs and the machines' journals, in time order. Filter by --unit,
--level, --grep (text, ignoring case) and --source, and by time with --since and
--until, or --around a time with --window. --span an event id lists the lines
the index tied to that event by its span id, marked *, and the same unit's lines
within --window of the hook (2s by default).`,
		Example: `  juju-lens inspect logs ./rec --unit otelcol/0 --level error,warning
  juju-lens inspect logs ./rec --span 7bfdb120d1995d97 --window 5s`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := inspect.LogsQuery{Page: inspect.Page{Limit: limit, Skip: skip}, Units: units, Levels: levels, Grep: grep,
				Source: source, Span: span, Window: window, Width: width}
			var err error
			parse := func(flag, v string) (t time.Time) {
				if v == "" || err != nil {
					return
				}
				if t, err = time.Parse(time.RFC3339, v); err != nil {
					err = fmt.Errorf("--%s: %w", flag, err)
				}
				return
			}
			q.Since, q.Until = parse("since", since), parse("until", until)
			if around != "" {
				at := parse("around", around)
				w := window
				if w == 0 {
					w = 5 * time.Second
				}
				if span == "" {
					q.Since, q.Until = at.Add(-w), at.Add(w)
				}
			}
			if err != nil {
				return err
			}
			src, err := openInspect(args[0], model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			list, err := inspect.ListLogs(src, q)
			if err != nil {
				return err
			}
			if asJSON {
				return list.WriteJSON(cmd.OutOrStdout())
			}
			list.WriteText(cmd.OutOrStdout(), args[0], q.Page)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&model, "model", "", "the model to read (required if the recording has more than one)")
	f.StringArrayVar(&units, "unit", nil, "only this unit, application, or entity such as machine-0 (repeatable)")
	f.StringSliceVar(&levels, "level", nil, "only these levels, such as error,warning (DEBUG, INFO, WARNING, ERROR, ...)")
	f.StringArrayVar(&grep, "grep", nil, "only lines containing this text, ignoring case (repeatable)")
	f.StringVar(&source, "source", "", "only this source: debug-log, k8s or journal")
	f.StringVar(&since, "since", "", "RFC3339 lower time bound")
	f.StringVar(&until, "until", "", "RFC3339 upper time bound")
	f.StringVar(&around, "around", "", "RFC3339 time: the lines within --window of it (5s by default)")
	f.DurationVar(&window, "window", 0, "with --around, how far either side; with --span, how far around the hook (2s by default)")
	f.StringVar(&span, "span", "", "the lines about this event, by span id")
	f.IntVar(&width, "width", 0, "cut each line's text at this many characters (240 by default)")
	f.IntVar(&limit, "limit", 40, "show at most this many lines (0 = all)")
	f.IntVar(&skip, "skip", 0, "leave out this many lines first")
	f.BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// ----- inspect state

func newInspectStateCmd() *cobra.Command {
	var (
		model  string
		kind   string
		at     string
		span   string
		match  []string
		width  int
		limit  int
		skip   int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "state <recording>",
		Short: "Status, config, leadership and relation data at a time",
		Long: `state prints what Juju held at a time: the status of each application and
unit, charm config, which unit leads, and relation databags. --at is a time
(RFC3339; the end of the recording by default); --span an event gives the state
when that hook had ended. --kind is ` + joinValues(inspect.StateKinds) + `, --match keeps scopes that contain
some text, such as an application name.

The status and config come from 'juju status' at the start and from the
hooks' own calls; relation data values come only from a recording made with
--attach host.`,
		Example: `  juju-lens inspect state ./rec --kind status --match otelcol
  juju-lens inspect state ./rec --kind config --span 7bfdb120d1995d97`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := inspect.StateQuery{Page: inspect.Page{Limit: limit, Skip: skip}, Kind: kind, Span: span, Match: match, Width: width}
			if at != "" {
				t, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return fmt.Errorf("--at: %w", err)
				}
				q.At = t
			}
			src, err := openInspect(args[0], model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			l, err := inspect.ReadState(src, q)
			if err != nil {
				return err
			}
			if asJSON {
				return l.WriteJSON(cmd.OutOrStdout())
			}
			l.WriteText(cmd.OutOrStdout(), args[0], q.Page)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&model, "model", "", "the model to read (required if the recording has more than one)")
	f.StringVar(&kind, "kind", "", "only this kind: "+joinValues(inspect.StateKinds))
	f.StringVar(&at, "at", "", "RFC3339 time (default: the end of the recording)")
	f.StringVar(&span, "span", "", "the state when this event's hook had ended, by span id; replaces --at")
	f.StringArrayVar(&match, "match", nil, "only scopes containing this text, ignoring case (repeatable)")
	f.IntVar(&width, "width", 0, "cut each value at this many characters (200 by default)")
	f.IntVar(&limit, "limit", 40, "show at most this many values (0 = all)")
	f.IntVar(&skip, "skip", 0, "leave out this many values first")
	f.BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// ----- inspect summary

func newInspectSummaryCmd() *cobra.Command {
	var (
		model      string
		units      []string
		since      string
		until      string
		errorsOnly bool
	)
	cmd := &cobra.Command{
		Use:   "summary <recording>",
		Short: "The shape of a recording, or of part of it",
		Long: `summary prints what the export's SUMMARY.md says, for the whole recording or
for a scope: the time range, how many hooks ended each way, events per unit,
the slowest events, and for profiled recordings the hooks that used the most
CPU and took the most memory.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sf := scopeFlags{units: units, since: since, until: until}
			sc, err := sf.scope()
			if err != nil {
				return err
			}
			src, err := openInspect(args[0], model, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			defer src.Close()
			res, err := src.Summarize(export.Options{Units: units, Since: sc.Since, Until: sc.Until, ErrorsOnly: errorsOnly})
			if err != nil {
				return err
			}
			if err := export.WriteSummary(cmd.OutOrStdout(), res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "next: juju-lens inspect events %s --fail problem\n      juju-lens inspect events %s --sort cpu\n", args[0], args[0])
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&model, "model", "", "the model to read (required if the recording has more than one)")
	f.StringArrayVar(&units, "unit", nil, "only this unit or application (repeatable)")
	f.StringVar(&since, "since", "", "RFC3339 lower time bound")
	f.StringVar(&until, "until", "", "RFC3339 upper time bound")
	f.BoolVar(&errorsOnly, "errors-only", false, "only hooks that errored, retried or had an RPC error, with one neighbour each side")
	return cmd
}

// ----- inspect diff

func newInspectDiffCmd() *cobra.Command {
	var (
		sf     scopeFlags
		spanA  string
		spanB  string
		metric string
		detail string
		order  string
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "diff <recording-a> [<recording-b>]",
		Short: "Which functions cost more or less between two hooks or recordings",
		Long: `diff compares the stack samples of two sides and lists the functions whose
cost changed the most, per hook so that sides with different numbers of hooks
compare. A side is one hook (--span-a, --span-b) or the hooks a scope picks
(--unit, --name, --kind, --fail, --since, --until, applied to both sides). With
one recording, the sides are two hooks of it (--span-a and --span-b); with two,
each side is taken from its own recording: the same hook before and after a
change to the charm, for example.

Functions are matched by name and file, not by line, which moves when a charm
changes. --metric, --detail and --order are as for 'inspect profile'.`,
		Example: `  juju-lens inspect diff ./before ./after --unit otelcol --name config-changed
  juju-lens inspect diff ./rec --span-a 7bfdb120d1995d97 --span-b 5ea2a76a77e2a8be`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sc, err := sf.scope()
			if err != nil {
				return err
			}
			q := inspect.DiffQuery{Order: order, Limit: limit}
			if q.Metric, err = inspect.ParseMetric(metric); err != nil {
				return err
			}
			if q.Detail, err = inspect.ParseDetail(detail); err != nil {
				return err
			}
			a, b := inspect.Side{Label: args[0], Scope: sc, Span: spanA}, inspect.Side{Label: args[len(args)-1], Scope: sc, Span: spanB}
			if len(args) == 1 && (spanA == "" || spanB == "") {
				return fmt.Errorf("with one recording, diff needs --span-a and --span-b: the two hooks to compare")
			}
			if a.Src, err = openInspect(args[0], sf.model, cmd.ErrOrStderr()); err != nil {
				return err
			}
			defer a.Src.Close()
			b.Src = a.Src
			if len(args) == 2 {
				if b.Src, err = openInspect(args[1], sf.model, cmd.ErrOrStderr()); err != nil {
					return err
				}
				defer b.Src.Close()
			}
			d, err := inspect.Compare(a, b, q)
			if err != nil {
				return err
			}
			if asJSON {
				return d.WriteJSON(cmd.OutOrStdout())
			}
			d.WriteText(cmd.OutOrStdout())
			return nil
		},
	}
	sf.add(cmd)
	f := cmd.Flags()
	f.StringVar(&spanA, "span-a", "", "side a is this hook or span of the first recording")
	f.StringVar(&spanB, "span-b", "", "side b is this hook or span of the second recording (or of the first, with one recording)")
	f.StringVar(&metric, "metric", "wall", "what to weigh by: wall, cpu or mem (memory taken)")
	f.StringVar(&detail, "detail", "callee", "how much code to compare: charm, callee, ops or all")
	f.StringVar(&order, "order", "self", "compare each function's self value or its total (what it called counts)")
	f.IntVar(&limit, "limit", 15, "show at most this many functions (0 = 15)")
	f.BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
