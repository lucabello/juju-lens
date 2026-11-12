package query

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// WriteEvent renders one event as text: its heading,
// failure explanation, statuses, charm-side tree, top stacks, and databag and
// config changes.
func WriteEvent(w io.Writer, ev *EventDetail) error {
	b := &strings.Builder{}
	writeEventSection(b, ev)
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
	if ev.DurationMS > 0 {
		fmt.Fprintf(b, "duration: %dms\n", ev.DurationMS)
	}
	switch ev.Tap {
	case recording.TapMissing:
		fmt.Fprintf(b, "not instrumented: the tap recorded nothing of this hook%s\n", orReason(ev.TapReason))
	case recording.TapTruncated:
		fmt.Fprintf(b, "truncated: the tap's records of this hook are incomplete%s\n", orReason(ev.TapReason))
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
