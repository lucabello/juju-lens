package inspect

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/lucabello/juju-lens/internal/export"
	"github.com/lucabello/juju-lens/internal/narrative"
)

// Sorts for ListEvents.
const (
	SortTime     = "time"     // oldest first
	SortDuration = "duration" // longest first
	SortCPU      = "cpu"      // most CPU in the charm's own code first
	SortMem      = "mem"      // most memory taken first
)

// EventsQuery asks for a page of the events a scope picks, in an order.
type EventsQuery struct {
	Scope
	Page
	Sort string // one of the Sort constants; "" is SortTime
}

// EventRow is one line of the list. Its cost fields are those of
// export.HookCost, which are empty for a hook that wasn't profiled.
type EventRow struct {
	TS         time.Time `json:"ts"`
	Unit       string    `json:"unit,omitempty"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Fail       string    `json:"fail"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	export.HookCost
	SpanID string `json:"span_id"`
}

// EventList is a page of events and how many the query matched in all.
type EventList struct {
	Recording string     `json:"recording"`
	Model     string     `json:"model"`
	Sort      string     `json:"sort"`
	Total     int        `json:"total"`
	Skip      int        `json:"skip,omitempty"`
	Events    []EventRow `json:"events"`
}

// ListEvents runs q against src.
func ListEvents(src *export.Source, q EventsQuery) (EventList, error) {
	switch q.Sort {
	case "":
		q.Sort = SortTime
	case SortTime, SortDuration, SortCPU, SortMem:
	default:
		return EventList{}, fmt.Errorf("unknown --sort %q (want time, duration, cpu or mem)", q.Sort)
	}
	events, err := q.Scope.Events(src)
	if err != nil {
		return EventList{}, err
	}
	costs := map[string]export.HookCost{}
	cost := func(ev narrative.Event) export.HookCost {
		c, ok := costs[ev.SpanID]
		if !ok {
			c = src.Cost(ev)
			costs[ev.SpanID] = c
		}
		return c
	}
	switch q.Sort {
	case SortDuration:
		sort.SliceStable(events, func(i, j int) bool { return events[i].Dur > events[j].Dur })
	case SortCPU:
		sort.SliceStable(events, func(i, j int) bool { return cost(events[i]).CPUMS > cost(events[j]).CPUMS })
	case SortMem:
		sort.SliceStable(events, func(i, j int) bool { return cost(events[i]).MemGrowth > cost(events[j]).MemGrowth })
	}
	lo, hi := q.bounds(len(events))
	list := EventList{
		Recording: src.Recording.Dir, Model: src.Model.Name, Sort: q.Sort,
		Total: len(events), Skip: lo, Events: []EventRow{},
	}
	for _, ev := range events[lo:hi] {
		list.Events = append(list.Events, EventRow{
			TS: ev.TS, Unit: ev.Unit, Kind: export.KindLabel(ev.Kind), Name: ev.Summary,
			Fail: ev.Fail.String(), DurationMS: ev.Dur.Milliseconds(),
			HookCost: cost(ev), SpanID: ev.SpanID,
		})
	}
	return list, nil
}

// WriteText prints the list as a table, then how much of it that is and a
// command to go on with. rec is the recording as the user named it.
func (l EventList) WriteText(w io.Writer, rec string, p Page) {
	fmt.Fprintf(w, "events of %s, model %s, by %s\n\n", rec, l.Model, l.Sort)
	if len(l.Events) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME (UTC)\tUNIT\tEVENT\tOUTCOME\tDURATION\tCPU\tMEMORY TAKEN\tSPAN")
		for _, r := range l.Events {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.TS.UTC().Format("15:04:05.000"), dash(r.Unit), r.Name, r.Fail, durMS(float64(r.DurationMS)),
				cpuCell(r.HookCost), memCell(r.HookCost), r.SpanID)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, p.more(len(l.Events), l.Total, "events"))
	for _, r := range l.Events {
		if r.Profiled {
			fmt.Fprintln(w, "CPU and memory taken are those of charm init and the observers; time importing Python modules before ops starts is not counted. Memory sizes use 1024-based units")
			break
		}
	}
	if len(l.Events) > 0 {
		first := l.Events[0]
		fmt.Fprintf(w, "next: juju-lens inspect event %s %s\n", rec, first.SpanID)
		for _, r := range l.Events {
			if r.Profiled {
				fmt.Fprintf(w, "      juju-lens inspect profile %s --span %s\n", rec, r.SpanID)
				break
			}
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func durMS(ms float64) string {
	if ms <= 0 {
		return "-"
	}
	return narrative.FormatOpsDur(time.Duration(ms * float64(time.Millisecond)))
}

func cpuCell(c export.HookCost) string {
	if !c.Profiled {
		return "-"
	}
	return durMS(c.CPUMS)
}

func memCell(c export.HookCost) string {
	if !c.Profiled {
		return "-"
	}
	return narrative.FormatBytes(c.MemGrowth)
}
