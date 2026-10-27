package narrative

import (
	"path"
	"sort"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// This file lays one hook process's work out along time, like a trace: the
// spans of its ops tree (init, events, observers, slow hook tool and Pebble
// calls), and under each the functions of the charm's own code, rebuilt from
// the stack samples' timeline, so the viewer can draw what ran when, and what
// was running when memory rose.

// TraceFunc is the Kind of a TraceRow that is a function of the charm's code.
const TraceFunc = "function"

const (
	// traceSlowCall is how long a hook tool or Pebble call takes to be listed
	// in the compact trace.
	traceSlowCall = 20 * time.Millisecond
	// traceShortSpan is how long an observer takes to be listed.
	traceShortSpan = 5 * time.Millisecond
	// traceShortFunc is how long a function runs to be listed.
	traceShortFunc = 15 * time.Millisecond
	// traceSlack is how far past a function's end a call may run and still
	// be listed under it: the samples place the end only to within one.
	traceSlack = 20 * time.Millisecond
	// traceCtxAge is how old the last sample before a span may be and still
	// say what function the span was called from.
	traceCtxAge = 100 * time.Millisecond
)

// TraceOptions choose what a trace lists.
type TraceOptions struct {
	Detail Detail // what code the function rows show
	All    bool   // list every span, call and function, however short

	// shortest is how long a call or a function must run to be listed: a
	// hundredth of the process, and at least the traceShort* minimums.
	shortest time.Duration
}

// TraceRow is one bar of a trace: a span, a call, or a function.
type TraceRow struct {
	Key      string // identifies the row between builds of the same trace
	Depth    int
	Kind     string // recording.Ops* kinds, or TraceFunc
	Label    string
	Loc      string // where a function is: "charm.py:93"
	Detail   string // an exit code, share of the hook, call arguments
	Class    Class  // for a function: whose code it is
	Start    time.Time
	End      time.Time
	CPUMS    float64 // functions: from the stack samples
	MemBytes int64   // functions: resident memory the samples saw it take
	Samples  int     // functions: how many samples saw it running
	Waiting  bool    // a function that took long for the CPU it used
	// WaitingOn says what that function was waiting on: a hook tool or
	// Pebble call, or "in <function>" where it was blocked.
	WaitingOn string
	Failed    bool
	Error     string
	Marks     []string
	Cost      *OpsCost // spans: what the span cost, when measured
	SpanID    string
	HasKids   bool // the row has rows under it
	Hidden    int  // short calls and events under the row left out
}

// Mem is how much resident memory the row took, and whether it is known: from
// the stack samples for a function and for init shown as the constructor, from
// the tap's readings at the span's start and end for other spans.
func (r TraceRow) Mem() (int64, bool) {
	switch {
	case r.Kind == TraceFunc || r.Class == ClassCharm:
		return r.MemBytes, true
	case r.Cost != nil && r.Cost.RSSSource != "" && r.Cost.RSSSource != RSSFromStacks:
		return r.Cost.RSSGrowth(), true
	}
	return 0, false
}

// Dur is how long the row ran.
func (r TraceRow) Dur() time.Duration { return r.End.Sub(r.Start) }

// TracePoint is the process's resident memory at a time.
type TracePoint struct {
	T   time.Time
	RSS int64
}

// Trace is the rows of one hook process in start order, nested by depth,
// and the process's memory over the same time.
type Trace struct {
	From, To time.Time
	Rows     []TraceRow
	RSS      []TracePoint
	// ImportMS is the time before ops started, spent importing Python
	// modules, which the compact trace leaves out.
	ImportMS float64
}

// tnode is a row with the rows under it, before flattening.
type tnode struct {
	row  TraceRow
	kids []*tnode
}

// traceProc is what a process's stack samples give a trace.
type traceProc struct {
	calls  []callRange // the hook tool and Pebble calls of the process, in words
	prof   *recording.OpsProfile
	vis    *Profile  // the process's stacks at the chosen detail, aligned with prof.Stacks
	paths  [][]int   // each stack's visible frames, outermost first
	avgCPU []float64 // each stack's CPU time per sample, ns
}

func newTraceProc(prof *recording.OpsProfile, d Detail, calls []callRange) *traceProc {
	raw := &Profile{Frames: prof.Frames, Stacks: prof.Stacks}
	tp := &traceProc{prof: prof, vis: raw.AtDetail(d), calls: calls}
	tp.paths = make([][]int, len(prof.Stacks))
	tp.avgCPU = make([]float64, len(prof.Stacks))
	for i, s := range tp.vis.Stacks {
		for j := len(s.Frames) - 1; j >= 0; j-- {
			if f := s.Frames[j]; f < len(prof.Frames) {
				tp.paths[i] = append(tp.paths[i], f)
			}
		}
		if n := prof.Stacks[i].Samples; n > 0 {
			tp.avgCPU[i] = float64(prof.Stacks[i].CPUNs) / float64(n)
		}
	}
	return tp
}

// BuildTrace lays out the tree of one hook process.
func BuildTrace(root *OpsNode, profs []recording.OpsProfile, o TraceOptions) *Trace {
	byRoot := map[string]*traceProc{}
	calls := callRanges(root)
	for i := range profs {
		byRoot[profs[i].RootSpanID] = newTraceProc(&profs[i], o.Detail, calls)
	}
	o.shortest = root.Dur / 100
	tr := &Trace{From: root.Start, To: root.Start.Add(root.Dur)}
	if tp := byRoot[root.SpanID]; tp != nil {
		prev := tp.prof.Start
		for _, s := range tp.prof.Timeline {
			tr.RSS = append(tr.RSS, TracePoint{T: s.Time, RSS: s.RSSBytes})
			if s.Stack >= 0 && s.Stack < len(tp.prof.Stacks) && tp.prof.Stacks[s.Stack].SpanID == root.SpanID && !prev.IsZero() {
				tr.ImportMS += float64(min(s.Time.Sub(prev), 100*time.Millisecond).Microseconds()) / 1000
			}
			prev = s.Time
		}
	}
	top := buildTraceNode(root, root, byRoot[root.SpanID], byRoot, o)
	settleTrace(top)
	flattenTrace(top, 0, "", tr)
	return tr
}

func flattenTrace(n *tnode, depth int, parentKey string, tr *Trace) {
	n.row.Depth = depth
	n.row.Key = parentKey + "/" + n.row.Label + "@" + n.row.Start.Format("150405.000000")
	n.row.HasKids = len(n.kids) > 0
	tr.Rows = append(tr.Rows, n.row)
	for _, k := range n.kids {
		flattenTrace(k, depth+1, n.row.Key, tr)
	}
}

// showTraceChild reports whether a child span gets a row in the compact trace.
func showTraceChild(c *OpsNode, o TraceOptions) bool {
	if o.All {
		return true
	}
	switch c.Kind {
	case recording.OpsTool:
		return c.Dur >= max(traceSlowCall, o.shortest) || c.Name == "status-set" || c.Error != ""
	case recording.OpsPebble:
		return c.Dur >= max(traceSlowCall, o.shortest)
	case recording.OpsBacklog:
		return len(c.Children) > 0 || c.Error != ""
	case recording.OpsEvent:
		return len(c.Children) > 0 || len(c.Marks) > 0 || c.Error != ""
	case recording.OpsObserver:
		return c.Dur >= traceShortSpan || len(c.Children) > 0 || len(c.Marks) > 0 || c.Error != ""
	}
	return true
}

func buildTraceNode(n, root *OpsNode, tp *traceProc, byRoot map[string]*traceProc, o TraceOptions) *tnode {
	ln := lineFor(n, 0, root)
	tn := &tnode{row: TraceRow{
		Kind: n.Kind, Label: ln.Label, Loc: ln.Loc, Detail: ln.Detail, Start: n.Start, End: n.Start.Add(n.Dur),
		Failed: ln.Failed, Error: ln.Error, Marks: n.Marks, Cost: n.Cost, SpanID: n.SpanID,
	}}
	if n.Kind == recording.OpsRoot && n.Name == "" {
		tn.row.Label = "process"
	}
	own := map[string]bool{n.SpanID: true}
	for _, c := range n.Children {
		if isCall(c) {
			own[c.SpanID] = true
		}
	}
	var funcs []*tnode
	if tp != nil && !isCall(n) {
		skip := ""
		if n.Kind == recording.OpsObserver {
			skip = ShortObserver(n.Name)
		}
		funcs = traceFuncs(tp, n, own, skip, o)
	}
	// Init isn't a function: shown as the constructor that is most of it.
	if n.Kind == recording.OpsInit {
		for i, f := range funcs {
			if strings.HasSuffix(f.row.Label, "__init__") && f.row.Dur()*10 >= n.Dur*9 {
				tn.row.Label, tn.row.Loc, tn.row.Class, tn.row.MemBytes = f.row.Label, f.row.Loc, ClassCharm, f.row.MemBytes
				funcs = append(append(append([]*tnode(nil), funcs[:i]...), f.kids...), funcs[i+1:]...)
				break
			}
		}
	}
	var kids []*tnode
	for _, c := range n.Children {
		if !showTraceChild(c, o) {
			tn.row.Hidden++
			continue
		}
		sub := tp
		if c.Kind == recording.OpsRoot {
			sub = byRoot[c.SpanID]
		}
		kids = append(kids, buildTraceNode(c, root, sub, byRoot, o))
	}
	// A call or an event belongs under the function that was running when it
	// began; the rest sit directly under the span.
	for _, k := range kids {
		placeUnder(&funcs, tn, k)
	}
	tn.kids = append(tn.kids, funcs...)
	sortTrace(tn)
	return tn
}

// placeUnder adds k under the deepest function in funcs that was running when
// k began, or else under parent.
func placeUnder(funcs *[]*tnode, parent, k *tnode) {
	at := k.row.Start
	list := funcs
	var host *tnode
	for {
		var next *tnode
		for _, f := range *list {
			if f.row.Kind == TraceFunc && !at.Before(f.row.Start) && !k.row.End.After(f.row.End.Add(traceSlack)) {
				next = f
				break
			}
		}
		if next == nil {
			break
		}
		host, list = next, &next.kids
	}
	if host == nil {
		parent.kids = append(parent.kids, k)
		return
	}
	host.kids = append(host.kids, k)
}

func sortTrace(n *tnode) {
	sort.SliceStable(n.kids, func(i, j int) bool { return n.kids[i].row.Start.Before(n.kids[j].row.Start) })
	for _, k := range n.kids {
		sortTrace(k)
	}
}

// open is a function that is running at a point of the samples' timeline.
type open struct {
	frame  int
	node   *tnode
	stacks map[int]int // samples by stack
}

// traceFuncs rebuilds, from the samples taken in the spans of own, the
// functions of the charm's code that ran in n as a flame chart: a function
// runs from the sample before the first that saw it to the last that did.
func traceFuncs(tp *traceProc, n *OpsNode, own map[string]bool, skip string, o TraceOptions) []*tnode {
	prof := tp.prof
	// What the span was called from: the stack of the last sample before it.
	var ctx []int
	for i := len(prof.Timeline) - 1; i >= 0; i-- {
		s := prof.Timeline[i]
		if s.Time.After(n.Start) || s.Stack < 0 || s.Stack >= len(prof.Stacks) || own[prof.Stacks[s.Stack].SpanID] {
			continue
		}
		if n.Start.Sub(s.Time) <= traceCtxAge {
			ctx = tp.paths[s.Stack]
		}
		break
	}
	interval := time.Duration(prof.IntervalNs)
	if interval <= 0 {
		interval = 10 * time.Millisecond
	}

	var roots, stack []*tnode
	var opened, finished []open
	var lastT time.Time
	closeTo := func(depth int) {
		for len(opened) > depth {
			f := opened[len(opened)-1]
			f.node.row.End = lastT
			finished = append(finished, f)
			opened = opened[:len(opened)-1]
			stack = stack[:len(stack)-1]
		}
	}
	for i, s := range prof.Timeline {
		if s.Stack < 0 || s.Stack >= len(prof.Stacks) {
			continue
		}
		st := prof.Stacks[s.Stack]
		if !own[st.SpanID] {
			continue
		}
		path := tp.paths[s.Stack]
		k := 0
		for k < len(path) && k < len(ctx) && path[k] == ctx[k] {
			k++
		}
		path = path[k:]
		if len(path) > 0 && skip != "" && tp.vis.Frames[path[0]].Func == skip {
			path = path[1:]
		}
		prevT := s.Time.Add(-interval)
		prevRSS := prof.RSSStart
		if i > 0 {
			prevT, prevRSS = prof.Timeline[i-1].Time, prof.Timeline[i-1].RSSBytes
		}
		// The functions of the path shared with those open stay open.
		keep := 0
		for keep < len(opened) && keep < len(path) && opened[keep].frame == path[keep] {
			keep++
		}
		closeTo(keep)
		for _, f := range path[keep:] {
			fn := FrameName(tp.vis.Frames[f])
			_, loc := FrameLabel(tp.vis.Frames[f])
			tn := &tnode{row: TraceRow{
				Kind: TraceFunc, Label: fn, Loc: path0(loc), Class: tp.vis.Class(f),
				Start: prevT, End: s.Time, SpanID: n.SpanID,
			}}
			if len(stack) > 0 {
				stack[len(stack)-1].kids = append(stack[len(stack)-1].kids, tn)
			} else {
				roots = append(roots, tn)
			}
			stack = append(stack, tn)
			opened = append(opened, open{frame: f, node: tn, stacks: map[int]int{}})
		}
		grow := max(s.RSSBytes-prevRSS, 0)
		for j := range opened {
			r := &opened[j].node.row
			r.Samples++
			r.CPUMS += tp.avgCPU[s.Stack] / 1e6
			r.MemBytes += grow
			opened[j].stacks[s.Stack]++
		}
		lastT = s.Time
	}
	closeTo(0)
	for _, f := range finished {
		r := &f.node.row
		if d := r.Dur(); d >= funcWaiting && r.CPUMS*float64(time.Millisecond)*10 < float64(d) {
			r.WaitingOn = tp.waitingOn(f, r)
		}
	}

	// Leave out the functions that ran briefly, and mark those that took long
	// for the CPU they used.
	var prune func(list []*tnode) []*tnode
	prune = func(list []*tnode) []*tnode {
		var out []*tnode
		for _, f := range list {
			d := f.row.Dur()
			if !o.All && d < max(traceShortFunc, o.shortest) {
				continue
			}
			f.row.Waiting = d >= funcWaiting && f.row.CPUMS*float64(time.Millisecond)*10 < float64(d)
			f.kids = prune(f.kids)
			out = append(out, f)
		}
		return out
	}
	return prune(roots)
}

// path0 is the file name and line of a location: "charm.py:93".
func path0(loc string) string { return path.Base(loc) }

// callRange is a hook tool or Pebble call in time, in words.
type callRange struct {
	start, end time.Time
	label      string
}

// callRanges lists the hook tool and Pebble calls under n.
func callRanges(n *OpsNode) []callRange {
	var out []callRange
	var walk func(n *OpsNode)
	walk = func(n *OpsNode) {
		for _, c := range n.Children {
			if isCall(c) {
				out = append(out, callRange{c.Start, c.Start.Add(c.Dur), callLabel(c)})
			}
			walk(c)
		}
	}
	walk(n)
	return out
}

// waitingOn says what a function was waiting for: the hook tool or Pebble
// call that covered most of its time, or else, from where its heaviest stack
// was blocked, what that was.
func (tp *traceProc) waitingOn(f open, r *TraceRow) string {
	var best callRange
	var bestOverlap time.Duration
	for _, c := range tp.calls {
		lo, hi := c.start, c.end
		if r.Start.After(lo) {
			lo = r.Start
		}
		if r.End.Before(hi) {
			hi = r.End
		}
		if o := hi.Sub(lo); o > bestOverlap {
			best, bestOverlap = c, o
		}
	}
	if bestOverlap*2 >= r.Dur() {
		return "waiting for " + best.label
	}
	byReason := map[string]int64{}
	for i, n := range f.stacks {
		var frames []recording.ProfileFrame
		if i >= 0 && i < len(tp.prof.Stacks) {
			for _, fi := range tp.prof.Stacks[i].Frames {
				if fi >= 0 && fi < len(tp.prof.Frames) {
					frames = append(frames, tp.prof.Frames[fi])
				}
			}
		}
		byReason[blockedOn(frames)] += int64(n)
	}
	return heaviestReason(byReason)
}

// settleTrace clears the waiting mark from a function whose time is mostly
// that of a function under it that is waiting: the deepest one says it. It
// reports whether the row, or one under it covering most of its time, is
// waiting.
func settleTrace(n *tnode) bool {
	covered := false
	for _, k := range n.kids {
		if settleTrace(k) && k.row.Kind == TraceFunc && float64(k.row.Dur()) >= 0.6*float64(n.row.Dur()) {
			covered = true
		}
	}
	if covered && n.row.Kind == TraceFunc {
		n.row.Waiting, n.row.WaitingOn = false, ""
	}
	return n.row.Waiting || covered
}
