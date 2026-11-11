package inspect

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/query"
)

// WriteFolded prints the stacks the scope's hooks were sampled in as folded
// stacks, the format flame graph tools and grep read: one line for each
// distinct stack, "unit;event;span;frame;...;frame value", outermost frame
// first. The span is "charm __init__", an observer, an ops event, or
// "process" for what ran before ops started. The value is microseconds for the
// wall and cpu metrics, and bytes for memory taken. Stacks of the same unit,
// event name, span and call path in different hooks are added up, and the
// lines run from the largest value down. Nothing but the lines is printed.
func WriteFolded(w io.Writer, src *query.Source, q ProfileQuery) error {
	type hook struct {
		prefix string
		p      *narrative.Profile
	}
	var hooks []hook
	if q.Span != "" {
		p, ev, _, err := src.SpanProfile(q.Span)
		if err != nil {
			return err
		}
		hooks = append(hooks, hook{foldedName(dash(ev.Unit)) + ";" + foldedName(ev.Summary), p})
	} else {
		events, err := q.Scope.Events(src)
		if err != nil {
			return err
		}
		for _, ev := range events {
			if len(ev.OpsSpans) == 0 {
				continue
			}
			p, err := src.Profile([]narrative.Event{ev})
			if err != nil {
				return err
			}
			hooks = append(hooks, hook{foldedName(dash(ev.Unit)) + ";" + foldedName(ev.Summary), p})
		}
	}
	sums := map[string]narrative.Value{}
	for _, h := range hooks {
		p := h.p.AtDetail(q.Detail)
		for _, st := range p.Stacks {
			if len(st.Frames) == 0 {
				continue
			}
			frames := make([]string, 0, len(st.Frames))
			for i := len(st.Frames) - 1; i >= 0; i-- {
				fr := p.Frames[st.Frames[i]]
				name := narrative.FrameName(fr)
				if _, loc := narrative.FrameLabel(fr); loc != "" && !strings.HasPrefix(name, "importing ") {
					name += " (" + loc + ")"
				}
				frames = append(frames, foldedName(name))
			}
			span := src.SpanLabel(st.SpanID)
			if span == "" {
				span = "process"
			}
			key := h.prefix + ";" + foldedName(span) + ";" + strings.Join(frames, ";")
			v := sums[key]
			v.Samples += st.Samples
			v.WallNs += st.WallNs
			v.CPUNs += st.CPUNs
			v.MemBytes += st.MemBytes
			sums[key] = v
		}
	}
	if len(sums) == 0 {
		return fmt.Errorf("no stack samples in scope: stacks are sampled in hooks recorded with --attach unit, unless the recording ran with --profile=false")
	}
	type line struct {
		text  string
		value int64
	}
	var lines []line
	for k, v := range sums {
		n := int64(v.Get(q.Metric))
		if q.Metric != narrative.MetricMem {
			n /= 1000
		}
		if n > 0 {
			lines = append(lines, line{k, n})
		}
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].value != lines[j].value {
			return lines[i].value > lines[j].value
		}
		return lines[i].text < lines[j].text
	})
	if q.Limit > 0 && len(lines) > q.Limit {
		lines = lines[:q.Limit]
	}
	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "%s %d\n", l.text, l.value); err != nil {
			return err
		}
	}
	return nil
}

// foldedName makes a name safe for a folded stack line: no semicolons, which
// separate frames, and no line breaks.
func foldedName(s string) string {
	return strings.NewReplacer(";", ",", "\n", " ", "\r", " ").Replace(s)
}
