package narrative

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// stacksTree is the one config-changed process of the lab recording
// "stacks" (otelcol, profiled), with its profile.
func stacksTree(t *testing.T) (*OpsNode, []recording.OpsProfile) {
	t.Helper()
	spans, profs, err := recording.LoadOps(filepath.Join("..", "recording", "testdata", "ops", "stacks"))
	if err != nil {
		t.Fatal(err)
	}
	x := NewOpsIndex(spans)
	var roots []string
	for _, sp := range spans {
		if sp.Attrs["kind"] == recording.OpsRoot && sp.ParentSpanID == "" {
			roots = append(roots, sp.SpanID)
		}
	}
	trees := x.Trees(roots)
	if len(trees) != 1 || len(profs) != 1 {
		t.Fatalf("%d trees and %d profiles, want 1 each", len(trees), len(profs))
	}
	return trees[0], profs
}

func childOfKind(n *OpsNode, kind string) *OpsNode {
	for _, c := range n.Children {
		if c.Kind == kind {
			return c
		}
	}
	return nil
}

// TestAttachCostsFromStacks: a span's CPU is what its stack samples, and
// those of the spans under it, add up to; its memory comes from the RSS
// the samples read, around it.
func TestAttachCostsFromStacks(t *testing.T) {
	root, profs := stacksTree(t)
	AttachCosts([]*OpsNode{root}, profs)

	var cpu, wall int64
	for _, s := range profs[0].Stacks {
		cpu += s.CPUNs
		wall += s.WallNs
	}
	if got := root.Cost.CPUMS; int64(got*1e6)/1000 != cpu/1000 {
		t.Errorf("process CPU %vms, its stacks add up to %dns", got, cpu)
	}
	init := childOfKind(root, recording.OpsInit)
	spans := SubtreeSpans(init)
	var initCPU int64
	var initSamples int
	for _, s := range profs[0].Stacks {
		if spans[s.SpanID] {
			initCPU += s.CPUNs
			initSamples += s.Samples
		}
	}
	if init.Cost == nil || init.Cost.Samples != initSamples || int64(init.Cost.CPUMS*1e6)/1000 != initCPU/1000 {
		t.Fatalf("init cost %+v, want %d samples, %dns CPU", init.Cost, initSamples, initCPU)
	}
	// Init is most of the hook here, and the samples saw RSS within it.
	if init.Cost.Samples*2 < root.Cost.Samples || init.Cost.RSSSource != RSSFromSamples ||
		init.Cost.RSSPeakBytes < init.Cost.RSSEndBytes || init.Cost.RSSPeakBytes > profs[0].RSSPeak {
		t.Errorf("init cost %+v, process %+v", init.Cost, root.Cost)
	}
	observers := 0
	var walk func(n *OpsNode)
	walk = func(n *OpsNode) {
		if n.Kind == recording.OpsObserver {
			observers++
			if n.Cost == nil || !n.Cost.Profiled {
				t.Errorf("observer %s has no cost", n.Name)
			}
		}
		if (n.Kind == recording.OpsTool || n.Kind == recording.OpsPebble) && n.Cost != nil {
			t.Errorf("call %s has a cost", n.Name)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	if observers == 0 {
		t.Fatal("no observers in the fixture")
	}
}

// TestAttachCostsPrefersTapRSS: RSS the tap read at a span's start and end
// (rss_*_bytes) wins over the samples', and growth is end less start.
func TestAttachCostsPrefersTapRSS(t *testing.T) {
	root, profs := stacksTree(t)
	init := childOfKind(root, recording.OpsInit)
	init.Attrs[AttrRSSStart] = strconv.Itoa(60 << 20)
	init.Attrs[AttrRSSEnd] = strconv.Itoa(75 << 20)
	init.Attrs[AttrRSSPeak] = strconv.Itoa(90 << 20)
	init.Attrs[AttrRSSPeakExact] = "true"
	AttachCosts([]*OpsNode{root}, profs)
	c := init.Cost
	if c.RSSSource != RSSFromTap || c.RSSGrowth() != 15<<20 || c.RSSPeakBytes != 90<<20 || !c.RSSPeakExact {
		t.Errorf("init cost %+v", c)
	}
	if _, growth, peak := OpsCostCells(c); growth != "+15MB" || peak != "90MB!" {
		t.Errorf("cells %q %q", growth, peak)
	}
}

// TestNoCostWithoutMeasurements: a recording made before stacks or RSS
// readings keeps its tree as it was.
func TestNoCostWithoutMeasurements(t *testing.T) {
	spans := opsFixture(t, "k8s")
	x := NewOpsIndex(spans)
	var roots []string
	for _, sp := range spans {
		if sp.Attrs["kind"] == recording.OpsRoot && sp.ParentSpanID == "" {
			roots = append(roots, sp.SpanID)
		}
	}
	trees := x.Trees(roots)
	AttachCosts(trees, nil)
	if lines := OpsLines(trees, false); OpsHasCost(lines) {
		t.Error("cost columns on a recording with no measurements")
	}
}

// TestProfileScopesAndTopStacks: a profile limited to init's spans holds
// init's samples only, and the heaviest stacks come first.
func TestProfileScopesAndTopStacks(t *testing.T) {
	root, profs := stacksTree(t)
	AttachCosts([]*OpsNode{root}, profs)
	init := childOfKind(root, recording.OpsInit)
	p := NewProfile(profs, SubtreeSpans(init))
	if got := p.Total().Samples; got != init.Cost.Samples {
		t.Errorf("init profile %d samples, init cost %d", got, init.Cost.Samples)
	}
	tree := p.Tree()
	if tree.Total != p.Total() {
		t.Errorf("tree total %+v, profile %+v", tree.Total, p.Total())
	}
	top := NewProfile(profs, nil).TopStacks(MetricWall, 5)
	if len(top) != 5 {
		t.Fatalf("%d top stacks", len(top))
	}
	for i := 1; i < len(top); i++ {
		if top[i].WallNs > top[i-1].WallNs {
			t.Errorf("stack %d heavier than %d", i, i-1)
		}
	}
	// The lab hook waits on Pebble in refresh_certs for most of init.
	whole := NewProfile(profs, nil)
	found := false
	for _, f := range top[0].Frames {
		if fn, _ := whole.FrameLabel(f); fn == "refresh_certs" {
			found = true
		}
	}
	if !found || top[0].SpanID != init.SpanID {
		t.Errorf("heaviest stack isn't init's refresh_certs wait: %+v", top[0])
	}
	mem := whole.TopStacks(MetricMem, 1)
	if fn, _ := whole.FrameLabel(mem[0].Frames[0]); fn != "compress" {
		t.Errorf("most memory growth in %q, want lzma compress", fn)
	}
}

// TestProfileAtDetail: hiding frames charges their cost to the nearest
// visible caller and never changes the totals; each level shows a subset of
// the one above.
func TestProfileAtDetail(t *testing.T) {
	_, profs := stacksTree(t)
	whole := NewProfile(profs, nil)
	want := whole.Total()
	prev := -1
	for _, d := range Details {
		p := whole.AtDetail(d)
		if got := p.Total(); got != want {
			t.Errorf("%s: total %+v, want %+v", d, got, want)
		}
		if tree := p.Tree(); tree.Total != want {
			t.Errorf("%s: tree total %+v", d, tree.Total)
		}
		frames := 0
		for _, s := range p.Stacks {
			frames += len(s.Frames)
			for _, f := range s.Frames {
				if d == DetailCharm && f < len(whole.Frames) && p.Class(f) != ClassCharm {
					t.Errorf("charm level shows %s", whole.Frames[f].Func)
				}
			}
		}
		if d != DetailCharmCallee && prev >= 0 && frames < prev {
			t.Errorf("%s shows fewer frames (%d) than the level before (%d)", d, frames, prev)
		}
		if d != DetailCharmCallee {
			prev = frames
		}
	}
	// Something ran in a hidden callee of a charm function.
	var folded Value
	for _, fs := range whole.AtDetail(DetailCharm).Functions(MetricWall) {
		folded.WallNs += fs.Folded.WallNs
	}
	if folded.WallNs == 0 {
		t.Error("no cost was folded into a visible caller at the charm level")
	}
}

// TestProfilePeek: a function's callers and callees add up to its total.
func TestProfilePeek(t *testing.T) {
	_, profs := stacksTree(t)
	p := NewProfile(profs, nil)
	var frame int
	for i, f := range p.Frames {
		if f.Func == "refresh_certs" {
			frame = i
		}
	}
	self, callers, callees := p.Peek(frame, MetricWall)
	var up, down int64
	for _, e := range callers {
		up += e.Value.WallNs
	}
	for _, e := range callees {
		down += e.Value.WallNs
	}
	if up != self.Total.WallNs || down != self.Total.WallNs || self.Total.WallNs == 0 {
		t.Errorf("total %d, callers %d, callees %d", self.Total.WallNs, up, down)
	}
}

// TestInitListsCharmFunctions: charm init lists the functions of the charm's
// code that ran in it, as a call tree with how long each took; the Pebble
// calls fold into one row; observers list what they called, not themselves.
func TestInitListsCharmFunctions(t *testing.T) {
	root, profs := stacksTree(t)
	AttachCosts([]*OpsNode{root}, profs)
	init := childOfKind(root, recording.OpsInit)
	if len(init.Funcs) == 0 {
		t.Fatal("init lists no functions")
	}
	var path []string
	var find func(fs []*FuncCall) *FuncCall
	find = func(fs []*FuncCall) *FuncCall {
		for _, f := range fs {
			path = append(path, f.Name)
			if f.Name == "refresh_certs" {
				return f
			}
			if got := find(f.Calls); got != nil {
				return got
			}
			path = path[:len(path)-1]
		}
		return nil
	}
	refresh := find(init.Funcs)
	if refresh == nil || len(path) < 2 {
		t.Fatalf("refresh_certs not under init's call path: %v", path)
	}
	if !strings.HasSuffix(init.Ctor, "__init__") {
		t.Errorf("init is shown as %q, want the charm's constructor", init.Ctor)
	}
	if refresh.WallMS < 500 || !refresh.Waiting {
		t.Errorf("refresh_certs %.0fms, mostly waiting %v: it waits on Pebble", refresh.WallMS, refresh.Waiting)
	}
	for _, f := range init.Funcs {
		if strings.HasPrefix(f.Name, "ops.") || strings.Contains(f.At, "ops/") {
			t.Errorf("ops function %s listed as the charm's", f.Name)
		}
	}
	lines := OpsLines([]*OpsNode{root}, false)
	var funcs, pebbleRows int
	var pebbleFolded bool
	for _, l := range lines {
		switch {
		case l.Kind == OpsLineFunc:
			funcs++
		case l.Kind == recording.OpsPebble:
			pebbleRows++
		case l.Kind == OpsLineCalls && strings.Contains(l.Label, "Pebble"):
			pebbleFolded = true
		}
	}
	if funcs < 3 || pebbleRows != 0 || !pebbleFolded {
		t.Errorf("%d function rows, %d Pebble rows, folded %v", funcs, pebbleRows, pebbleFolded)
	}
	for _, o := range root.Children {
		if o.Kind != recording.OpsEvent {
			continue
		}
		for _, ob := range o.Children {
			for _, f := range ob.Funcs {
				if f.Name == ShortObserver(ob.Name) {
					t.Errorf("observer %s lists itself", ob.Name)
				}
			}
		}
	}
}

// TestWrapperFramesAreHidden: the charm_tracing decorators between the
// charm's functions are hidden at every level but "all"; their cost goes to
// the function that was wrapped.
func TestWrapperFramesAreHidden(t *testing.T) {
	const lib = "/var/lib/juju/agents/unit-x-0/charm/lib/charms/tempo_coordinator_k8s/v0/charm_tracing.py"
	const src = "/var/lib/juju/agents/unit-x-0/charm/src/charm.py"
	p := &Profile{
		Frames: []recording.ProfileFrame{
			{Func: "work", File: src, Line: 10},
			{Func: "_trace_callable.<locals>.wrapped_function", File: lib, Line: 50},
			{Func: "Charm._on_event", File: src, Line: 20},
			{Func: "wrapped_function", File: lib, Line: 50},
			{Func: "Charm.__init__", File: src, Line: 5},
		},
		Stacks: []recording.ProfileStack{{Frames: []int{0, 1, 2, 3, 4}, Samples: 3, WallNs: 3e7}},
	}
	for _, d := range []Detail{DetailCharm, DetailCharmCallee, DetailOps} {
		q := p.AtDetail(d)
		for _, f := range q.Stacks[0].Frames {
			if fn := FrameName(q.Frames[f]); strings.HasSuffix(fn, "wrapped_function") {
				t.Errorf("%s shows the wrapper %s", d, fn)
			}
		}
		if got := q.Total(); got != p.Total() {
			t.Errorf("%s: total %+v, want %+v", d, got, p.Total())
		}
		if got := len(q.Stacks[0].Frames); got != 3 {
			t.Errorf("%s: %d frames, want the 3 of the charm's own functions", d, got)
		}
	}
	if q := p.AtDetail(DetailAll); len(q.Stacks[0].Frames) != 5 {
		t.Errorf("all code shows %d frames, want 5", len(q.Stacks[0].Frames))
	}
}

// TestTraceNestsCallsUnderFunctions: the trace lays the hook out along time:
// charm init holds the functions of the charm's code that ran, rebuilt from
// the samples, and the Pebble call a function waited on sits under it.
func TestTraceNestsCallsUnderFunctions(t *testing.T) {
	root, profs := stacksTree(t)
	AttachCosts([]*OpsNode{root}, profs)
	tr := BuildTrace(root, profs, TraceOptions{Detail: DetailCharm})
	if len(tr.Rows) < 5 || tr.Rows[0].Kind != recording.OpsRoot || tr.Rows[0].Depth != 0 {
		t.Fatalf("%d rows, first %+v", len(tr.Rows), tr.Rows[0])
	}
	if len(tr.RSS) == 0 {
		t.Error("trace has no memory over time")
	}
	idx := map[string]int{}
	for i, r := range tr.Rows {
		if r.Kind == TraceFunc {
			idx[r.Label] = i
		}
		if r.Start.Before(tr.From.Add(-time.Second)) || r.End.After(tr.To.Add(time.Second)) {
			t.Errorf("row %s %v–%v outside the process %v–%v", r.Label, r.Start, r.End, tr.From, tr.To)
		}
		if r.End.Before(r.Start) {
			t.Errorf("row %s ends before it starts", r.Label)
		}
	}
	refresh, ok := idx["refresh_certs"]
	if !ok {
		t.Fatalf("no refresh_certs row in %d rows", len(tr.Rows))
	}
	rc := tr.Rows[refresh]
	if !rc.Waiting || rc.Dur() < 500*time.Millisecond {
		t.Errorf("refresh_certs %v waiting %v: it waits on Pebble", rc.Dur(), rc.Waiting)
	}
	if rc.Depth < 3 {
		t.Errorf("refresh_certs at depth %d: it runs under the root, __init__ (init) and _reconcile", rc.Depth)
	}
	// A Pebble call under refresh_certs, nested inside it in time.
	found := false
	for _, r := range tr.Rows[refresh+1:] {
		if r.Depth <= rc.Depth {
			break
		}
		if r.Kind == recording.OpsPebble && r.Dur() >= 20*time.Millisecond {
			found = true
			if r.Start.Before(rc.Start) || r.End.After(rc.End.Add(50*time.Millisecond)) {
				t.Errorf("call %s %v–%v not inside refresh_certs %v–%v", r.Label, r.Start, r.End, rc.Start, rc.End)
			}
		}
	}
	if !found {
		t.Error("the Pebble wait isn't listed under refresh_certs")
	}
	// Short calls are left out unless asked for.
	short := BuildTrace(root, profs, TraceOptions{Detail: DetailCharm, All: true})
	if len(short.Rows) <= len(tr.Rows) {
		t.Errorf("all: %d rows, compact: %d", len(short.Rows), len(tr.Rows))
	}
	for _, r := range tr.Rows {
		if (r.Kind == recording.OpsPebble || r.Kind == recording.OpsTool) && r.Dur() < 20*time.Millisecond && r.Label != "status-set active" && !r.Failed {
			t.Errorf("compact trace lists the short call %s (%v)", r.Label, r.Dur())
		}
	}
}
