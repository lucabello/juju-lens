package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/query"
)

// Side is one half of a diff: the hooks of a recording that a scope picks,
// or one hook.
type Side struct {
	Src   *query.Source
	Label string // the recording as the user named it
	Scope Scope
	Span  string // one hook, by span id; replaces Scope
}

// DiffQuery compares the profiles of two sides.
type DiffQuery struct {
	Metric narrative.Metric
	Detail narrative.Detail
	Order  string // "self" (the default) or "total": which of a function's values is compared
	Limit  int    // functions to show; 0 picks 15
}

// DiffRow is a function whose cost differs between the sides, per hook.
type DiffRow struct {
	Function string  `json:"function"`
	File     string  `json:"file,omitempty"`
	A        float64 `json:"a"`
	B        float64 `json:"b"`
	Delta    float64 `json:"delta"` // b - a
	Class    string  `json:"class"`
}

// SideSummary is what one side adds up to, per hook.
type SideSummary struct {
	Label   string  `json:"label"`
	Scope   string  `json:"scope"`
	Hooks   int     `json:"profiled_hooks"`
	Samples int     `json:"samples"`
	WallNs  float64 `json:"wall_ns_per_hook"`
	CPUNs   float64 `json:"cpu_ns_per_hook"`
	MemB    float64 `json:"mem_taken_bytes_per_hook"`
}

// Diff is the comparison.
type Diff struct {
	Metric    string      `json:"metric"` // wall and cpu in nanoseconds, mem in bytes, per hook
	Detail    string      `json:"detail"`
	Order     string      `json:"order"`
	A         SideSummary `json:"a"`
	B         SideSummary `json:"b"`
	Functions []DiffRow   `json:"functions"`
	Total     int         `json:"changed_functions"`

	m narrative.Metric
}

type sideProfile struct {
	sum  SideSummary
	fn   map[string]*DiffRow // by function and file, without the line, which moves between revisions
	self map[string]float64
	tot  map[string]float64
	cls  map[string]string
}

func (s Side) profile(q DiffQuery) (*sideProfile, error) {
	var (
		p      *narrative.Profile
		events []narrative.Event
		label  string
		err    error
	)
	if s.Span != "" {
		var ev narrative.Event
		p, ev, label, err = s.Src.SpanProfile(s.Span)
		if err != nil {
			return nil, err
		}
		events = []narrative.Event{ev}
	} else {
		if events, err = s.Scope.Events(s.Src); err != nil {
			return nil, err
		}
		if p, err = s.Src.Profile(events); err != nil {
			return nil, err
		}
		label = fmt.Sprintf("%d events", len(events))
		if d := s.Scope.describe(); d != "" {
			label += " (" + d + ")"
		}
	}
	hooks := 0
	for _, ev := range events {
		if s.Src.Cost(ev).Profiled {
			hooks++
		}
	}
	if len(p.Stacks) == 0 || hooks == 0 {
		return nil, fmt.Errorf("no stack samples for %s in %s: stacks are sampled in hooks recorded with --attach unit, unless the recording ran with --profile=false", label, s.Label)
	}
	n := float64(hooks)
	tot := p.Total()
	out := &sideProfile{
		sum: SideSummary{Label: s.Label, Scope: label, Hooks: hooks, Samples: tot.Samples,
			WallNs: float64(tot.WallNs) / n, CPUNs: float64(tot.CPUNs) / n, MemB: float64(tot.MemBytes) / n},
		self: map[string]float64{}, tot: map[string]float64{}, cls: map[string]string{}, fn: map[string]*DiffRow{},
	}
	v := p.AtDetail(q.Detail)
	for _, fs := range v.Functions(q.Metric) {
		fr := v.Frames[fs.Frame]
		key := narrative.FrameName(fr) + "\x00" + narrative.ShortPath(fr.File)
		out.self[key] += fs.Self.Get(q.Metric) / n
		out.tot[key] += fs.Total.Get(q.Metric) / n
		switch v.Class(fs.Frame) {
		case narrative.ClassCharm:
			out.cls[key] = "charm"
		case narrative.ClassOps:
			out.cls[key] = "ops"
		default:
			out.cls[key] = "other"
		}
		out.fn[key] = &DiffRow{Function: narrative.FrameName(fr), File: narrative.ShortPath(fr.File)}
	}
	return out, nil
}

// Compare diffs the profiles of a and b, per hook, so that sides with
// different numbers of hooks compare.
func Compare(a, b Side, q DiffQuery) (*Diff, error) {
	if q.Order != "" && q.Order != "self" && q.Order != "total" {
		return nil, fmt.Errorf("unknown --order %q (want self or total)", q.Order)
	}
	if q.Order == "" {
		q.Order = "self"
	}
	pa, err := a.profile(q)
	if err != nil {
		return nil, err
	}
	pb, err := b.profile(q)
	if err != nil {
		return nil, err
	}
	pick := func(p *sideProfile) map[string]float64 {
		if q.Order == "total" {
			return p.tot
		}
		return p.self
	}
	va, vb := pick(pa), pick(pb)
	keys := map[string]bool{}
	for k := range va {
		keys[k] = true
	}
	for k := range vb {
		keys[k] = true
	}
	d := &Diff{Metric: metricFlag(q.Metric), Detail: detailFlag(q.Detail), Order: q.Order, A: pa.sum, B: pb.sum, m: q.Metric, Functions: []DiffRow{}}
	var rows []DiffRow
	for k := range keys {
		r := DiffRow{A: va[k], B: vb[k]}
		src := pa.fn[k]
		cls := pa.cls[k]
		if src == nil {
			src, cls = pb.fn[k], pb.cls[k]
		}
		r.Function, r.File, r.Class = src.Function, src.File, cls
		r.Delta = r.B - r.A
		if math.Abs(r.Delta) > 0 {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if x, y := math.Abs(rows[i].Delta), math.Abs(rows[j].Delta); x != y {
			return x > y
		}
		return rows[i].Function < rows[j].Function
	})
	d.Total = len(rows)
	limit := q.Limit
	if limit == 0 {
		limit = 15
	}
	d.Functions = rows[:min(limit, len(rows))]
	return d, nil
}

// WriteText prints the comparison.
func (d *Diff) WriteText(w io.Writer) {
	m := d.m
	fmt.Fprintf(w, "profile diff, per hook, by %s, --detail %s, functions by %s\n", metricLabel(m), d.Detail, d.Order)
	fmt.Fprintf(w, "  a: %s, %s: %d profiled hooks, %s wall, %s cpu, %s memory taken each\n", d.A.Label, d.A.Scope, d.A.Hooks,
		narrative.MetricWall.Format(d.A.WallNs), narrative.MetricCPU.Format(d.A.CPUNs), narrative.FormatBytes(int64(d.A.MemB)))
	fmt.Fprintf(w, "  b: %s, %s: %d profiled hooks, %s wall, %s cpu, %s memory taken each\n\n", d.B.Label, d.B.Scope, d.B.Hooks,
		narrative.MetricWall.Format(d.B.WallNs), narrative.MetricCPU.Format(d.B.CPUNs), narrative.FormatBytes(int64(d.B.MemB)))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "A\tB\tCHANGE\tFUNCTION")
	for _, r := range d.Functions {
		sign := "+"
		if r.Delta < 0 {
			sign = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s%s\t%s\n", m.Format(r.A), m.Format(r.B), sign, m.Format(math.Abs(r.Delta)), nameOfDiff(r))
	}
	tw.Flush()
	fmt.Fprintf(w, "\nshowing %d of %d functions that differ, the biggest change first (--limit)\n", len(d.Functions), d.Total)
	if m == narrative.MetricMem {
		fmt.Fprintln(w, "memory taken is growth of resident memory between samples, so it shows where memory was taken, not where it was held")
	}
}

func nameOfDiff(r DiffRow) string {
	if r.File == "" || strings.HasPrefix(r.Function, "importing ") {
		return r.Function
	}
	return r.Function + " (" + r.File + ")"
}

// WriteJSON prints the comparison as JSON.
func (d *Diff) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(d)
}
