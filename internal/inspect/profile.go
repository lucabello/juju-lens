package inspect

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/query"
)

// Views of a profile.
const (
	ViewTop    = "top"    // functions by their own time, as pprof -top
	ViewPeek   = "peek"   // what called one function and what it called, as pprof -peek
	ViewTree   = "tree"   // the call tree, outermost first
	ViewStacks = "stacks" // the heaviest stacks, outermost frame first
)

// ProfileQuery asks for one view of the stacks sampled in some hooks. Span, if
// set, narrows it to one hook or one span in it, and Scope is ignored.
type ProfileQuery struct {
	Scope
	Span     string
	View     string // one of the View constants; "" is ViewTop
	Function string // for ViewPeek: part of a function's name
	Metric   narrative.Metric
	Detail   narrative.Detail
	Order    string // for ViewTop: "self" (the default) or "total"
	Limit    int    // rows (lines, for the tree); 0 picks a default for the view
}

// ParseMetric reads --metric.
func ParseMetric(s string) (narrative.Metric, error) {
	switch s {
	case "", "wall":
		return narrative.MetricWall, nil
	case "cpu":
		return narrative.MetricCPU, nil
	case "mem", "memory":
		return narrative.MetricMem, nil
	}
	return 0, fmt.Errorf("unknown --metric %q (want wall, cpu or mem)", s)
}

// ParseDetail reads --detail.
func ParseDetail(s string) (narrative.Detail, error) {
	switch s {
	case "", "callee":
		return narrative.DetailCharmCallee, nil
	case "charm":
		return narrative.DetailCharm, nil
	case "ops":
		return narrative.DetailOps, nil
	case "all":
		return narrative.DetailAll, nil
	}
	return 0, fmt.Errorf("unknown --detail %q (want charm, callee, ops or all)", s)
}

func detailFlag(d narrative.Detail) string {
	switch d {
	case narrative.DetailCharm:
		return "charm"
	case narrative.DetailOps:
		return "ops"
	case narrative.DetailAll:
		return "all"
	}
	return "callee"
}

func metricFlag(m narrative.Metric) string {
	switch m {
	case narrative.MetricCPU:
		return "cpu"
	case narrative.MetricMem:
		return "mem"
	}
	return "wall"
}

// ProfileView is a profile ready to print, as text or JSON.
type ProfileView struct {
	q       ProfileQuery
	rec     string
	src     *query.Source
	p       *narrative.Profile
	scope   string
	total   narrative.Value
	events  int
	scopeFl string // the flags that gave this scope, for the next command
}

// OpenProfile gathers the stacks q's scope covers and the view's inputs. It
// fails, with what to do, when there are none.
func OpenProfile(src *query.Source, rec string, q ProfileQuery) (*ProfileView, error) {
	switch q.View {
	case "":
		q.View = ViewTop
	case ViewTop, ViewPeek, ViewTree, ViewStacks:
	default:
		return nil, fmt.Errorf("unknown view %q (want top, peek, tree or stacks)", q.View)
	}
	if q.Order != "" && q.Order != "self" && q.Order != "total" {
		return nil, fmt.Errorf("unknown --order %q (want self or total)", q.Order)
	}
	if q.View == ViewPeek && q.Function == "" {
		return nil, fmt.Errorf("peek needs a function: part of its name, as printed by `top`")
	}
	v := &ProfileView{q: q, rec: rec, src: src}
	var p *narrative.Profile
	if q.Span != "" {
		var err error
		p, _, v.scope, err = src.SpanProfile(q.Span)
		if err != nil {
			return nil, err
		}
		v.events = 1
		v.scopeFl = " --span " + q.Span
	} else {
		events, err := q.Scope.Events(src)
		if err != nil {
			return nil, err
		}
		if p, err = src.Profile(events); err != nil {
			return nil, err
		}
		v.events = len(events)
		v.scope = fmt.Sprintf("%d %s", len(events), plural(len(events), "event", "events"))
		v.scopeFl = q.Scope.flags()
		if d := q.Scope.describe(); d != "" {
			v.scope += " (" + d + ")"
		}
	}
	if len(p.Stacks) == 0 {
		return nil, fmt.Errorf("no stack samples for %s: stacks are sampled in hooks recorded with --attach unit, unless the recording ran with --profile=false", v.scope)
	}
	v.total = p.Total()
	v.p = p.AtDetail(q.Detail)
	return v, nil
}

func (sc Scope) flags() string {
	var b strings.Builder
	add := func(flag string, vals []string) {
		for _, x := range vals {
			fmt.Fprintf(&b, " --%s %s", flag, x)
		}
	}
	add("unit", sc.Units)
	add("name", sc.Names)
	add("fail", sc.Fail)
	if !sc.Since.IsZero() {
		fmt.Fprintf(&b, " --since %s", sc.Since.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if !sc.Until.IsZero() {
		fmt.Fprintf(&b, " --until %s", sc.Until.UTC().Format("2006-01-02T15:04:05Z"))
	}
	return b.String()
}

func (sc Scope) describe() string {
	var parts []string
	if len(sc.Units) > 0 {
		parts = append(parts, "units "+strings.Join(sc.Units, ", "))
	}
	if len(sc.Names) > 0 {
		parts = append(parts, "events matching "+strings.Join(sc.Names, ", "))
	}
	if len(sc.Fail) > 0 {
		parts = append(parts, "outcomes "+strings.Join(sc.Fail, ", "))
	}
	return strings.Join(parts, "; ")
}

func (v *ProfileView) m() narrative.Metric { return v.q.Metric }

func (v *ProfileView) frameName(f int) string {
	if f < 0 || f >= len(v.p.Frames) {
		return "(all)"
	}
	fr := v.p.Frames[f]
	name := narrative.FrameName(fr)
	_, loc := narrative.FrameLabel(fr)
	if loc == "" || strings.HasPrefix(name, "importing ") {
		return name
	}
	return name + " (" + loc + ")"
}

func (v *ProfileView) header(w io.Writer) {
	t := v.total
	fmt.Fprintf(w, "profile of %s in %s, model %s\n", v.scope, v.rec, v.src.Model.Name)
	fmt.Fprintf(w, "%d samples, %s wall, %s cpu, %s memory taken\n",
		t.Samples, narrative.MetricWall.Format(float64(t.WallNs)), narrative.MetricCPU.Format(float64(t.CPUNs)),
		narrative.FormatBytes(t.MemBytes))
	fmt.Fprintf(w, "showing %s, --detail %s\n\n", metricLabel(v.m()), detailFlag(v.q.Detail))
}

func (v *ProfileView) footer(w io.Writer) {
	if v.m() == narrative.MetricMem {
		fmt.Fprintln(w, "memory taken is growth of resident memory between samples, so it shows where memory was taken, not where it was held")
	}
}

func (v *ProfileView) cmd(view, extra string) string {
	s := fmt.Sprintf("juju-lens inspect profile %s%s %s", v.rec, v.scopeFl, view)
	if extra != "" {
		s += " " + extra
	}
	if v.q.Metric != narrative.MetricWall {
		s += " --metric " + metricFlag(v.q.Metric)
	}
	if v.q.Detail != narrative.DetailCharmCallee {
		s += " --detail " + detailFlag(v.q.Detail)
	}
	return s
}

// ----- top

// FuncRow is a function in the top view.
type FuncRow struct {
	Function string  `json:"function"`
	File     string  `json:"file,omitempty"`
	Line     int     `json:"line,omitempty"`
	Self     float64 `json:"self"`
	SelfPct  float64 `json:"self_pct"`
	Total    float64 `json:"total"`
	TotalPct float64 `json:"total_pct"`
	Class    string  `json:"class"` // charm, ops or other
}

func (v *ProfileView) classOf(f int) string {
	switch v.p.Class(f) {
	case narrative.ClassCharm:
		return "charm"
	case narrative.ClassOps:
		return "ops"
	}
	return "other"
}

func (v *ProfileView) top() []FuncRow {
	t := v.total.Get(v.m())
	pct := func(x float64) float64 {
		if t <= 0 {
			return 0
		}
		return x / t * 100
	}
	var rows []FuncRow
	for _, fs := range v.p.Functions(v.m()) {
		fr := v.p.Frames[fs.Frame]
		rows = append(rows, FuncRow{
			Function: narrative.FrameName(fr), File: fr.File, Line: fr.Line,
			Self: fs.Self.Get(v.m()), SelfPct: pct(fs.Self.Get(v.m())),
			Total: fs.Total.Get(v.m()), TotalPct: pct(fs.Total.Get(v.m())),
			Class: v.classOf(fs.Frame),
		})
	}
	if v.q.Order == "total" {
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Total > rows[j].Total })
	}
	return rows
}

func (v *ProfileView) limit(def int) int {
	if v.q.Limit > 0 {
		return v.q.Limit
	}
	return def
}

// ----- peek

// matchFrames lists the frames of the profile whose function name contains
// part, an exact match winning over a partial one.
func (v *ProfileView) matchFrames(part string) []int {
	var exact, partial []int
	lp := strings.ToLower(part)
	for i, fr := range v.p.Frames {
		name := narrative.FrameName(fr)
		switch {
		case fr.Func == part || name == part:
			exact = append(exact, i)
		case strings.Contains(strings.ToLower(name), lp):
			partial = append(partial, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

// EdgeRow is a caller or callee in the peek view.
type EdgeRow struct {
	Function string  `json:"function"`
	Value    float64 `json:"value"`
	Pct      float64 `json:"pct"` // of the function's own total
}

// Peek is the peek view of one function.
type Peek struct {
	Function string    `json:"function"`
	Self     float64   `json:"self"`
	Total    float64   `json:"total"`
	Callers  []EdgeRow `json:"callers"`
	Callees  []EdgeRow `json:"callees"`
}

func (v *ProfileView) peek() (*Peek, error) {
	frames := v.matchFrames(v.q.Function)
	switch {
	case len(frames) == 0:
		return nil, fmt.Errorf("no function matching %q in this profile; `%s` lists them", v.q.Function, v.cmd("top", ""))
	case len(frames) > 1:
		var names []string
		for i, f := range frames {
			if i == 8 {
				names = append(names, fmt.Sprintf("… %d more", len(frames)-8))
				break
			}
			names = append(names, "  "+v.frameName(f))
		}
		return nil, fmt.Errorf("%q matches %d functions; use more of the name:\n%s", v.q.Function, len(frames), strings.Join(names, "\n"))
	}
	f := frames[0]
	self, callers, callees := v.p.Peek(f, v.m())
	total := self.Total.Get(v.m())
	row := func(e narrative.Edge, own string) EdgeRow {
		name := own
		if e.Frame >= 0 {
			name = v.frameName(e.Frame)
		}
		pc := 0.0
		if total > 0 {
			pc = e.Value.Get(v.m()) / total * 100
		}
		return EdgeRow{Function: name, Value: e.Value.Get(v.m()), Pct: pc}
	}
	pk := &Peek{Function: v.frameName(f), Self: self.Self.Get(v.m()), Total: total}
	for _, e := range callers {
		pk.Callers = append(pk.Callers, row(e, "(outermost: nothing called it)"))
	}
	for _, e := range callees {
		pk.Callees = append(pk.Callees, row(e, "(its own code)"))
	}
	return pk, nil
}

// ----- tree

// TreeNode is a function in the call tree.
type TreeNode struct {
	Function string      `json:"function"`
	Self     float64     `json:"self"`
	Total    float64     `json:"total"`
	Pct      float64     `json:"pct"`
	Children []*TreeNode `json:"children,omitempty"`
}

// treeMinPct is the share of the profile below which the tree leaves a
// function out, and says how many it left out.
const treeMinPct = 1.0

func (v *ProfileView) tree(maxLines int) (*TreeNode, int) {
	root := v.p.Tree()
	root.Sort(v.m())
	t := root.Total.Get(v.m())
	pct := func(x float64) float64 {
		if t <= 0 {
			return 0
		}
		return x / t * 100
	}
	lines, hidden := 0, 0
	var conv func(n *narrative.Node) *TreeNode
	var count func(n *narrative.Node) int
	count = func(n *narrative.Node) int {
		c := 1
		for _, ch := range n.Children {
			c += count(ch)
		}
		return c
	}
	conv = func(n *narrative.Node) *TreeNode {
		lines++
		out := &TreeNode{
			Function: v.frameName(n.Frame), Self: n.Self.Get(v.m()),
			Total: n.Total.Get(v.m()), Pct: pct(n.Total.Get(v.m())),
		}
		for _, ch := range n.Children {
			if lines >= maxLines || pct(ch.Total.Get(v.m())) < treeMinPct {
				hidden += count(ch)
				continue
			}
			out.Children = append(out.Children, conv(ch))
		}
		return out
	}
	// The root holds no function: start at its children.
	top := &TreeNode{Function: "(all)", Total: root.Total.Get(v.m()), Pct: 100}
	for _, ch := range root.Children {
		if lines >= maxLines || pct(ch.Total.Get(v.m())) < treeMinPct {
			hidden += count(ch)
			continue
		}
		top.Children = append(top.Children, conv(ch))
	}
	return top, hidden
}

// ----- stacks

// StackRow is a stack in the stacks view: frames outermost first, as in
// folded stacks.
type StackRow struct {
	Span    string   `json:"span"`
	SpanID  string   `json:"span_id"`
	Value   float64  `json:"value"`
	Pct     float64  `json:"pct"`
	Samples int      `json:"samples"`
	Frames  []string `json:"frames"`
}

func (v *ProfileView) stacks(n int) []StackRow {
	t := v.total.Get(v.m())
	var rows []StackRow
	for _, s := range v.p.TopStacks(v.m(), n) {
		var val narrative.Value
		val.Samples, val.WallNs, val.CPUNs, val.MemBytes = s.Samples, s.WallNs, s.CPUNs, s.MemBytes
		r := StackRow{
			Span: v.src.SpanLabel(s.SpanID), SpanID: s.SpanID, Value: val.Get(v.m()), Samples: s.Samples,
		}
		if t > 0 {
			r.Pct = r.Value / t * 100
		}
		for i := len(s.Frames) - 1; i >= 0; i-- {
			r.Frames = append(r.Frames, v.frameName(s.Frames[i]))
		}
		rows = append(rows, r)
	}
	return rows
}

// ----- output

// WriteText prints the view.
func (v *ProfileView) WriteText(w io.Writer) error {
	v.header(w)
	m := v.m()
	switch v.q.View {
	case ViewTop:
		rows := v.top()
		n := min(v.limit(15), len(rows))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SELF\tSELF%\tTOTAL\tTOTAL%\tFUNCTION")
		for _, r := range rows[:n] {
			fmt.Fprintf(tw, "%s\t%.0f%%\t%s\t%.0f%%\t%s\n", m.Format(r.Self), r.SelfPct, m.Format(r.Total), r.TotalPct, nameOf(r))
		}
		tw.Flush()
		order := "self"
		if v.q.Order == "total" {
			order = "total"
		}
		fmt.Fprintf(w, "\nshowing %d of %d functions, by %s; self is the value in its own code, total includes what it called (--order self|total)\n", n, len(rows), order)
		if len(rows) > 0 {
			fmt.Fprintf(w, "next: %s\n", v.cmd("peek", "'"+rows[0].Function+"'"))
			fmt.Fprintf(w, "      %s\n", v.cmd("tree", ""))
		}
	case ViewPeek:
		pk, err := v.peek()
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\nself %s, total %s\n", pk.Function, m.Format(pk.Self), m.Format(pk.Total))
		write := func(title string, rows []EdgeRow) {
			fmt.Fprintf(w, "\n%s:\n", title)
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			for _, r := range rows[:min(v.limit(10), len(rows))] {
				fmt.Fprintf(tw, "  %s\t%.0f%%\t%s\n", m.Format(r.Value), r.Pct, r.Function)
			}
			tw.Flush()
			if len(rows) > v.limit(10) {
				fmt.Fprintf(w, "  … %d more (--limit)\n", len(rows)-v.limit(10))
			}
		}
		write("called by", pk.Callers)
		write("calls", pk.Callees)
		if len(pk.Callees) > 0 && pk.Callees[0].Value > 0 && !strings.HasPrefix(pk.Callees[0].Function, "(") {
			fmt.Fprintf(w, "\nnext: %s\n", v.cmd("peek", "'"+shortFn(pk.Callees[0].Function)+"'"))
		}
	case ViewTree:
		root, hidden := v.tree(v.limit(40))
		var pr func(n *TreeNode, depth int)
		pr = func(n *TreeNode, depth int) {
			fmt.Fprintf(w, "%s%3.0f%% %s  %s\n", strings.Repeat("  ", depth), n.Pct, m.Format(n.Total), n.Function)
			for _, c := range n.Children {
				pr(c, depth+1)
			}
		}
		for _, c := range root.Children {
			pr(c, 0)
		}
		if hidden > 0 {
			fmt.Fprintf(w, "\n%d functions left out: under %.0f%% of the total, or past --limit\n", hidden, treeMinPct)
		}
		fmt.Fprintf(w, "next: %s\n", v.cmd("peek", "'<function>'"))
	case ViewStacks:
		rows := v.stacks(v.limit(8))
		for _, r := range rows {
			fmt.Fprintf(w, "%s  %.0f%%  [%s %s]\n  %s\n", m.Format(r.Value), r.Pct, r.Span, r.SpanID, strings.Join(r.Frames, ";"))
		}
		fmt.Fprintf(w, "\nstacks run outermost frame first, joined with ';'; each is in the span in brackets, whose id --span takes\n")
	}
	v.footer(w)
	return nil
}

func nameOf(r FuncRow) string {
	if r.File == "" || strings.HasPrefix(r.Function, "importing ") {
		return r.Function
	}
	return fmt.Sprintf("%s (%s:%d)", r.Function, narrative.ShortPath(r.File), r.Line)
}

func shortFn(name string) string {
	if i := strings.Index(name, " ("); i >= 0 {
		return name[:i]
	}
	return name
}

// ProfileJSON is the JSON form of a view.
type ProfileJSON struct {
	Recording string     `json:"recording"`
	Model     string     `json:"model"`
	Scope     string     `json:"scope"`
	View      string     `json:"view"`
	Metric    string     `json:"metric"` // wall and cpu in nanoseconds, mem in bytes
	Detail    string     `json:"detail"`
	Samples   int        `json:"samples"`
	Functions []FuncRow  `json:"functions,omitempty"`
	Peek      *Peek      `json:"peek,omitempty"`
	Tree      *TreeNode  `json:"tree,omitempty"`
	Hidden    int        `json:"tree_hidden,omitempty"`
	Stacks    []StackRow `json:"stacks,omitempty"`
}

// WriteJSON prints the view as JSON.
func (v *ProfileView) WriteJSON(w io.Writer) error {
	out := ProfileJSON{
		Recording: v.rec, Model: v.src.Model.Name, Scope: v.scope, View: v.q.View,
		Metric: metricFlag(v.q.Metric), Detail: detailFlag(v.q.Detail), Samples: v.total.Samples,
	}
	switch v.q.View {
	case ViewTop:
		rows := v.top()
		out.Functions = rows[:min(v.limit(15), len(rows))]
	case ViewPeek:
		pk, err := v.peek()
		if err != nil {
			return err
		}
		out.Peek = pk
	case ViewTree:
		out.Tree, out.Hidden = v.tree(v.limit(40))
	case ViewStacks:
		out.Stacks = v.stacks(v.limit(8))
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// metricLabel names a metric as the output and the flags do.
func metricLabel(m narrative.Metric) string {
	switch m {
	case narrative.MetricCPU:
		return "cpu time"
	case narrative.MetricMem:
		return "memory taken"
	}
	return "wall time"
}
