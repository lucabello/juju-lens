package export

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// foldedMetric is one of the three files of folded stacks.
type foldedMetric struct {
	file   string
	metric narrative.Metric
	scale  float64 // value per unit of the metric: nanoseconds to microseconds
}

var foldedMetrics = []foldedMetric{
	{"wall.folded", narrative.MetricWall, 1e-3},
	{"cpu.folded", narrative.MetricCPU, 1e-3},
	{"mem.folded", narrative.MetricMem, 1},
}

// writeFolded writes the stacks the hooks of r were sampled in as folded
// stacks, the format flame graph tools and `grep` read: one line for each
// distinct stack, `unit;event;span;frame;…;frame value`, outermost frame
// first. Stacks of the same unit, event name, span and call path in different
// hooks are added up. It writes nothing when no hook was profiled.
func writeFolded(dir string, r *Result) error {
	type key struct{ unit, event, span, path string }
	sums := map[key]narrative.Value{}
	spanLabel := map[string]string{}
	for _, it := range r.Items {
		if it.Kind != "event" || len(it.Event.profs) == 0 {
			continue
		}
		ev := it.Event
		p := narrative.NewProfile(ev.profs, nil)
		for _, st := range p.Stacks {
			if len(st.Frames) == 0 {
				continue
			}
			var frames []string
			for i := len(st.Frames) - 1; i >= 0; i-- {
				fn, loc := p.FrameLabel(st.Frames[i])
				name := narrative.FrameName(p.Frames[st.Frames[i]])
				if loc != "" && fn == name {
					name += " (" + loc + ")"
				}
				frames = append(frames, foldedName(name))
			}
			k := key{foldedName(orDash(ev.Unit)), foldedName(ev.Summary), foldedName(spanName(r, ev, st.SpanID, spanLabel)), strings.Join(frames, ";")}
			v := sums[k]
			v.Samples += st.Samples
			v.WallNs += st.WallNs
			v.CPUNs += st.CPUNs
			v.MemBytes += st.MemBytes
			sums[k] = v
		}
	}
	if len(sums) == 0 {
		return nil
	}
	out := filepath.Join(dir, "profiles")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", out, err)
	}
	for _, fm := range foldedMetrics {
		type line struct {
			text  string
			value int64
		}
		var lines []line
		for k, v := range sums {
			n := int64(v.Get(fm.metric) * fm.scale)
			if n > 0 {
				lines = append(lines, line{k.unit + ";" + k.event + ";" + k.span + ";" + k.path, n})
			}
		}
		sort.Slice(lines, func(i, j int) bool {
			if lines[i].value != lines[j].value {
				return lines[i].value > lines[j].value
			}
			return lines[i].text < lines[j].text
		})
		var b strings.Builder
		for _, l := range lines {
			fmt.Fprintf(&b, "%s %d\n", l.text, l.value)
		}
		if err := os.WriteFile(filepath.Join(out, fm.file), []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// foldedName makes a frame or span name safe for a folded stack line: no
// semicolons, which separate frames, and no newlines.
func foldedName(s string) string {
	return strings.NewReplacer(";", ",", "\n", " ", "\r", " ").Replace(s)
}

// spanName is how the stacks of a span are labelled, as the trace labels it:
// "charm __init__", an observer's class and method, an event's name, or
// "process" for what ran before ops started.
func spanName(r *Result, ev *EventDetail, spanID string, cache map[string]string) string {
	if n, ok := cache[spanID]; ok {
		return n
	}
	var walk func(n *narrative.OpsNode)
	walk = func(n *narrative.OpsNode) {
		label := ""
		switch n.Kind {
		case recording.OpsInit:
			label = "charm __init__"
		case recording.OpsObserver:
			label = narrative.ShortObserver(n.Name)
		case recording.OpsBacklog:
			label = "deferred events"
		case recording.OpsRoot:
			label = "process"
		default:
			label = n.Name
		}
		cache[n.SpanID] = label
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, t := range ev.Ops {
		walk(t)
	}
	if n, ok := cache[spanID]; ok {
		return n
	}
	return "process"
}
