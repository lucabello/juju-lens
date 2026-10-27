package narrative

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/recording"
)

// Span attributes the tap's RSS readings give init, backlog, event,
// observer and root spans: the process's resident memory at the span's start and end,
// and the highest seen within it.
const (
	AttrRSSStart     = "rss_start_bytes"
	AttrRSSEnd       = "rss_end_bytes"
	AttrRSSPeak      = "rss_peak_bytes"
	AttrRSSPeakExact = "rss_hwm_exact" // "true": the peak is the kernel's high-water mark, not a sample
)

// OpsCost is what a span cost the charm, including the spans under it: CPU
// from the stack samples taken in them, and memory from the tap's RSS
// readings, or from the RSS the stack samples read when the tap had none.
type OpsCost struct {
	// Profiled: stack samples were taken in the span's process, so CPUMS
	// and StackMemGrowthBytes are measured, even when they are zero.
	Profiled bool    `json:"profiled"`
	Samples  int     `json:"samples,omitempty"`
	CPUMS    float64 `json:"cpu_ms,omitempty"`
	// StackMemGrowthBytes adds up how much RSS grew from one sample to the
	// next within the span: memory taken, not held.
	StackMemGrowthBytes int64 `json:"stack_mem_growth_bytes,omitempty"`
	// RSSSource says where the RSS figures come from: RSSFromTap, read as
	// the span started and ended, or RSSFromSamples, the first and last
	// stack samples within the span; "" when there are none.
	RSSSource     string `json:"rss_source,omitempty"`
	RSSStartBytes int64  `json:"rss_start_bytes,omitempty"`
	RSSEndBytes   int64  `json:"rss_end_bytes,omitempty"`
	RSSPeakBytes  int64  `json:"rss_peak_bytes,omitempty"`
	RSSPeakExact  bool   `json:"rss_peak_exact,omitempty"`
}

// Sources of OpsCost's RSS figures.
const (
	RSSFromTap     = "tap"
	RSSFromSamples = "samples"
	// RSSFromStacks marks the cost of a function of the charm's code: only
	// its share of the memory the stack samples saw taken.
	RSSFromStacks = "stacks"
)

// RSSGrowth is how much resident memory the process held more at the
// span's end than at its start.
func (c *OpsCost) RSSGrowth() int64 { return c.RSSEndBytes - c.RSSStartBytes }

// costKind reports whether spans of kind get a cost.
func costKind(kind string) bool {
	switch kind {
	case recording.OpsRoot, recording.OpsInit, recording.OpsBacklog, recording.OpsEvent, recording.OpsObserver:
		return true
	}
	return false
}

// ProcessRoots returns the span id of every process in trees: the roots and
// the subprocesses nested in them, whose profiles AttachCosts needs.
func ProcessRoots(trees []*OpsNode) []string {
	var out []string
	var walk func(n *OpsNode)
	walk = func(n *OpsNode) {
		if n.Kind == recording.OpsRoot {
			out = append(out, n.SpanID)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, t := range trees {
		walk(t)
	}
	return out
}

// SubtreeSpans is the span id of n and of every span under it.
func SubtreeSpans(n *OpsNode) map[string]bool {
	out := map[string]bool{}
	var walk func(n *OpsNode)
	walk = func(n *OpsNode) {
		out[n.SpanID] = true
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(n)
	return out
}

// AttachCosts sets the Cost of every root, init, backlog, event and observer span in
// trees that has stack samples (from profs, the profiles of the trees'
// processes) or RSS readings. A span's CPU includes the spans under it,
// subprocesses too, as its wall time does; its RSS is its own process's.
func AttachCosts(trees []*OpsNode, profs []recording.OpsProfile) {
	bySpan := map[string]Value{}
	byRoot := map[string]*recording.OpsProfile{}
	for i, p := range profs {
		byRoot[p.RootSpanID] = &profs[i]
		for _, s := range p.Stacks {
			v := bySpan[s.SpanID]
			v.add(s)
			bySpan[s.SpanID] = v
		}
	}
	var walk func(n *OpsNode, prof *recording.OpsProfile) Value
	walk = func(n *OpsNode, prof *recording.OpsProfile) Value {
		if n.Kind == recording.OpsRoot {
			prof = byRoot[n.SpanID]
		}
		v := bySpan[n.SpanID]
		for _, c := range n.Children {
			cv := walk(c, prof)
			v.Samples += cv.Samples
			v.WallNs += cv.WallNs
			v.CPUNs += cv.CPUNs
			v.MemBytes += cv.MemBytes
		}
		if !costKind(n.Kind) {
			return v
		}
		c := &OpsCost{Profiled: prof != nil}
		if prof != nil {
			c.Samples = v.Samples
			c.CPUMS = float64(time.Duration(v.CPUNs).Microseconds()) / 1000
			c.StackMemGrowthBytes = v.MemBytes
		}
		start, okS := attrInt(n.Attrs, AttrRSSStart)
		end, okE := attrInt(n.Attrs, AttrRSSEnd)
		switch {
		case okS && okE:
			c.RSSSource, c.RSSStartBytes, c.RSSEndBytes = RSSFromTap, start, end
			c.RSSPeakBytes = max(start, end)
			if peak, ok := attrInt(n.Attrs, AttrRSSPeak); ok {
				c.RSSPeakBytes = max(c.RSSPeakBytes, peak)
			}
			c.RSSPeakExact = n.Attrs[AttrRSSPeakExact] == "true"
		case prof != nil:
			sampledRSS(c, prof, n.Start, n.Start.Add(n.Dur))
		}
		if c.Profiled || c.RSSSource != "" {
			n.Cost = c
		}
		if prof != nil && v.Samples > 0 && (n.Kind == recording.OpsInit || n.Kind == recording.OpsObserver) {
			skip := ""
			if n.Kind == recording.OpsObserver {
				skip = ShortObserver(n.Name)
			}
			n.Funcs, n.FuncsRestMS = charmFunctions(NewProfile([]recording.OpsProfile{*prof}, SubtreeSpans(n)), skip)
			// Charm init is almost all the constructor: show it as that.
			if n.Kind == recording.OpsInit && len(n.Funcs) > 0 && strings.HasSuffix(n.Funcs[0].Name, "__init__") &&
				n.Funcs[0].WallMS >= 0.9*float64(n.Dur.Microseconds())/1000 {
				n.Ctor, n.CtorAt, n.CtorMem = n.Funcs[0].Name, n.Funcs[0].At, n.Funcs[0].MemGrowthBytes
				n.Funcs, n.FuncsRestMS = n.Funcs[0].Calls, 0
			}
		}
		return v
	}
	for _, t := range trees {
		walk(t, nil)
	}
}

// sampledRSS fills c's RSS from the stack samples of prof: at the span's
// start the last sample before it (or its first, when the process had none
// before), at its end the last sample within it, and the highest of those
// and every sample between. A span with no sample in it gets none.
func sampledRSS(c *OpsCost, prof *recording.OpsProfile, from, to time.Time) {
	var before, inside []recording.ProfileSample
	for i, s := range prof.Timeline {
		switch {
		case !s.Time.After(from):
			before = prof.Timeline[i : i+1]
		case !s.Time.After(to):
			inside = append(inside, s)
		}
	}
	if len(inside) == 0 {
		return
	}
	start := inside[0].RSSBytes
	if len(before) > 0 {
		start = before[0].RSSBytes
	}
	c.RSSSource, c.RSSStartBytes, c.RSSPeakBytes = RSSFromSamples, start, start
	for _, s := range inside {
		c.RSSPeakBytes = max(c.RSSPeakBytes, s.RSSBytes)
	}
	c.RSSEndBytes = inside[len(inside)-1].RSSBytes
}

func attrInt(a map[string]string, k string) (int64, bool) {
	s, ok := a[k]
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

// OpsCostCells formats a cost as the inspector's and the export's columns:
// CPU, memory growth and peak. Growth is the RSS the span held more at its
// end than at its start. Figures from stack samples rather than the tap's
// own readings are marked "~"; a span shorter than a sample has "-". A peak
// from the kernel's high-water mark is marked "!". Cells are empty for what
// wasn't measured.
func OpsCostCells(c *OpsCost) (cpu, growth, peak string) {
	if c == nil {
		return "", "", ""
	}
	if c.Profiled {
		cpu = "-"
		if c.Samples > 0 {
			cpu = FormatOpsDur(time.Duration(c.CPUMS * float64(time.Millisecond)))
		}
	}
	switch c.RSSSource {
	case RSSFromTap:
		growth = signedBytes(c.RSSGrowth())
		peak = FormatBytes(c.RSSPeakBytes)
		if c.RSSPeakExact {
			peak += "!"
		}
	case RSSFromSamples:
		growth = "~" + signedBytes(c.RSSGrowth())
		peak = "~" + FormatBytes(c.RSSPeakBytes)
	case RSSFromStacks:
		growth = "~" + signedBytes(c.StackMemGrowthBytes)
	default:
		if c.Profiled {
			growth = "-"
		}
	}
	return cpu, growth, peak
}

func signedBytes(n int64) string {
	if n > 0 {
		return "+" + FormatBytes(n)
	}
	if n == 0 {
		return "0"
	}
	return FormatBytes(n)
}

// opsCostFormat lays out the cost columns after the duration.
const opsCostFormat = "  %7s  %8s  %7s"

// OpsHasCost reports whether any line has a cost, so the cost columns are
// worth drawing.
func OpsHasCost(lines []OpsLine) bool {
	for _, l := range lines {
		if l.Cost != nil {
			return true
		}
	}
	return false
}

// OpsCostColumns is a line's cost columns, blank for a line without one.
func OpsCostColumns(l OpsLine) string {
	cpu, growth, peak := OpsCostCells(l.Cost)
	return fmt.Sprintf(opsCostFormat, cpu, growth, peak)
}

// OpsCostHeader names the duration and cost columns, to stand above a tree
// whose labels are w wide.
func OpsCostHeader(w int) string {
	return fmt.Sprintf("%*s  %7s"+opsCostFormat, w, "", "wall", "cpu", "rss Δ", "peak")
}

// FuncCall is a function of the charm's own code that ran while a span was
// open: the time it took with what it called, and the part of that the stack
// samples saw as CPU or as memory taken.
type FuncCall struct {
	Name           string      `json:"name"`
	At             string      `json:"at"` // file:line
	WallMS         float64     `json:"wall_ms"`
	CPUMS          float64     `json:"cpu_ms,omitempty"`
	MemGrowthBytes int64       `json:"mem_growth_bytes,omitempty"`
	Waiting        bool        `json:"mostly_waiting,omitempty"` // little CPU for the time: it was waiting
	WaitingOn      string      `json:"waiting_on,omitempty"`     // for what: "waiting for Pebble (a change to finish)", "waiting for a hook tool", ...
	Calls          []*FuncCall `json:"calls,omitempty"`
}

const (
	// funcMinWall and funcMinShare are how much a function must take of a
	// span to be listed.
	funcMinWall  = 10 * time.Millisecond
	funcMinShare = 0.02
	// funcMaxDepth bounds how deep calls are followed.
	funcMaxDepth = 8
	// funcWaiting is the wall time above which a function with little CPU is
	// called mostly waiting.
	funcWaiting = 100 * time.Millisecond
)

// charmFunctions turns the stacks of one span into the call tree of the
// charm's own functions, heaviest first, leaving out what took little. The
// function named skip, and a module's import at the top, are passed through
// so their callees come first. The second result is the time of the span
// that wasn't in the functions listed.
func charmFunctions(p *Profile, skip string) ([]*FuncCall, float64) {
	cp := p.AtDetail(DetailCharm)
	tree := cp.Tree()
	tree.Sort(MetricWall)
	total := tree.Total.WallNs
	if total <= 0 {
		return nil, 0
	}
	var conv func(n *Node, depth int) []*FuncCall
	conv = func(n *Node, depth int) []*FuncCall {
		var out []*FuncCall
		for _, c := range n.Children {
			fn, loc := cp.FrameLabel(c.Frame)
			wall := time.Duration(c.Total.WallNs)
			switch {
			case fn == skip:
				out = append(out, conv(c, depth)...)
				continue
			case wall < funcMinWall || float64(c.Total.WallNs)/float64(total) < funcMinShare, c.Frame >= len(p.Frames):
				continue
			}
			if c.Frame < len(cp.Frames) {
				fn = FrameName(cp.Frames[c.Frame])
			}
			f := &FuncCall{
				Name: fn, At: loc,
				WallMS:         float64(wall.Microseconds()) / 1000,
				CPUMS:          float64(time.Duration(c.Total.CPUNs).Microseconds()) / 1000,
				MemGrowthBytes: c.Total.MemBytes,
				Waiting:        wall >= funcWaiting && c.Total.CPUNs*10 < c.Total.WallNs,
			}
			if f.Waiting {
				f.WaitingOn = waitingOn(p, c.StackIdx)
			}
			if depth+1 < funcMaxDepth {
				f.Calls = conv(c, depth+1)
			}
			out = append(out, f)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].WallMS > out[j].WallMS })
		return out
	}
	funcs := conv(tree, 0)
	settleWaiting(funcs)
	var listed float64
	for _, f := range funcs {
		listed += f.WallMS
	}
	rest := float64(time.Duration(total).Microseconds())/1000 - listed
	return funcs, max(rest, 0)
}

// callLabel says what a hook tool or Pebble call is, in words.
func callLabel(c *OpsNode) string {
	if c.Kind == recording.OpsPebble {
		return "Pebble call " + pebbleLabel(c)
	}
	return "hook tool " + toolLabel(c)
}

// waitingOn says what a function that used little CPU for its time was
// waiting for: the reason (see blockedOn) of most of its wall time over its
// stacks.
func waitingOn(p *Profile, stacks []int) string {
	byReason := map[string]int64{}
	for _, i := range stacks {
		if i < 0 || i >= len(p.Stacks) {
			continue
		}
		var frames []recording.ProfileFrame
		for _, f := range p.Stacks[i].Frames {
			if f >= 0 && f < len(p.Frames) {
				frames = append(frames, p.Frames[f])
			}
		}
		byReason[blockedOn(frames)] += p.Stacks[i].WallNs
	}
	return heaviestReason(byReason)
}

// heaviestReason is the reason with the most time; ties go to the first
// alphabetically, so the answer doesn't change from run to run.
func heaviestReason(by map[string]int64) string {
	best, bestW := "", int64(-1)
	for r, w := range by {
		if w > bestW || w == bestW && r < best {
			best, bestW = r, w
		}
	}
	return best
}

// blockedOn says, for a stack innermost first, what it was waiting for. The
// reasons are in this order of priority, wherever they are in the stack: a
// Pebble call, a hook tool call, a subprocess, a sleep, the network; failing
// those, the function it was blocked in.
func blockedOn(frames []recording.ProfileFrame) string {
	var pebble, tool, sub, sleep, net bool
	var pebbleWhat string
	for _, f := range frames {
		file := filepath.ToSlash(f.File)
		name := f.Func
		if i := strings.LastIndexByte(name, '.'); i >= 0 && !strings.HasPrefix(name, "<") {
			name = name[i+1:]
		}
		switch {
		case strings.Contains(file, "/ops/pebble.py"):
			pebble = true
			switch {
			case strings.Contains(f.Func, "ExecProcess") || name == "exec" || name == "wait_output":
				pebbleWhat = "a command it ran"
			case pebbleWhat == "" && (strings.Contains(name, "wait_change") || name == "_wait_change"):
				pebbleWhat = "a change to finish"
			}
		case strings.Contains(file, "/ops/hookcmds/"), strings.Contains(file, "/ops/model.py") && name == "_run":
			tool = true
		case strings.HasSuffix(file, "/subprocess.py"):
			sub = true
		case name == "sleep" && strings.HasSuffix(file, "time.py"), name == "sleep":
			sleep = true
		case strings.HasSuffix(file, "/socket.py"), strings.HasSuffix(file, "/ssl.py"), strings.Contains(file, "/http/"),
			strings.Contains(file, "/urllib"), strings.Contains(file, "/requests/"), strings.Contains(file, "/httpx/"):
			net = true
		}
	}
	switch {
	case pebble && pebbleWhat != "":
		return "waiting for Pebble (" + pebbleWhat + ")"
	case pebble:
		return "waiting for Pebble"
	case tool:
		return "waiting for a hook tool"
	case sub:
		return "waiting for a subprocess"
	case sleep:
		return "sleeping"
	case net:
		return "waiting on the network"
	case len(frames) > 0:
		return "blocked in " + FrameName(frames[0]) + " (" + path.Base(filepath.ToSlash(frames[0].File)) + ")"
	}
	return ""
}

// settleWaiting clears the waiting mark from a function whose time is mostly
// that of a function under it that is waiting: the deepest one says it. It
// reports whether the function, or one under it covering most of its time,
// is waiting.
func settleWaiting(fs []*FuncCall) {
	for _, f := range fs {
		settleOne(f)
	}
}

func settleOne(f *FuncCall) bool {
	covered := false
	for _, k := range f.Calls {
		if settleOne(k) && k.WallMS >= 0.6*f.WallMS {
			covered = true
		}
	}
	if covered {
		f.Waiting, f.WaitingOn = false, ""
	}
	return f.Waiting || covered
}
