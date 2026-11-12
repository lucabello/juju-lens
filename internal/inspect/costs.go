package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/query"
)

// Ways to group costs.
const (
	ByObserver = "observer" // each observer, and charm init, over the hooks that ran it
	ByEvent    = "event"    // each event name: what the charm spent in all hooks of that name
	ByUnit     = "unit"
	ByApp      = "app"
)

// CostsQuery sums the cost of charm init and observers over the hooks a scope
// picks.
type CostsQuery struct {
	Scope
	Page
	By   string // ByObserver, ByEvent, ByUnit or ByApp; "" is ByObserver
	Sort string // SortCPU (the default), SortMem or SortDuration
}

// CostRow is the cost of a group.
type CostRow struct {
	Name        string  `json:"name"`
	Kind        string  `json:"kind,omitempty"` // for observers: "observer" or "init"
	Hooks       int     `json:"hooks"`          // hooks the group ran in
	Calls       int     `json:"calls"`          // spans: an observer runs once per event it observes
	WallMS      float64 `json:"wall_ms"`
	CPUMS       float64 `json:"cpu_ms"`
	MemTaken    int64   `json:"mem_taken_bytes"`
	RSSPeak     int64   `json:"rss_peak_bytes,omitempty"` // the highest of any span
	Unprofiled  int     `json:"unprofiled_calls,omitempty"`
	ExampleSpan string  `json:"costliest_span_id,omitempty"` // the span with the most CPU (or memory, by --sort mem)
}

// CostList is a page of groups.
type CostList struct {
	Recording string    `json:"recording"`
	Model     string    `json:"model"`
	By        string    `json:"by"`
	Sort      string    `json:"sort"`
	Hooks     int       `json:"hooks"` // hooks in scope that have charm-side records
	Total     int       `json:"total"`
	Skip      int       `json:"skip,omitempty"`
	Costs     []CostRow `json:"costs"`
}

// SumCosts runs q against src.
func SumCosts(src *query.Source, q CostsQuery) (CostList, error) {
	switch q.By {
	case "":
		q.By = ByObserver
	case ByObserver, ByEvent, ByUnit, ByApp:
	default:
		return CostList{}, fmt.Errorf("unknown --by %q (want observer, event, unit or app)", q.By)
	}
	switch q.Sort {
	case "":
		q.Sort = SortCPU
	case SortCPU, SortMem, SortDuration:
	default:
		return CostList{}, fmt.Errorf("unknown --sort %q (want cpu, mem or duration)", q.Sort)
	}
	events, err := q.Scope.Events(src)
	if err != nil {
		return CostList{}, err
	}
	type acc struct {
		row    CostRow
		hooks  map[string]bool
		best   float64
		unprof int
	}
	groups := map[string]*acc{}
	list := CostList{Recording: src.Recording.Dir, Model: src.Model.Name, By: q.By, Sort: q.Sort, Costs: []CostRow{}}
	for _, ev := range events {
		costs := src.SpanCosts(ev)
		if len(costs) == 0 {
			continue
		}
		list.Hooks++
		for _, c := range costs {
			key, kind := c.Name, c.Kind
			switch q.By {
			case ByEvent:
				key, kind = ev.Summary, ""
			case ByUnit:
				key, kind = ev.Unit, ""
			case ByApp:
				key, kind = appOfUnit(ev.Unit), ""
			}
			a := groups[key]
			if a == nil {
				a = &acc{row: CostRow{Name: key, Kind: kind}, hooks: map[string]bool{}}
				groups[key] = a
			}
			a.hooks[ev.SpanID] = true
			a.row.Calls++
			a.row.WallMS += c.DurationMS
			a.row.CPUMS += c.CPUMS
			a.row.MemTaken += c.StackMemGrowthBytes
			a.row.RSSPeak = max(a.row.RSSPeak, c.RSSPeakBytes)
			if !c.Profiled {
				a.unprof++
			}
			w := c.CPUMS
			if q.Sort == SortMem {
				w = float64(c.StackMemGrowthBytes)
			} else if q.Sort == SortDuration {
				w = c.DurationMS
			}
			if w >= a.best {
				a.best, a.row.ExampleSpan = w, c.SpanID
			}
		}
	}
	var rows []CostRow
	for _, a := range groups {
		a.row.Hooks = len(a.hooks)
		a.row.Unprofiled = a.unprof
		rows = append(rows, a.row)
	}
	sort.Slice(rows, func(i, j int) bool {
		var x, y float64
		switch q.Sort {
		case SortMem:
			x, y = float64(rows[i].MemTaken), float64(rows[j].MemTaken)
		case SortDuration:
			x, y = rows[i].WallMS, rows[j].WallMS
		default:
			x, y = rows[i].CPUMS, rows[j].CPUMS
		}
		if x != y {
			return x > y
		}
		return rows[i].Name < rows[j].Name
	})
	lo, hi := q.bounds(len(rows))
	list.Total, list.Skip = len(rows), lo
	if hi > lo {
		list.Costs = rows[lo:hi]
	}
	return list, nil
}

// WriteText prints the groups as a table.
func (l CostList) WriteText(w io.Writer, rec string, p Page) {
	fmt.Fprintf(w, "cost of charm init and observers in %s, model %s, in %d hooks, by %s, sorted by %s\n\n", rec, l.Model, l.Hooks, l.By, l.Sort)
	if len(l.Costs) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CPU\tWALL\tMEMORY TAKEN\tHOOKS\tCALLS\tNAME")
		for _, r := range l.Costs {
			name := r.Name
			if r.Kind == "init" {
				name = "(charm init)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n", durMS(r.CPUMS), durMS(r.WallMS), narrative.FormatBytes(r.MemTaken), r.Hooks, r.Calls, name)
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, p.more(len(l.Costs), l.Total, "groups"))
	fmt.Fprintln(w, "CPU and memory come from the stack samples of the charm's code in each span, with what the span called; time importing Python modules before ops starts is not in them")
	if len(l.Costs) > 0 && l.Costs[0].ExampleSpan != "" {
		fmt.Fprintf(w, "next: juju-lens inspect profile %s --span %s\n", rec, l.Costs[0].ExampleSpan)
	}
}

// WriteJSON prints the groups as JSON.
func (l CostList) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(l)
}
