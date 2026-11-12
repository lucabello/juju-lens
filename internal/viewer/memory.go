package viewer

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/narrative"
)

const (
	// memShapeWidth is how many columns a memory curve has.
	memShapeWidth = 60
)

// memState is the memory view of one hook, read by narrative.LoadMemory, and
// what the view keeps with it.
type memState struct {
	key string
	*narrative.Memory
}

// loadMemory reads what the memory view of ev draws, once per event.
func (m *model) loadMemory(ev event) *memState {
	st := &m.mem
	if st.key == ev.spanID && st.Memory != nil {
		return st
	}
	*st = memState{key: ev.spanID, Memory: &narrative.Memory{}}
	if m.db == nil {
		return st
	}
	mem, err := narrative.LoadMemory(m.db, m.activeModel.ID, m.spans, ev.unit, ev.opsSpans)
	if err == nil && mem != nil {
		st.Memory = mem
	}
	return st
}

// levels are the eighths a curve is drawn in.
var levels = []rune("▁▂▃▄▅▆▇█")

// shape draws a series as one line of memShapeWidth columns scaled from its
// lowest reading to its highest, so a rise shows even when it is a small part
// of the total. A cgroup's value holds from one reading to the next; the
// charm processes' RSS only exists while one ran. It also returns the lowest
// and the highest value.
func (st *memState) shape(s narrative.MemSeries, carry bool, from, to time.Time) (string, int64, int64) {
	n := memShapeWidth
	span := to.Sub(from)
	if len(s.Points) == 0 || span <= 0 {
		return "", 0, 0
	}
	col := func(t time.Time) int { return clamp(int(int64(n)*int64(t.Sub(from))/int64(span)), -1, n) }
	vals := make([]int64, n)
	set := make([]bool, n)
	var last *narrative.MemPoint
	pi := 0
	for c := 0; c < n; c++ {
		for pi < len(s.Points) && col(s.Points[pi].T) <= c {
			p := s.Points[pi]
			if col(p.T) == c || col(p.T) < 0 {
				vals[c], set[c] = max(vals[c], p.Total), true
			}
			last = &s.Points[pi]
			pi++
		}
		if !set[c] && carry && last != nil {
			vals[c], set[c] = last.Total, true
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
	span := st.To.Sub(st.From)
	col := func(t time.Time) int { return clamp(int(int64(n)*int64(t.Sub(st.From))/int64(span)), 0, n-1) }
	for c := col(t0); c <= col(t1); c++ {
		cells[c] = mark
	}
}

// memRow is what happened to one series of memory readings.
type memRow struct {
	name, what          string // the series, and what it is in words
	before, peak, after int64
	shape               string
	lo, hi              int64
	cache               int64 // page cache at the end, when the cgroup says
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
	if st.HookStart.IsZero() || len(st.Series) == 0 && len(st.RSS.Points) == 0 {
		fmt.Fprintf(b, "%s\n", styleDim.Render("no memory readings for this hook: record with --attach unit"))
		return
	}
	fmt.Fprintf(b, "%s  %s\n", styleSection.Render("memory on "+st.Host),
		styleDim.Render(fmt.Sprintf("the hook ran %s → %s UTC (%.1f s); readings from 2 s before it until its workloads settled",
			st.HookStart.UTC().Format("15:04:05.0"), st.HookEnd.UTC().Format("15:04:05.0"), st.HookEnd.Sub(st.HookStart).Seconds())))

	// What each series did.
	var rows []memRow
	if pts := st.RSS.Points; len(pts) > 0 {
		r := memRow{name: "hook process", what: "the charm's Python process while it ran the hook", before: pts[0].Total, after: pts[len(pts)-1].Total}
		for _, p := range pts {
			r.peak = max(r.peak, p.Total)
		}
		rows = append(rows, r)
	}
	var peakNote bool
	for _, s := range st.Series {
		if len(s.Points) == 0 {
			continue
		}
		r := memRow{name: s.Name + " container"}
		if s.Name == "charm" {
			r.what = "the container the charm runs in: its process, files it read, everything else in it"
		} else {
			r.what = "the workload container"
		}
		for _, p := range s.Points {
			if !p.T.After(st.HookStart) {
				r.before = p.Total
			}
			if !p.T.Before(st.HookStart) {
				r.peak = max(r.peak, p.Total)
			}
		}
		if r.before == 0 {
			r.before = s.Points[0].Total
		}
		last := s.Points[len(s.Points)-1]
		r.after = last.Total
		r.peak = max(r.peak, r.before, r.after)
		if s.Name == "charm" && st.Peak > r.peak {
			r.peak, peakNote = st.Peak, true
		}
		if last.Split {
			r.cache = last.File
		}
		r.shape, r.lo, r.hi = st.shape(s, true, st.From, st.To)
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
	for _, h := range st.Hooks {
		if !h.This {
			st.paint(hookLane, h.Start, h.End, '─')
		}
	}
	st.paint(hookLane, st.HookStart, st.HookEnd, '━')
	pebbleLane := []rune(strings.Repeat(" ", memShapeWidth))
	for _, c := range st.Changes {
		st.paint(pebbleLane, c.Spawn, c.Spawn, '▲')
	}
	fmt.Fprintf(b, "\n  %s\n", styleSection.Render("Over time"))
	fmt.Fprintf(b, "  %s\n", styleDim.Render(fmt.Sprintf("%s → %s UTC, left to right. Each line runs from the container's lowest reading to its highest.", st.From.UTC().Format("15:04:05"), st.To.UTC().Format("15:04:05"))))
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
	least := max(1<<20, narrative.MaxTotal(st.RSS.Points)/20)
	if rs := narrative.Rises(st.Window(st.RSS.Points), time.Second, least); len(rs) > 0 {
		fmt.Fprintf(b, "\n  %s\n", styleSection.Render("What took memory in the hook process"))
		fmt.Fprintf(b, "  %s\n", styleDim.Render("the biggest jumps; ← means \"called by\""))
		for _, r := range rs {
			b.WriteString(m.renderRise(st, r))
		}
	}
	for _, s := range st.Series {
		least := max(4<<20, narrative.MaxTotal(s.Points)/10)
		rs := narrative.Rises(st.Window(s.Points), 2*time.Second, least)
		if len(rs) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n  %s\n", styleSection.Render("When the "+s.Name+" container grew"))
		for _, r := range rs {
			fmt.Fprintf(b, "  %s  %s → %s  %s\n", styleWarn.Render(fmt.Sprintf("%8s", "+"+narrative.FormatBytes(r.Size()))),
				narrative.FormatBytes(r.From.Total), narrative.FormatBytes(r.To.Total), styleDim.Render(st.Rel(r.Biggest.T)))
			if what := m.changeAt(st, r.From.T, r.To.T); what != "" {
				fmt.Fprintf(b, "            %s\n", what)
			} else if s.Name == "charm" {
				fmt.Fprintf(b, "            %s\n", styleDim.Render("most likely the hook process above, or files it read"))
			}
		}
	}
	for _, s := range st.Series {
		if e := s.Events; e != nil && e.HighEvents+e.MaxEvents+e.OOMEvents+e.OOMKillEvents > 0 {
			fmt.Fprintf(b, "\n  %s\n", styleErr.Render(fmt.Sprintf("The %s container reached its memory limit %d times, ran out of memory %d times and had a process killed %d times.",
				s.Name, e.HighEvents+e.MaxEvents, e.OOMEvents, e.OOMKillEvents)))
		}
	}
}

// renderRise says one jump in the hook process's memory: how much, when, in
// what part of the hook, and the code that was running, innermost first.
func (m *model) renderRise(st *memState, r narrative.MemRise) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  %s  %s → %s  %s\n", styleWarn.Render(fmt.Sprintf("%8s", "+"+narrative.FormatBytes(r.Size()))),
		narrative.FormatBytes(r.From.Total), narrative.FormatBytes(r.To.Total), styleDim.Render(st.Rel(r.Biggest.T)))
	where, path := m.stackAt(st, r.Biggest)
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
func (m *model) stackAt(st *memState, p narrative.MemPoint) (where, path string) {
	where, frames := st.StackAt(p)
	return where, strings.Join(frames, styleDim.Render(" ← "))
}

// changeAt says what the workload was doing between t0 and t1: the Pebble
// changes running then, or else the last one before.
func (m *model) changeAt(st *memState, t0, t1 time.Time) string {
	c, during, ok := st.ChangeAt(t0, t1)
	if !ok {
		return ""
	}
	s := fmt.Sprintf("%s %s: %s (%s", c.Container, c.Kind, c.Summary, c.Status)
	if !c.Ready.IsZero() {
		s += ", took " + formatDur(c.Ready.Sub(c.Spawn))
	}
	s += ")"
	if c.Err != "" {
		s += " " + c.Err
	}
	if during {
		return styleOK.Render("during Pebble ▲ ") + s
	}
	return styleDim.Render(fmt.Sprintf("last Pebble change %s before: %s", formatDur(t0.Sub(c.Spawn)), s))
}
