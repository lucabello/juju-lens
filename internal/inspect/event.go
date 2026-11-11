package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lucabello/juju-lens/internal/export"
	"github.com/lucabello/juju-lens/internal/narrative"
)

// EventSections are the parts of an event that WriteEvent can leave out.
var EventSections = []string{"statuses", "ops", "stacks", "costs", "databags", "config"}

// FindEvent resolves the span id a user gives to an event. An id from a
// profile, which names a span inside a hook (an observer, charm init), finds
// the hook that contains it; inside says so.
func FindEvent(src *export.Source, recDir, spanID string) (ev narrative.Event, inside bool, err error) {
	if ev, ok := src.Event(spanID); ok {
		return ev, false, nil
	}
	if ev, ok := src.SpanEvent(spanID); ok {
		return ev, true, nil
	}
	return ev, false, fmt.Errorf("no event or span with id %q in this recording; `juju-lens inspect events %s` lists events with their span ids", spanID, recDir)
}

// WriteEvent prints one event in full: as the report narrates it, or as the
// JSON of its details file. sections, when not empty, keeps only those parts
// of the event besides its heading and failure explanation.
func WriteEvent(w io.Writer, src *export.Source, ev narrative.Event, recDir string, sections []string, asJSON bool) error {
	for _, s := range sections {
		if !oneOf(s, EventSections) {
			return fmt.Errorf("unknown --section %q (want one of %s)", s, strings.Join(EventSections, ", "))
		}
	}
	d := src.Detail(ev)
	keep := func(name string) bool { return len(sections) == 0 || oneOf(name, sections) }
	if !keep("statuses") {
		d.Statuses = nil
	}
	if !keep("ops") {
		d.Ops = nil
	}
	if !keep("stacks") {
		d.TopStacks, d.TopMemStacks = nil, nil
	}
	if !keep("costs") {
		d.Costs = nil
	}
	if !keep("databags") {
		d.Databags = nil
	}
	if !keep("config") {
		d.Config = nil
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(d)
	}
	if err := export.WriteEventMarkdown(w, &d); err != nil {
		return err
	}
	if len(d.Costs) > 0 {
		fmt.Fprintln(w, "costs of charm init and each observer:")
		for _, c := range d.Costs {
			fmt.Fprintf(w, "- %s %s (span %s): %s wall, %s cpu, %s memory taken, peak rss %s\n",
				c.Kind, c.Name, c.SpanID, durMS(c.DurationMS), durMS(c.CPUMS),
				narrative.FormatBytes(c.StackMemGrowthBytes), narrative.FormatBytes(c.RSSPeakBytes))
		}
		fmt.Fprintln(w)
	}
	if len(d.Costs) > 0 || len(d.TopStacks) > 0 {
		fmt.Fprintf(w, "next: juju-lens inspect profile %s --span %s\n", recDir, ev.SpanID)
	}
	return nil
}
