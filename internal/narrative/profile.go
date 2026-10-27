package narrative

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// This file turns the stack samples the tap took (recording.OpsProfile, one
// per hook process) into what the viewer's flame graph and the export show:
// the stacks of one span and the spans under it, merged into one call tree.

// Metric selects what a profile weighs stacks by.
type Metric int

const (
	MetricWall Metric = iota // time on the main thread, on CPU or waiting
	MetricCPU                // the main thread's CPU time
	MetricMem                // resident memory growth seen with the stack
)

// Metrics lists every Metric, in the order views cycle through them.
var Metrics = []Metric{MetricWall, MetricCPU, MetricMem}

func (m Metric) String() string {
	switch m {
	case MetricCPU:
		return "cpu"
	case MetricMem:
		return "memory growth"
	}
	return "wall"
}

// Format renders a value of this metric, nanoseconds or bytes.
func (m Metric) Format(v float64) string {
	if m == MetricMem {
		return FormatBytes(int64(v))
	}
	return FormatOpsDur(time.Duration(v))
}

// Value is what a set of samples adds up to.
type Value struct {
	Samples  int
	WallNs   int64
	CPUNs    int64
	MemBytes int64
}

func (v *Value) add(s recording.ProfileStack) {
	v.Samples += s.Samples
	v.WallNs += s.WallNs
	v.CPUNs += s.CPUNs
	v.MemBytes += s.MemBytes
}

// Get returns one metric of v: nanoseconds or bytes.
func (v Value) Get(m Metric) float64 {
	switch m {
	case MetricCPU:
		return float64(v.CPUNs)
	case MetricMem:
		return float64(v.MemBytes)
	}
	return float64(v.WallNs)
}

// Profile is the stacks sampled in a set of spans, from one or more
// processes, with their frames in one table: a function the processes share
// is one frame.
type Profile struct {
	Frames []recording.ProfileFrame
	Stacks []recording.ProfileStack // Frames index Profile.Frames, innermost first

	// folded[i] is set when Stacks[i]'s innermost frame was hidden by AtDetail,
	// so its cost is charged to the nearest visible caller. nil when nothing
	// was hidden.
	folded []bool
}

// NewProfile gathers the stacks of profs that ran in one of spans, or every
// stack when spans is nil. Stacks of the same call path in different
// processes or spans stay apart.
func NewProfile(profs []recording.OpsProfile, spans map[string]bool) *Profile {
	p := &Profile{}
	ids := map[recording.ProfileFrame]int{}
	for _, pr := range profs {
		remap := make([]int, len(pr.Frames))
		for i, f := range pr.Frames {
			j, ok := ids[f]
			if !ok {
				j = len(p.Frames)
				ids[f] = j
				p.Frames = append(p.Frames, f)
			}
			remap[i] = j
		}
		for _, s := range pr.Stacks {
			if s.Samples == 0 || spans != nil && !spans[s.SpanID] {
				continue
			}
			c := s
			c.Frames = make([]int, 0, len(s.Frames))
			for _, f := range s.Frames {
				if f >= 0 && f < len(remap) {
					c.Frames = append(c.Frames, remap[f])
				}
			}
			p.Stacks = append(p.Stacks, c)
		}
	}
	return p
}

// Total is what every stack adds up to.
func (p *Profile) Total() Value {
	var v Value
	for _, s := range p.Stacks {
		v.add(s)
	}
	return v
}

// Node is a function in a call tree: one path from the outermost frame.
type Node struct {
	Frame  int // index into Profile.Frames; -1 for the root
	Total  Value
	Self   Value
	Folded Value // the part of Self that ran in hidden callees
	// StackIdx lists the stacks, by index in Profile.Stacks, that go through
	// the node.
	StackIdx []int
	Parent   *Node
	Children []*Node
}

// Tree merges the stacks into a call tree under a root with no frame.
func (p *Profile) Tree() *Node {
	root := &Node{Frame: -1}
	type key struct {
		parent *Node
		frame  int
	}
	kids := map[key]*Node{}
	for si, s := range p.Stacks {
		root.Total.add(s)
		n := root
		for i := len(s.Frames) - 1; i >= 0; i-- {
			k := key{n, s.Frames[i]}
			c := kids[k]
			if c == nil {
				c = &Node{Frame: s.Frames[i], Parent: n}
				kids[k] = c
				n.Children = append(n.Children, c)
			}
			c.Total.add(s)
			c.StackIdx = append(c.StackIdx, si)
			n = c
		}
		n.Self.add(s)
		if p.folded != nil && p.folded[si] {
			n.Folded.add(s)
		}
	}
	return root
}

// Sort orders every node's children by m, heaviest first, then by frame.
func (n *Node) Sort(m Metric) {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i].Total.Get(m), n.Children[j].Total.Get(m)
		if a != b {
			return a > b
		}
		return n.Children[i].Frame < n.Children[j].Frame
	})
	for _, c := range n.Children {
		c.Sort(m)
	}
}

// Path is the frames from the outermost down to n.
func (n *Node) Path() []int {
	var out []int
	for c := n; c != nil && c.Frame >= 0; c = c.Parent {
		out = append([]int{c.Frame}, out...)
	}
	return out
}

// FuncStat is one function's share of a profile: Self is the time in its
// own code, Total also counts what it called. A recursive function counts
// once per stack.
type FuncStat struct {
	Frame       int
	Self, Total Value
	Folded      Value // the part of Self that ran in hidden callees
}

// Functions adds up the stacks per function, heaviest self value of m
// first, then total.
func (p *Profile) Functions(m Metric) []FuncStat {
	by := map[int]*FuncStat{}
	get := func(f int) *FuncStat {
		fs := by[f]
		if fs == nil {
			fs = &FuncStat{Frame: f}
			by[f] = fs
		}
		return fs
	}
	for i, s := range p.Stacks {
		if len(s.Frames) == 0 {
			continue
		}
		get(s.Frames[0]).Self.add(s)
		if p.folded != nil && p.folded[i] {
			get(s.Frames[0]).Folded.add(s)
		}
		seen := make(map[int]bool, len(s.Frames))
		for _, f := range s.Frames {
			if !seen[f] {
				seen[f] = true
				get(f).Total.add(s)
			}
		}
	}
	out := make([]FuncStat, 0, len(by))
	for _, fs := range by {
		out = append(out, *fs)
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := out[i].Self.Get(m), out[j].Self.Get(m); a != b {
			return a > b
		}
		if a, b := out[i].Total.Get(m), out[j].Total.Get(m); a != b {
			return a > b
		}
		return out[i].Frame < out[j].Frame
	})
	return out
}

// TopStacks returns the n heaviest stacks by m, with the same call path in
// different spans or processes added together. A stack keeps the span of
// its heaviest part.
func (p *Profile) TopStacks(m Metric, n int) []recording.ProfileStack {
	var (
		out   []recording.ProfileStack
		heavy []float64
	)
	byPath := map[string]int{}
	for _, s := range p.Stacks {
		k := fmt.Sprint(s.Frames)
		i, ok := byPath[k]
		if !ok {
			byPath[k] = len(out)
			out = append(out, recording.ProfileStack{SpanID: s.SpanID, Frames: s.Frames})
			heavy = append(heavy, 0)
			i = len(out) - 1
		}
		var v Value
		v.add(s)
		if w := v.Get(m); w > heavy[i] {
			heavy[i] = w
			out[i].SpanID = s.SpanID
		}
		out[i].Samples += s.Samples
		out[i].WallNs += s.WallNs
		out[i].CPUNs += s.CPUNs
		out[i].MemBytes += s.MemBytes
	}
	get := func(s recording.ProfileStack) float64 {
		var v Value
		v.add(s)
		return v.Get(m)
	}
	sort.SliceStable(out, func(i, j int) bool { return get(out[i]) > get(out[j]) })
	for len(out) > 0 && get(out[len(out)-1]) <= 0 {
		out = out[:len(out)-1]
	}
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// FrameLabel names a frame compactly: the function ("Class.method" on
// Python 3.11 and later) and where it is ("charm.py:12").
func (p *Profile) FrameLabel(i int) (fn, loc string) {
	if i < 0 || i >= len(p.Frames) {
		return "<all>", ""
	}
	return FrameLabel(p.Frames[i])
}

// FrameName is a frame's function as people read it: the top level of a
// module is its import, "importing lib/charms/x/v0/y".
func FrameName(f recording.ProfileFrame) string {
	if f.Func == "<module>" {
		return "importing " + strings.TrimSuffix(ShortPath(f.File), ".py")
	}
	return f.Func
}

// FrameLabel names a frame compactly; see Profile.FrameLabel.
func FrameLabel(f recording.ProfileFrame) (fn, loc string) {
	if f.File == "" {
		return f.Func, ""
	}
	return f.Func, ShortPath(f.File) + ":" + strconv.Itoa(f.Line)
}

// IsCharmFrame reports whether frame i is the charm's own code.
func (p *Profile) IsCharmFrame(i int) bool { return p.Class(i) == ClassCharm }

// Class is whose code a frame is.
type Class int

const (
	ClassOther Class = iota // the standard library, third-party libraries, the runtime
	ClassOps                // the ops framework
	ClassCharm              // the charm's src/ and its charm libraries
)

// Class reports whose code frame i is.
func (p *Profile) Class(i int) Class {
	if i < 0 || i >= len(p.Frames) {
		return ClassOther
	}
	switch f := p.Frames[i].File; {
	case IsCharmFile(f):
		return ClassCharm
	case IsOpsFile(f):
		return ClassOps
	}
	return ClassOther
}

// IsOpsFile reports whether a Python source file belongs to the ops
// framework: the ops package in the charm's venv.
func IsOpsFile(path string) bool {
	p := filepath.ToSlash(path)
	for _, marker := range []string{"/site-packages/", "/dist-packages/"} {
		if i := strings.LastIndex(p, marker); i >= 0 {
			return strings.HasPrefix(p[i+len(marker):], "ops/")
		}
	}
	return false
}

// ShortPath trims a Python source path to the part after site-packages, the
// charm directory, or the standard library directory, so "…/venv/lib/
// python3.12/site-packages/ops/main.py" reads "ops/main.py".
func ShortPath(path string) string {
	p := filepath.ToSlash(path)
	for _, marker := range []string{"/site-packages/", "/dist-packages/", "/charm/"} {
		if i := strings.LastIndex(p, marker); i >= 0 {
			return p[i+len(marker):]
		}
	}
	if i := strings.Index(p, "/lib/python3"); i >= 0 {
		if j := strings.IndexByte(p[i+len("/lib/"):], '/'); j >= 0 {
			return p[i+len("/lib/")+j+1:]
		}
	}
	return p
}

// IsCharmFile reports whether a Python source file is the charm's own code
// (its src/ and the charm libraries in lib/), rather than ops, another
// library from the charm's venv, or the standard library.
func IsCharmFile(path string) bool {
	p := filepath.ToSlash(path)
	return strings.Contains(p, "/charm/") && !strings.Contains(p, "/site-packages/") &&
		!strings.Contains(p, "/dist-packages/") && !strings.Contains(p, "/venv/")
}

// FormatBytes renders a byte count with a binary unit: "512B", "91MB",
// "1.5GB"; negative counts keep their sign.
func FormatBytes(n int64) string {
	if n < 0 {
		return "-" + FormatBytes(-n)
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	if v >= 10 {
		return fmt.Sprintf("%.0f%cB", v, "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f%cB", v, "KMGTPE"[exp])
}

// Detail is how much of the call stacks a profile shows. Frames below the
// level, and wrapper frames (IsWrapperFrame), are hidden, and what they cost
// is charged to the nearest visible frame that called them, so totals never
// change.
type Detail int

const (
	DetailCharm       Detail = iota // the charm's own code
	DetailCharmCallee               // the charm's code and what it calls directly
	DetailOps                       // the charm and the ops framework
	DetailAll                       // every frame
)

// Details lists every Detail, from the fewest frames to the most, in the
// order views cycle through them.
var Details = []Detail{DetailCharm, DetailCharmCallee, DetailOps, DetailAll}

func (d Detail) String() string {
	switch d {
	case DetailCharm:
		return "charm"
	case DetailCharmCallee:
		return "charm+callee"
	case DetailAll:
		return "all"
	}
	return "charm+ops"
}

// IsImportFrame reports whether a frame is the top level of a Python module:
// code that runs as the module is imported, or the module's entry point.
func IsImportFrame(f recording.ProfileFrame) bool { return f.Func == "<module>" }

// IsNoiseFrame reports whether a frame is one the views hide unless they show
// all code: a wrapper or a module's top level.
func IsNoiseFrame(f recording.ProfileFrame) bool { return IsImportFrame(f) || IsWrapperFrame(f) }

// IsWrapperFrame reports whether a frame only wraps another call: the
// charm_tracing library's decorators, which sit between the charm's own
// functions and add nothing to read. Every level but DetailAll hides them,
// and what they cost is charged to the function they call.
func IsWrapperFrame(f recording.ProfileFrame) bool {
	if filepath.Base(filepath.ToSlash(f.File)) != "charm_tracing.py" {
		return false
	}
	name := f.Func
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}
	return name == "wrapped_function" || name == "wrap_init"
}

func (d Detail) shows(c Class) bool {
	switch d {
	case DetailCharm, DetailCharmCallee:
		return c == ClassCharm
	case DetailOps:
		return c != ClassOther
	}
	return true
}

// Placeholders for a stack none of whose frames is visible: all of it ran
// outside the code the level shows. They say what it was, by what is on the
// stack, and are numbered after the profile's own frames.
var placeholders = []recording.ProfileFrame{
	{Func: "(Python runtime and libraries)"},
	{Func: "(importing Python modules)"},
	{Func: "(ops framework)"},
}

// placeholderFor says which placeholder stands for the stack: importing, if it
// is in the import machinery or in modules importing modules; the ops
// framework, if ops is on it; else the runtime.
func (p *Profile) placeholderFor(frames []int) int {
	modules, ops := 0, false
	for _, f := range frames {
		if f < 0 || f >= len(p.Frames) {
			continue
		}
		fr := p.Frames[f]
		switch {
		case strings.HasPrefix(fr.File, "<frozen importlib"):
			return 1
		case IsImportFrame(fr):
			modules++
		case p.Class(f) == ClassOps:
			ops = true
		}
	}
	switch {
	case modules > 1:
		return 1
	case ops:
		return 2
	}
	return 0
}

// AtDetail returns the profile with only the frames d shows. Frame numbers
// stay those of p, plus placeholder frames for stacks with nothing left; the
// result's totals equal p's.
func (p *Profile) AtDetail(d Detail) *Profile {
	if d == DetailAll {
		return p
	}
	out := &Profile{
		Frames: append(append([]recording.ProfileFrame(nil), p.Frames...), placeholders...),
		Stacks: make([]recording.ProfileStack, len(p.Stacks)),
		folded: make([]bool, len(p.Stacks)),
	}
	base := len(p.Frames)
	noise := func(f int) bool { return f >= 0 && f < len(p.Frames) && IsNoiseFrame(p.Frames[f]) }
	for i, s := range p.Stacks {
		c := s
		c.Frames = make([]int, 0, len(s.Frames))
		for j, f := range s.Frames {
			hidden := noise(f)
			keep := d.shows(p.Class(f)) && !hidden
			if !keep && !hidden && d == DetailCharmCallee {
				// What a charm function calls directly: the nearest frame
				// outside it, wrappers aside, is the charm's.
				k := j + 1
				for k < len(s.Frames) && noise(s.Frames[k]) {
					k++
				}
				keep = k < len(s.Frames) && p.Class(s.Frames[k]) == ClassCharm
			}
			if keep {
				c.Frames = append(c.Frames, f)
			} else if j == 0 {
				out.folded[i] = true
			}
		}
		if len(c.Frames) == 0 {
			c.Frames = []int{base + p.placeholderFor(s.Frames)}
			out.folded[i] = len(s.Frames) > 0
		}
		out.Stacks[i] = c
	}
	return out
}

// Edge is a function a function is called from or calls, with the cost of
// the stacks that went through that edge. Frame -1 is no caller (the
// function is the outermost frame) or, among callees, the function's own code.
type Edge struct {
	Frame int
	Value Value
}

// Peek lists what called function frame and what it called, like pprof's
// -peek: for each stack it passes through, the frame outside the function's
// innermost occurrence and the frame inside it. Both lists are heaviest
// first by m. The function's own totals are in the returned FuncStat.
func (p *Profile) Peek(frame int, m Metric) (self FuncStat, callers, callees []Edge) {
	self.Frame = frame
	callerOf, calleeOf := map[int]*Edge{}, map[int]*Edge{}
	add := func(into map[int]*Edge, f int, s recording.ProfileStack) {
		e := into[f]
		if e == nil {
			e = &Edge{Frame: f}
			into[f] = e
		}
		e.Value.add(s)
	}
	for i, s := range p.Stacks {
		at := -1
		for j, f := range s.Frames {
			if f == frame {
				at = j
				break
			}
		}
		if at < 0 {
			continue
		}
		self.Total.add(s)
		next := -1
		if at+1 < len(s.Frames) {
			next = s.Frames[at+1]
		}
		add(callerOf, next, s)
		if at == 0 {
			self.Self.add(s)
			if p.folded != nil && p.folded[i] {
				self.Folded.add(s)
			}
			add(calleeOf, -1, s)
		} else {
			add(calleeOf, s.Frames[at-1], s)
		}
	}
	list := func(from map[int]*Edge) []Edge {
		out := make([]Edge, 0, len(from))
		for _, e := range from {
			out = append(out, *e)
		}
		sort.Slice(out, func(i, j int) bool {
			if a, b := out[i].Value.Get(m), out[j].Value.Get(m); a != b {
				return a > b
			}
			return out[i].Frame < out[j].Frame
		})
		return out
	}
	return self, list(callerOf), list(calleeOf)
}

// HeaviestStack is the stack through function frame with the most of m, and
// how many different call paths go through the function.
func (p *Profile) HeaviestStack(frame int, m Metric) (frames []int, paths int) {
	seen := map[string]bool{}
	best := -1.0
	for _, s := range p.Stacks {
		at := -1
		for j, f := range s.Frames {
			if f == frame {
				at = j
				break
			}
		}
		if at < 0 {
			continue
		}
		seen[fmt.Sprint(s.Frames[at:])] = true
		var v Value
		v.add(s)
		if w := v.Get(m); w > best {
			best = w
			frames = s.Frames[at:]
		}
	}
	return frames, len(seen)
}
