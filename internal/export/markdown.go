package export

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// WriteMarkdown renders r as a narrated Markdown report — the "paste into an
// LLM chat, or read top to bottom" format. Events become headings carrying
// their failure explanation, statuses, and databag/config diffs; log lines
// between two events render as plain indented lines so the document reads as
// one story instead of fragmenting into hundreds of headings.
func WriteMarkdown(w io.Writer, r *Result) error {
	b := &strings.Builder{}

	fmt.Fprintf(b, "# juju-lens export — %s / %s\n\n", orDash(r.Recording.Controller), orDash(r.Model.Name))
	fmt.Fprintf(b, "Recording: %s\n", r.Recording.Dir)
	if !r.Recording.Started.IsZero() {
		ended := "(in progress)"
		if !r.Recording.Ended.IsZero() {
			ended = fmtTS(r.Recording.Ended)
		}
		fmt.Fprintf(b, "Captured: %s → %s\n", fmtTS(r.Recording.Started), ended)
	}
	fmt.Fprintf(b, "Scope: %s\n\n", describeFilters(r.Filters))

	fmt.Fprintf(b, "## Summary\n\n")
	fmt.Fprintf(b, "- %d events (%d errored, %d retried, %d rpc-warn)\n",
		r.Summary.TotalEvents, r.Summary.ErroredEvents, r.Summary.RetriedEvents, r.Summary.RPCWarnEvents)
	fmt.Fprintf(b, "- %d log lines\n", r.Summary.TotalLogs)
	if len(r.Summary.UnitsInScope) > 0 {
		fmt.Fprintf(b, "- units in scope: %s\n", strings.Join(r.Summary.UnitsInScope, ", "))
	}
	if !r.Summary.Start.IsZero() {
		fmt.Fprintf(b, "- time range: %s → %s\n", fmtTS(r.Summary.Start), fmtTS(r.Summary.End))
	}
	b.WriteByte('\n')

	fmt.Fprintf(b, "## Timeline\n\n")
	if len(r.Items) == 0 {
		fmt.Fprintf(b, "(no events or logs matched the given filters)\n")
	}
	for _, it := range r.Items {
		switch it.Kind {
		case "event":
			writeEventSection(b, it.Event)
		case "log":
			writeLogLine(b, it.Log)
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func writeEventSection(b *strings.Builder, ev *EventDetail) {
	heading := fmt.Sprintf("### %s — %s — %s", fmtTS(ev.TS), orDash(ev.Unit), ev.Summary)
	switch {
	case ev.Fail != "none":
		heading += fmt.Sprintf(" [%s]", strings.ToUpper(ev.Fail))
	case ev.Running:
		heading += " [RUNNING]"
	}
	fmt.Fprintf(b, "%s\n", heading)
	if ev.Context {
		fmt.Fprintf(b, "*(context: shown around a nearby failure, not itself a failure)*\n")
	}
	if ev.DurationMS > 0 {
		fmt.Fprintf(b, "duration: %dms\n", ev.DurationMS)
	}
	switch ev.Tap {
	case recording.TapMissing:
		fmt.Fprintf(b, "not instrumented: the tap recorded nothing of this hook%s\n", orReason(ev.TapReason))
	case recording.TapTruncated:
		fmt.Fprintf(b, "truncated: the tap's records stop before the process exited%s\n", orReason(ev.TapReason))
	}
	if ev.Detail != "" {
		fmt.Fprintf(b, "%s\n", ev.Detail)
	}
	if ev.FailExplanation != "" {
		fmt.Fprintf(b, "\n%s\n", ev.FailExplanation)
	}
	if len(ev.Statuses) > 0 {
		fmt.Fprintf(b, "\nstatus set by this hook:\n")
		for _, s := range ev.Statuses {
			line := fmt.Sprintf("- %s → %s", s.Who, s.Value)
			if s.Message != "" {
				line += fmt.Sprintf("  %q", s.Message)
			}
			if s.Redundant {
				line += "  (repeat)"
			}
			fmt.Fprintf(b, "%s\n", line)
		}
	}
	if len(ev.Ops) > 0 {
		writeOpsTree(b, ev.Ops)
	}
	writeStacks(b, "top stacks by wall time", ev.TopStacks, narrative.MetricWall)
	writeStacks(b, "top stacks by memory growth", ev.TopMemStacks, narrative.MetricMem)
	for _, dd := range ev.Databags {
		fmt.Fprintf(b, "\ndatabags — %s\n", dd.Relation)
		for _, e := range dd.Entries {
			fmt.Fprintf(b, "- %s\n", e.Label)
			writeValueOrDiff(b, e.Cur, e.Diff)
		}
	}
	if ev.Config != nil {
		fmt.Fprintf(b, "\nconfig\n")
		writeValueOrDiff(b, ev.Config.Cur, ev.Config.Diff)
	}
	b.WriteByte('\n')
}

// orReason renders an optional reason as a trailing clause.
func orReason(reason string) string {
	if reason == "" {
		return ""
	}
	return " (" + reason + ")"
}

// writeValueOrDiff fences diff as a ```diff block when the value actually
// changed, else cur as a plain ```json block.
func writeValueOrDiff(b *strings.Builder, cur any, diff []narrative.DiffLine) {
	if len(diff) > 0 {
		fmt.Fprintf(b, "  ```diff\n")
		for _, d := range diff {
			prefix := " "
			switch d.Op {
			case '+', '-':
				prefix = string(d.Op)
			}
			fmt.Fprintf(b, "  %s %s\n", prefix, d.Text)
		}
		fmt.Fprintf(b, "  ```\n")
		return
	}
	fmt.Fprintf(b, "  ```json\n")
	fmt.Fprintf(b, "  %s\n", narrative.MarshalIndent(cur))
	fmt.Fprintf(b, "  ```\n")
}

// writeOpsTree fences the charm-side trees of a hook as text, compact: most
// hook tool and Pebble calls fold into one line per span with counts (the
// JSON output keeps every span).
func writeOpsTree(b *strings.Builder, trees []*narrative.OpsNode) {
	title := "in the charm"
	if v := trees[0].Attrs["ops_version"]; v != "" {
		title += " (ops " + v + ")"
	}
	fmt.Fprintf(b, "\n%s\n", title)
	fmt.Fprintf(b, "  ```text\n")
	for _, l := range narrative.FormatOpsLines(narrative.OpsLines(trees, false)) {
		fmt.Fprintf(b, "  %s\n", l)
	}
	fmt.Fprintf(b, "  ```\n")
}

// writeStacks fences a hook's heaviest stacks: what each cost, the span it
// ran in, and a digest of its frames, innermost first.
func writeStacks(b *strings.Builder, title string, stacks []StackDetail, m narrative.Metric) {
	if len(stacks) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", title)
	fmt.Fprintf(b, "  ```text\n")
	for i, s := range stacks {
		v := narrative.FormatOpsDur(time.Duration(s.WallMS * float64(time.Millisecond)))
		if m == narrative.MetricMem {
			v = narrative.FormatBytes(s.MemGrowthBytes)
		}
		fmt.Fprintf(b, "  %d. %s (%.0f%%) · cpu %s · %d %s · in %s\n", i+1, v, 100*s.Share,
			narrative.FormatOpsDur(time.Duration(s.CPUMS*float64(time.Millisecond))),
			s.Samples, plural(s.Samples, "sample", "samples"), orDash(s.Span))
		for _, f := range s.digest {
			fmt.Fprintf(b, "       %s\n", f)
		}
	}
	fmt.Fprintf(b, "  ```\n")
}

func fmtTS(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("15:04:05.000")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func describeFilters(f Options) string {
	var parts []string
	if len(f.Units) > 0 {
		parts = append(parts, "units=["+strings.Join(f.Units, ", ")+"]")
	}
	if !f.Since.IsZero() {
		parts = append(parts, "since="+f.Since.Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		parts = append(parts, "until="+f.Until.Format(time.RFC3339))
	}
	if f.ErrorsOnly {
		parts = append(parts, "errors-only=true")
	}
	if len(parts) == 0 {
		return "(none — full recording)"
	}
	return strings.Join(parts, ", ")
}

func writeLogLine(b *strings.Builder, lg *LogDetail) {
	who := lg.Unit
	if who == "" {
		who = lg.Entity
	}
	src := lg.Source
	if src == "" {
		src = "juju"
	}
	if who != "" {
		src = fmt.Sprintf("%s(%s)", src, who)
	}
	level := lg.Level
	if level == "" {
		level = "-"
	}
	fmt.Fprintf(b, "    %s  [%s]  %-8s  %s\n", fmtTS(lg.TS), src, level, lg.Body)
}
