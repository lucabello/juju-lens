package viewer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
	"github.com/lucabello/juju-lens/internal/unit"
)

const (
	// memLead is how much of the time before the hook the curves show.
	memLead = 2 * time.Second
	// memDefaultLinger is how long the curves go on after the hook, or after
	// the last Pebble change it caused was ready, for recordings whose
	// executions don't say how long the probe lingered: the workload's
	// memory can rise once the hook is over (a restart after a replan).
	memDefaultLinger = unit.DefaultLinger
	// memBaseline is how often the probe samples cgroups between hooks.
	memBaseline = 5 * time.Second
	// memShapeWidth is how many columns a memory curve has.
	memShapeWidth = 60
	// memMaxRises bounds how many jumps are listed per kind.
	memMaxRises = 5
	// memPathFrames bounds the charm functions shown for a jump.
	memPathFrames = 4
)

// memPoint is one memory reading: a cgroup's or the charm process's.
type memPoint struct {
	t           time.Time
	total       int64
	anon, file  int64
	split       bool
	prof, stack int // the charm process's profile and the stack sampled then; -1 for a cgroup
}

// memSeries is one curve: a container's memory, or the charm processes'
// RSS.
type memSeries struct {
	name   string
	limit  int64
	points []memPoint
	carry  bool // a cgroup: its value holds between samples
	events *index.MemSample
}

// memHook is a hook process on the unit, drawn on the hooks lane.
type memHook struct {
	start, end time.Time
	name       string
	this       bool // a process of the hook the inspector was opened on
}

// memState is the memory view of one hook: the unit's cgroups, the charm
// processes' RSS, the hooks and the Pebble changes around it.
type memState struct {
	key                string
	host               string
	from, to           time.Time // what was read
	hookStart, hookEnd time.Time
	series             []memSeries
	rss                memSeries
	hooks              []memHook
	changes            []index.PebbleChange
	profs              []recording.OpsProfile
	peak               int64 // the charm cgroup's peak during the hook, as the probe measured it; 0 if it didn't
	peakExact          bool  // the peak is exact, not polled between samples
}

// loadMemory reads what the memory view of ev draws, once per event.
func (m *model) loadMemory(ev event) *memState {
	st := &m.mem
	if st.key == ev.spanID {
		return st
	}
	*st = memState{key: ev.spanID}
	if m.db == nil || len(ev.opsSpans) == 0 {
		return st
	}
	for _, id := range ev.opsSpans {
		sp, ok := m.spanByID(id)
		if !ok {
			continue
		}
		if st.hookStart.IsZero() || sp.Start.Before(st.hookStart) {
			st.hookStart = sp.Start
		}
		if sp.End.After(st.hookEnd) {
			st.hookEnd = sp.End
		}
	}
	if st.hookStart.IsZero() {
		return st
	}
	st.host, _ = m.db.SpanHost(ev.opsSpans[0])
	linger := memDefaultLinger
	if root, ok := m.spanByID(ev.opsSpans[0]); ok {
		st.peak, st.peakExact = hookPeak(root.Attrs)
		if ms, err := strconv.ParseInt(root.Attrs[recording.AttrResLingerMs], 10, 64); err == nil && ms > 0 {
			linger = time.Duration(ms) * time.Millisecond
		}
	}
	st.from = st.hookStart.Add(-memLead)
	// The view goes on for the probe's linger time after the hook, or after
	// the last Pebble change it started was ready, whichever is later: the
	// time the probe sampled workloads densely.
	last := st.hookEnd
	caused, _ := m.db.PebbleChanges(m.activeModel.ID, st.host, st.hookStart, st.hookEnd)
	for _, c := range caused {
		if c.Ready.After(last) {
			last = c.Ready
		}
	}
	st.to = last.Add(linger)
	// From two baseline sampling periods before, so the curves start at the
	// left edge rather than at the first reading in the window.
	samples, _ := m.db.MemorySamples(m.activeModel.ID, st.host, st.from.Add(-2*memBaseline), st.to)
	st.changes, _ = m.db.PebbleChanges(m.activeModel.ID, st.host, st.from, st.to)

	// End at the last reading when the recording stops before the linger
	// does, so the plot doesn't trail off into nothing.
	var lastSample time.Time
	byName := map[string]*memSeries{}
	var names []string
	for _, s := range samples {
		ser := byName[s.Container]
		if ser == nil {
			ser = &memSeries{name: s.Container, carry: true}
			byName[s.Container] = ser
			names = append(names, s.Container)
		}
		ser.points = append(ser.points, memPoint{t: s.TS, total: s.MemBytes, anon: s.Anon, file: s.File, split: s.Split, prof: -1, stack: -1})
		if s.MaxBytes > 0 {
			ser.limit = s.MaxBytes
		}
		if s.Events {
			e := s
			ser.events = &e
		}
		if s.TS.After(lastSample) {
			lastSample = s.TS
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return names[i] == "charm" && names[j] != "charm" })
	for _, n := range names {
		st.series = append(st.series, *byName[n])
	}

	// The unit's hook processes in the window, and their RSS.
	var roots []string
	for _, sp := range m.spans {
		if sp.Service != recording.ServiceOps || sp.ParentSpanID != "" || sp.Unit != ev.unit ||
			sp.Attrs["kind"] != recording.OpsRoot || sp.End.Before(st.from) || sp.Start.After(st.to) {
			continue
		}
		this := false
		for _, id := range ev.opsSpans {
			this = this || id == sp.SpanID
		}
		name := sp.Name
		if name == "" {
			name = "process"
		}
		st.hooks = append(st.hooks, memHook{start: sp.Start, end: sp.End, name: name, this: this})
		roots = append(roots, sp.SpanID)
	}
	st.profs, _ = m.db.Profiles(roots)
	st.rss = memSeries{name: "charm process RSS"}
	for pi, p := range st.profs {
		for _, s := range p.Timeline {
			st.rss.points = append(st.rss.points, memPoint{t: s.Time, total: s.RSSBytes, prof: pi, stack: s.Stack})
			if s.Time.After(lastSample) {
				lastSample = s.Time
			}
		}
	}
	sort.SliceStable(st.rss.points, func(i, j int) bool { return st.rss.points[i].t.Before(st.rss.points[j].t) })
	if end := latest(lastSample, st.hookEnd).Add(time.Second); end.Before(st.to) {
		st.to = end
	}
	return st
}

// hookPeak is the charm cgroup's memory peak during the hook, as the probe
// measured it, and whether it is exact (memory.peak reset at the hook's start,
// or a new lifetime high) or polled between samples. 0 when not measured.
func hookPeak(a map[string]string) (int64, bool) {
	v, err := strconv.ParseInt(a["res_cgroup_mem_peak_bytes"], 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, a["res_cgroup_mem_peak_exact"] == "true"
}

// memRise is a stretch over which a reading only went up.
type memRise struct {
	from, to   memPoint // the readings it began and ended at
	biggest    memPoint // the reading the largest single step ended at
	hasBigStep bool
}

func (r memRise) size() int64 { return r.to.total - r.from.total }

// rises finds the stretches of pts in which memory only went up, with
// readings no more than gap apart, that gained at least least bytes,
// biggest first. A reading of another process, or a fall, ends a stretch.
func rises(pts []memPoint, gap time.Duration, least int64) []memRise {
	var out []memRise
	var cur *memRise
	var bestStep int64
	flush := func() {
		if cur != nil && cur.size() >= least {
			out = append(out, *cur)
		}
		cur, bestStep = nil, 0
	}
	for i := 1; i < len(pts); i++ {
		prev, p := pts[i-1], pts[i]
		step := p.total - prev.total
		if step <= 0 || p.prof != prev.prof || p.t.Sub(prev.t) > gap {
			flush()
			continue
		}
		if cur == nil {
			cur = &memRise{from: prev, to: p, biggest: p}
		}
		cur.to = p
		if step > bestStep {
			bestStep, cur.biggest = step, p
		}
	}
	flush()
	sort.SliceStable(out, func(i, j int) bool { return out[i].size() > out[j].size() })
	if len(out) > memMaxRises {
		out = out[:memMaxRises]
	}
	return out
}

// levels are the eighths a curve is drawn in.
var levels = []rune("▁▂▃▄▅▆▇█")

// shape draws a series as one line of memShapeWidth columns scaled from its
// lowest reading to its highest, so a rise shows even when it is a small part
// of the total. A cgroup's value holds from one reading to the next; the
// charm processes' RSS only exists while one ran. It also returns the lowest
// and the highest value.
func (st *memState) shape(s memSeries, from, to time.Time) (string, int64, int64) {
	n := memShapeWidth
	span := to.Sub(from)
	if len(s.points) == 0 || span <= 0 {
		return "", 0, 0
	}
	col := func(t time.Time) int { return clamp(int(int64(n)*int64(t.Sub(from))/int64(span)), -1, n) }
	vals := make([]int64, n)
	set := make([]bool, n)
	var last *memPoint
	pi := 0
	for c := 0; c < n; c++ {
		for pi < len(s.points) && col(s.points[pi].t) <= c {
			p := s.points[pi]
			if col(p.t) == c || col(p.t) < 0 {
				vals[c], set[c] = max(vals[c], p.total), true
			}
			last = &s.points[pi]
			pi++
		}
		if !set[c] && s.carry && last != nil {
			vals[c], set[c] = last.total, true
		}
	}
	lo, hi := int64(math.MaxInt64), int64(0)
	for c := range vals {
		if set[c] {
			lo, hi = min(lo, vals[c]), max(hi, vals[c])
		}
	}
	if hi == 0 && lo == math.MaxInt64 {
		return "", 0, 0
	}
	var b strings.Builder
	for c := range vals {
		switch {
		case !set[c]:
			b.WriteByte(' ')
		case hi == lo:
			b.WriteRune(levels[3])
		default:
			b.WriteRune(levels[int(7*(vals[c]-lo)/(hi-lo))])
		}
	}
	return b.String(), lo, hi
}

// paint draws mark over the columns of cells that t0 to t1 covers.
func (st *memState) paint(cells []rune, t0, t1 time.Time, mark rune) {
	n := len(cells)
	span := st.to.Sub(st.from)
	col := func(t time.Time) int { return clamp(int(int64(n)*int64(t.Sub(st.from))/int64(span)), 0, n-1) }
	for c := col(t0); c <= col(t1); c++ {
		cells[c] = mark
	}
}

// window is the readings from the window the view covers.
func (st *memState) window(pts []memPoint) []memPoint {
	var out []memPoint
	for _, p := range pts {
		if !p.t.Before(st.from) && !p.t.After(st.to) {
			out = append(out, p)
		}
	}
	return out
}

// memRow is what happened to one series of memory readings.
type memRow struct {
	name, what          string // the series, and what it is in words
	before, peak, after int64
	shape               string
	lo, hi              int64
	cache               int64 // page cache at the end, when the cgroup says
}

// rel says when t was relative to the hook: "2.8 s before the hook", "1.2 s
// into the hook", "4.0 s after the hook ended".
func (st *memState) rel(t time.Time) string {
	d := func(x time.Duration) string { return fmt.Sprintf("%.1f s", x.Seconds()) }
	switch {
	case t.Before(st.hookStart):
		return d(st.hookStart.Sub(t)) + " before the hook"
	case t.After(st.hookEnd):
		return d(t.Sub(st.hookEnd)) + " after the hook ended"
	}
	return d(t.Sub(st.hookStart)) + " into the hook"
}

// changeWords says how a series moved, in a sentence.
func changeWords(r memRow) string {
	diff := r.after - r.before
	switch {
	case absInt(diff) < 1<<20:
		return fmt.Sprintf("stayed around %s", narrative.FormatBytes(r.before))
	case diff > 0:
		return fmt.Sprintf("grew from %s to %s (+%s)", narrative.FormatBytes(r.before), narrative.FormatBytes(r.after), narrative.FormatBytes(diff))
	case diff < 0:
		return fmt.Sprintf("fell from %s to %s (-%s)", narrative.FormatBytes(r.before), narrative.FormatBytes(r.after), narrative.FormatBytes(-diff))
	}
	return fmt.Sprintf("ended where it began, at %s", narrative.FormatBytes(r.after))
}

func absInt(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// renderMemoryView says what happened to memory around the hook the
// inspector shows, in this order: a sentence for each of the hook process and
// the containers; a table of where each started, how high it went and where it
// ended; the curves of the containers with the hook and the Pebble changes
// under them; and the biggest jumps, with what was running then.
func (m *model) renderMemoryView(b *strings.Builder, ev event) {
	st := m.loadMemory(ev)
	if st.hookStart.IsZero() || len(st.series) == 0 && len(st.rss.points) == 0 {
		fmt.Fprintf(b, "%s\n", styleDim.Render("no memory readings for this hook: record with --attach unit"))
		return
	}
	fmt.Fprintf(b, "%s  %s\n", styleSection.Render("memory on "+st.host),
		styleDim.Render(fmt.Sprintf("the hook ran %s → %s UTC (%.1f s); readings from 2 s before it until its workloads settled",
			st.hookStart.UTC().Format("15:04:05.0"), st.hookEnd.UTC().Format("15:04:05.0"), st.hookEnd.Sub(st.hookStart).Seconds())))

	// What each series did.
	var rows []memRow
	if pts := st.rss.points; len(pts) > 0 {
		r := memRow{name: "hook process", what: "the charm's Python process while it ran the hook", before: pts[0].total, after: pts[len(pts)-1].total}
		for _, p := range pts {
			r.peak = max(r.peak, p.total)
		}
		rows = append(rows, r)
	}
	var peakNote bool
	for _, s := range st.series {
		if len(s.points) == 0 {
			continue
		}
		r := memRow{name: s.name + " container"}
		if s.name == "charm" {
			r.what = "the container the charm runs in: its process, files it read, everything else in it"
		} else {
			r.what = "the workload container"
		}
		for _, p := range s.points {
			if !p.t.After(st.hookStart) {
				r.before = p.total
			}
			if !p.t.Before(st.hookStart) {
				r.peak = max(r.peak, p.total)
			}
		}
		if r.before == 0 {
			r.before = s.points[0].total
		}
		last := s.points[len(s.points)-1]
		r.after = last.total
		r.peak = max(r.peak, r.before, r.after)
		if s.name == "charm" && st.peak > r.peak {
			r.peak, peakNote = st.peak, true
		}
		if last.split {
			r.cache = last.file
		}
		r.shape, r.lo, r.hi = st.shape(s, st.from, st.to)
		rows = append(rows, r)
	}

	fmt.Fprintf(b, "\n  %s\n", styleSection.Render("What happened"))
	for _, r := range rows {
		fmt.Fprintf(b, "  • %s %s", styleHook.Render("The "+r.name), changeWords(r))
		if r.peak-max(r.before, r.after) >= 4<<20 {
			fmt.Fprintf(b, ", and was as high as %s in between", narrative.FormatBytes(r.peak))
		}
		b.WriteString(".\n")
	}

	fmt.Fprintf(b, "\n  %s\n", styleDim.Render(fmt.Sprintf("%-26s %8s %8s %8s", "", "before", "highest", "after")))
	for _, r := range rows {
		fmt.Fprintf(b, "  %-26s %8s %8s %8s\n", r.name, narrative.FormatBytes(r.before), narrative.FormatBytes(r.peak), narrative.FormatBytes(r.after))
	}
	fmt.Fprintf(b, "  %s\n", styleDim.Render("hook process: only the charm's Python process. Containers: everything in them, with the file cache the kernel keeps and can take back."))
	if peakNote {
		fmt.Fprintf(b, "  %s\n", styleDim.Render("charm container, highest: the probe's own record of the peak, which can be higher than any single reading."))
	}
	for _, r := range rows {
		if r.cache > 0 {
			fmt.Fprintf(b, "  %s\n", styleDim.Render(fmt.Sprintf("%s: %s of its memory at the end is file cache.", r.name, narrative.FormatBytes(r.cache))))
		}
	}

	// The shape of each container over the whole window.
	hookLane := []rune(strings.Repeat(" ", memShapeWidth))
	for _, h := range st.hooks {
		if !h.this {
			st.paint(hookLane, h.start, h.end, '─')
		}
	}
	st.paint(hookLane, st.hookStart, st.hookEnd, '━')
	pebbleLane := []rune(strings.Repeat(" ", memShapeWidth))
	for _, c := range st.changes {
		st.paint(pebbleLane, c.Spawn, c.Spawn, '▲')
	}
	fmt.Fprintf(b, "\n  %s\n", styleSection.Render("Over time"))
	fmt.Fprintf(b, "  %s\n", styleDim.Render(fmt.Sprintf("%s → %s UTC, left to right. Each line runs from the container's lowest reading to its highest.", st.from.UTC().Format("15:04:05"), st.to.UTC().Format("15:04:05"))))
	for _, r := range rows {
		if r.shape == "" || r.name == "hook process" {
			continue
		}
		fmt.Fprintf(b, "  %-26s %s  %s\n", r.name, styleHook.Render(r.shape), styleDim.Render(narrative.FormatBytes(r.lo)+" to "+narrative.FormatBytes(r.hi)))
	}
	fmt.Fprintf(b, "  %-26s %s  %s\n", "this hook ━  other hooks ─", styleOK.Render(string(hookLane)), "")
	fmt.Fprintf(b, "  %-26s %s  %s\n", "Pebble change started ▲", styleWarn.Render(string(pebbleLane)), "")
	fmt.Fprintf(b, "  %s\n", styleDim.Render("To see the hook process's memory along the hook, next to what ran, press t."))

	// What took memory in the hook.
	least := max(1<<20, maxPoint(st.rss.points)/20)
	if rs := rises(st.window(st.rss.points), time.Second, least); len(rs) > 0 {
		fmt.Fprintf(b, "\n  %s\n", styleSection.Render("What took memory in the hook process"))
		fmt.Fprintf(b, "  %s\n", styleDim.Render("the biggest jumps; ← means \"called by\""))
		for _, r := range rs {
			b.WriteString(m.renderRise(st, r))
		}
	}
	for _, s := range st.series {
		least := max(4<<20, maxPoint(s.points)/10)
		rs := rises(st.window(s.points), 2*time.Second, least)
		if len(rs) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n  %s\n", styleSection.Render("When the "+s.name+" container grew"))
		for _, r := range rs {
			fmt.Fprintf(b, "  %s  %s → %s  %s\n", styleWarn.Render(fmt.Sprintf("%8s", "+"+narrative.FormatBytes(r.size()))),
				narrative.FormatBytes(r.from.total), narrative.FormatBytes(r.to.total), styleDim.Render(st.rel(r.biggest.t)))
			if what := m.changeAt(st, r.from.t, r.to.t); what != "" {
				fmt.Fprintf(b, "            %s\n", what)
			} else if s.name == "charm" {
				fmt.Fprintf(b, "            %s\n", styleDim.Render("most likely the hook process above, or files it read"))
			}
		}
	}
	for _, s := range st.series {
		if e := s.events; e != nil && e.HighEvents+e.MaxEvents+e.OOMEvents+e.OOMKillEvents > 0 {
			fmt.Fprintf(b, "\n  %s\n", styleErr.Render(fmt.Sprintf("The %s container reached its memory limit %d times, ran out of memory %d times and had a process killed %d times.",
				s.name, e.HighEvents+e.MaxEvents, e.OOMEvents, e.OOMKillEvents)))
		}
	}
}

func maxPoint(pts []memPoint) int64 {
	var v int64
	for _, p := range pts {
		v = max(v, p.total)
	}
	return v
}

// renderRise says one jump in the hook process's memory: how much, when, in
// what part of the hook, and the code that was running, innermost first.
func (m *model) renderRise(st *memState, r memRise) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s  %s → %s  %s\n", styleWarn.Render(fmt.Sprintf("%8s", "+"+narrative.FormatBytes(r.size()))),
		narrative.FormatBytes(r.from.total), narrative.FormatBytes(r.to.total), styleDim.Render(st.rel(r.biggest.t)))
	where, path := m.stackAt(st, r.biggest)
	if where != "" {
		fmt.Fprintf(&b, "            %s %s\n", styleDim.Render("during"), styleHook.Render(where))
	}
	if path != "" && !strings.HasPrefix(where, "the process starting up") {
		fmt.Fprintf(&b, "            %s %s\n", styleDim.Render("code:"), path)
	}
	return b.String()
}

// stackAt is where the charm was at a memory reading of its process: the
// span it ran in, and its stack: the innermost frame, then the frames of the
// charm's code that called it, innermost first.
func (m *model) stackAt(st *memState, p memPoint) (where, path string) {
	if p.prof < 0 || p.prof >= len(st.profs) {
		return "", ""
	}
	prof := st.profs[p.prof]
	if p.stack < 0 || p.stack >= len(prof.Stacks) {
		return "", ""
	}
	stack := prof.Stacks[p.stack]
	if sp, ok := m.spanByID(stack.SpanID); ok {
		switch sp.Attrs["kind"] {
		case recording.OpsObserver:
			where = "observer " + narrative.ShortObserver(sp.Name)
		case recording.OpsInit:
			where = "the charm's __init__"
		case recording.OpsRoot:
			where = "the process starting up, before ops (importing Python modules)"
		default:
			where = sp.Name
		}
	}
	var names []string
	charm := 0
	for i, f := range stack.Frames {
		if f < 0 || f >= len(prof.Frames) {
			continue
		}
		isCharm := narrative.IsCharmFile(prof.Frames[f].File)
		if narrative.IsNoiseFrame(prof.Frames[f]) || i > 0 && !isCharm {
			continue
		}
		if i > 0 {
			if charm++; charm > memPathFrames {
				break
			}
		}
		fn := narrative.FrameName(prof.Frames[f])
		if len(names) > 0 && names[len(names)-1] == fn {
			continue
		}
		if isCharm {
			fn = styleApp.Render(fn)
		}
		names = append(names, fn)
	}
	return where, strings.Join(names, styleDim.Render(" ← "))
}

// changeAt says what the workload was doing between t0 and t1: the Pebble
// changes running then, or else the last one before.
func (m *model) changeAt(st *memState, t0, t1 time.Time) string {
	var running []index.PebbleChange
	var before *index.PebbleChange
	for i, c := range st.changes {
		if c.Spawn.Before(t1) && (c.Ready.IsZero() || !c.Ready.Before(t0)) {
			running = append(running, c)
		}
		if c.Spawn.Before(t0) {
			before = &st.changes[i]
		}
	}
	desc := func(c index.PebbleChange) string {
		s := fmt.Sprintf("%s %s: %s (%s", c.Container, c.Kind, c.Summary, c.Status)
		if !c.Ready.IsZero() {
			s += ", took " + formatDur(c.Ready.Sub(c.Spawn))
		}
		s += ")"
		if c.Err != "" {
			s += " " + c.Err
		}
		return s
	}
	switch {
	case len(running) > 0:
		return styleOK.Render("during Pebble ▲ ") + desc(running[0])
	case before != nil:
		return styleDim.Render(fmt.Sprintf("last Pebble change %s before: %s", formatDur(t0.Sub(before.Spawn)), desc(*before)))
	}
	return ""
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
