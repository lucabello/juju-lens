package viewer

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// traceState is what the trace view remembers between keys.
type traceState struct {
	key       string          // event the state is for
	attempt   int             // which process of a retried hook
	lvl       int             // index into codeCycle: how much to show; everything also lists every short call
	level     int             // how many levels of rows are open: 0 for all, -1 if folded by hand
	collapsed map[string]bool // rows whose children are folded away, by row key
	sel       string          // the selected row's key
	selRow    int             // the selected row's line in the last render, from the top of the body
}

// traceData is the trace of the selected process, with the rows that are
// drawn: the ones under no folded row, and in the time range shown.
type traceData struct {
	tr       *narrative.Trace
	from, to time.Time
	vis      []int // indexes into tr.Rows
	sel      int   // index into vis
	attempts int
	hook     string
}

// traceBuild builds the trace of ev for the current state.
func (m *model) traceBuild(ev event) (*traceData, bool) {
	trees, profs := m.hookTrees(ev)
	if len(trees) == 0 {
		return nil, false
	}
	st := &m.trace
	if st.key != ev.spanID {
		*st = traceState{key: ev.spanID}
	}
	st.attempt = clamp(st.attempt, 0, len(trees)-1)
	d := &traceData{attempts: len(trees), hook: trees[st.attempt].Name}
	d.tr = narrative.BuildTrace(trees[st.attempt], profs, narrative.TraceOptions{Detail: codeCycle[st.lvl], All: codeCycle[st.lvl] == narrative.DetailAll})
	d.from, d.to = d.tr.From, d.tr.To
	if !d.to.After(d.from) {
		d.to = d.from.Add(time.Millisecond)
	}
	hideBelow := -1
	for i, r := range d.tr.Rows {
		if hideBelow >= 0 && r.Depth > hideBelow {
			continue
		}
		hideBelow = -1
		if st.collapsed[r.Key] && r.HasKids {
			hideBelow = r.Depth
		}
		if r.End.Before(d.from) || r.Start.After(d.to) {
			continue
		}
		d.vis = append(d.vis, i)
	}
	d.sel = 0
	for i, ri := range d.vis {
		if d.tr.Rows[ri].Key == st.sel {
			d.sel = i
		}
	}
	if len(d.vis) > 0 {
		st.sel = d.tr.Rows[d.vis[d.sel]].Key
	}
	return d, true
}

// handleTraceKey applies a trace view key; it reports whether the key was
// one, so other keys fall through to scrolling.
func (m *model) handleTraceKey(msg tea.KeyMsg) bool {
	st := &m.trace
	d, ok := m.traceBuild(m.currentEvent())
	if !ok {
		return false
	}
	move := func(i int) {
		if len(d.vis) > 0 {
			st.sel = d.tr.Rows[d.vis[clamp(i, 0, len(d.vis)-1)]].Key
		}
	}
	var cur narrative.TraceRow
	if len(d.vis) > 0 {
		cur = d.tr.Rows[d.vis[d.sel]]
	}
	switch {
	case key.Matches(msg, m.keys.Down):
		move(d.sel + 1)
	case key.Matches(msg, m.keys.Up):
		move(d.sel - 1)
	case key.Matches(msg, m.keys.ScrollRight):
		delete(st.collapsed, cur.Key)
		st.level = -1
	case msg.String() == " ":
		if cur.HasKids {
			if st.collapsed == nil {
				st.collapsed = map[string]bool{}
			}
			st.collapsed[cur.Key] = !st.collapsed[cur.Key]
			st.level = -1
		}
	case key.Matches(msg, m.keys.ScrollLeft):
		if cur.HasKids && !st.collapsed[cur.Key] {
			if st.collapsed == nil {
				st.collapsed = map[string]bool{}
			}
			st.collapsed[cur.Key] = true
			st.level = -1
			break
		}
		for i := d.sel - 1; i >= 0; i-- { // out to the row it is under
			if d.tr.Rows[d.vis[i]].Depth < cur.Depth {
				move(i)
				break
			}
		}
	case key.Matches(msg, m.keys.Inspect):
		if cur.HasKids {
			if st.collapsed == nil {
				st.collapsed = map[string]bool{}
			}
			st.collapsed[cur.Key] = !st.collapsed[cur.Key]
			st.level = -1
		}
	case msg.String() == ".":
		st.lvl = (st.lvl + 1) % len(codeCycle)
		if st.level > 0 {
			d, _ = m.traceBuild(m.currentEvent())
			m.traceOpen(d, st.level)
		}
	case len(msg.String()) == 1 && msg.String() >= "1" && msg.String() <= "9":
		m.traceOpen(d, int(msg.String()[0]-'0'))
	case msg.String() == "+" || msg.String() == "=":
		m.traceRelative(d, +1)
	case msg.String() == "-":
		m.traceRelative(d, -1)
	case msg.String() == "tab", msg.String() == "shift+tab":
		if d.attempts > 1 {
			st.attempt = (st.attempt + 1) % d.attempts
			st.sel = ""
		}
	default:
		return false
	}
	return true
}

// levels is how many levels of rows the trace has.
func (d *traceData) levels() int {
	n := 1
	for _, r := range d.tr.Rows {
		n = max(n, r.Depth+1)
	}
	return n
}

// traceOpen opens n levels of rows: every row above level n is unfolded, and
// the rows at level n are folded, so their children are hidden. At or past the
// deepest level, every row is open. If the selected row is hidden by it, the
// row it is under at level n is selected.
func (m *model) traceOpen(d *traceData, n int) {
	st := &m.trace
	n = max(n, 1)
	st.collapsed = nil
	st.level = n
	if n >= d.levels() {
		st.level = 0
		return
	}
	st.collapsed = map[string]bool{}
	sel := -1
	for i, r := range d.tr.Rows {
		if r.Key == st.sel {
			sel = i
		}
		if r.Depth == n-1 && r.HasKids {
			st.collapsed[r.Key] = true
		}
	}
	for i := sel; i >= 0 && d.tr.Rows[sel].Depth > n-1; i-- {
		if d.tr.Rows[i].Depth == n-1 {
			st.sel = d.tr.Rows[i].Key
			break
		}
	}
}

// traceRelative opens (delta 1) or closes (delta -1) one level of the rows
// under the selected row: opening unfolds the shallowest folded rows under it,
// closing folds the open rows just above the deepest level under it, or, for a
// row with nothing open under it, the row it is under. The rest of the trace
// stays as it is.
func (m *model) traceRelative(d *traceData, delta int) {
	st := &m.trace
	if len(d.vis) == 0 {
		return
	}
	rows := d.tr.Rows
	i := d.vis[d.sel]
	root := rows[i]
	var sub []int // the rows under the selected one that are drawn, and it
	hide := -1
	for j := i; j < len(rows); j++ {
		if j > i && rows[j].Depth <= root.Depth {
			break
		}
		if hide >= 0 && rows[j].Depth > hide {
			continue
		}
		hide = -1
		sub = append(sub, j)
		if st.collapsed[rows[j].Key] && rows[j].HasKids {
			hide = rows[j].Depth
		}
	}
	if st.collapsed == nil {
		st.collapsed = map[string]bool{}
	}
	folded := func(j int) bool { return rows[j].HasKids && st.collapsed[rows[j].Key] }
	switch delta {
	case 1:
		shallowest := -1
		for _, j := range sub {
			if folded(j) && (shallowest < 0 || rows[j].Depth < shallowest) {
				shallowest = rows[j].Depth
			}
		}
		for _, j := range sub {
			if folded(j) && rows[j].Depth == shallowest {
				delete(st.collapsed, rows[j].Key)
			}
		}
	default:
		deepest := root.Depth
		for _, j := range sub {
			deepest = max(deepest, rows[j].Depth)
		}
		if deepest == root.Depth {
			// Nothing is open under the row, so close the section it is in: fold
			// the row it is under, and select that.
			for k := d.sel - 1; k >= 0; k-- {
				if parent := rows[d.vis[k]]; parent.Depth < root.Depth {
					st.collapsed[parent.Key] = true
					st.sel = parent.Key
					break
				}
			}
			st.level = -1
			return
		}
		for _, j := range sub {
			if rows[j].HasKids && !folded(j) && rows[j].Depth == deepest-1 {
				st.collapsed[rows[j].Key] = true
			}
		}
	}
	st.level = -1
}

// traceHelp is the keybinding line of the trace view.
func (m *model) traceHelp() string {
	help := "esc close · ↑/↓ select · ←/→/enter fold · 1-9 open that many levels · +/- one level more or less under the row · . more or less · p profile · m memory"
	if hasChanges(m.currentEvent()) {
		help += " · d changes"
	}
	return help
}

// traceColor is the colour of a bar.
func traceColor(r narrative.TraceRow) lipgloss.Color {
	switch {
	case r.Failed:
		return lipgloss.Color("160")
	case r.Kind == narrative.TraceFunc || r.Class == narrative.ClassCharm:
		switch r.Class {
		case narrative.ClassCharm:
			return lipgloss.Color("173")
		case narrative.ClassOps:
			return lipgloss.Color("71")
		}
		return lipgloss.Color("67")
	case r.Kind == recording.OpsPebble:
		return lipgloss.Color("178")
	case r.Kind == recording.OpsTool:
		return lipgloss.Color("103")
	case r.Kind == recording.OpsRoot:
		return lipgloss.Color("244")
	}
	return lipgloss.Color("38")
}

// traceTicks puts a time scale over barW columns covering from to to: labels
// at round offsets from the start of the process, and marks under them.
func traceTicks(from, to, origin time.Time, barW int) (labels, marks string) {
	span := to.Sub(from)
	step := time.Millisecond
	// The smallest 1, 2 or 5 times a power of ten that gives at most about
	// one tick for every 12 columns.
	for _, unit := range []time.Duration{time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond, time.Second, 10 * time.Second, time.Minute, 10 * time.Minute} {
		found := false
		for _, mult := range []time.Duration{1, 2, 5} {
			if cand := unit * mult; int(span/cand) <= max(1, barW/12) {
				step, found = cand, true
				break
			}
		}
		if found {
			break
		}
	}
	l := []rune(strings.Repeat(" ", barW))
	k := []rune(strings.Repeat("─", barW))
	first := from.Sub(origin)
	t := (first + step - 1) / step * step
	lastEnd := -1
	for ; origin.Add(t).Before(to) || origin.Add(t).Equal(to); t += step {
		col := int(float64(barW) * float64(origin.Add(t).Sub(from)) / float64(span))
		if col < 0 || col >= barW {
			continue
		}
		k[col] = '┬'
		label := []rune("+" + narrative.FormatOpsDur(t))
		if t == 0 {
			label = []rune("0")
		}
		if col > lastEnd && col+len(label) <= barW {
			copy(l[col:], label)
			lastEnd = col + len(label)
		}
	}
	return string(l), string(k)
}

// renderTraceView draws the hook as a trace: a bar for each span, call and
// function of the charm's code, along a common time axis, nested as they ran,
// with the process's memory over the same time on top. ↑/↓ select a row.
func (m *model) renderTraceView(b *strings.Builder, ev event) {
	st := &m.trace
	d, ok := m.traceBuild(ev)
	if !ok {
		fmt.Fprintf(b, "%s\n", styleDim.Render("no charm-side trace for this hook: record with --attach unit"))
		return
	}
	title := "trace of " + d.hook
	if d.attempts > 1 {
		title += fmt.Sprintf(" · attempt %d of %d (tab)", st.attempt+1, d.attempts)
	}
	fmt.Fprintf(b, "%s  %s\n", styleSection.Render(title), styleDim.Render(d.tr.To.Sub(d.tr.From).Round(time.Millisecond).String()+" in all"))
	lvl := codeCycle[st.lvl]
	hidden := ""
	if lvl != narrative.DetailAll {
		hidden = " · hiding short calls and functions and imports"
		if d.tr.ImportMS >= 20 {
			hidden = fmt.Sprintf(" · hiding short calls and functions, and %s of importing Python modules before ops started", narrative.FormatOpsDur(time.Duration(d.tr.ImportMS*float64(time.Millisecond))))
		}
	}
	fmt.Fprintf(b, "  showing %s%s %s\n", styleHook.Render(levelLabel(lvl)), hidden, styleDim.Render("("+levelHelp(st.lvl)+")"))
	opened := "all levels"
	switch {
	case st.level > 0:
		opened = fmt.Sprintf("%d of %d levels", st.level, d.levels())
	case st.level < 0:
		opened = "as you folded it"
	}
	fmt.Fprintf(b, "  open: %s %s\n", styleHook.Render(opened), styleDim.Render("(1-9 set the levels; + and - open or close a level under the selected row)"))
	width := max(60, m.width-6)
	labelW := clamp(width*4/10, 28, 54)
	barW := width - labelW - 18
	span := d.to.Sub(d.from)
	col := func(t time.Time) int {
		return clamp(int(math.Round(float64(barW)*float64(t.Sub(d.from))/float64(span))), 0, barW)
	}

	labels, marks := traceTicks(d.from, d.to, d.tr.From, barW)
	pad := strings.Repeat(" ", labelW)
	b.WriteByte('\n')
	fmt.Fprintf(b, "  %s %s %s\n", pad, styleDim.Render(labels), styleDim.Render(fmt.Sprintf("%8s %7s", "time", "mem Δ")))
	fmt.Fprintf(b, "  %s %s\n", pad, styleDim.Render(marks))
	if mem := traceMemory(d, barW); mem != "" {
		fmt.Fprintf(b, "  %s %s\n", styleDim.Render(fmt.Sprintf("%-*s", labelW, "memory of the hook process")), styleHook.Render(mem))
	}

	base := strings.Count(b.String(), "\n")
	for i, ri := range d.vis {
		r := d.tr.Rows[ri]
		mark := "  "
		if r.HasKids {
			mark = "▾ "
			if st.collapsed[r.Key] {
				mark = "▸ "
			}
		}
		name := strings.Repeat("  ", min(r.Depth, 10)) + mark + r.Label
		name = ansi.Truncate(name, labelW, "…")
		loc := ""
		if r.Loc != "" && i != d.sel {
			if room := labelW - ansi.StringWidth(name) - 2; room >= len(r.Loc) {
				loc = "  " + r.Loc
			}
		}
		pad := strings.Repeat(" ", max(0, labelW-ansi.StringWidth(name)-len(loc)))
		c0, c1 := col(r.Start), col(r.End)
		n := max(1, c1-c0)
		c0 = min(c0, barW-n)
		block := "█"
		if c1-c0 < 1 {
			block = "▏"
		}
		bar := strings.Repeat(" ", c0) + strings.Repeat(block, n) + strings.Repeat(" ", max(0, barW-c0-n))
		sty := lipgloss.NewStyle().Foreground(traceColor(r))
		dur := durCell(r.Dur())
		memCol := fmt.Sprintf("%7s", traceMem(r))
		var line string
		if i == d.sel {
			st.selRow = base + i
			line = styleSelected.Render(name+pad+strings.Repeat(" ", len(loc))) + " " + sty.Render(bar) + " " + dur + " " + memCol
		} else {
			line = traceLabelStyle(r).Render(name) + styleDim.Render(loc) + pad + " " + sty.Render(bar) + " " + dur + " " + styleDim.Render(memCol)
		}
		fmt.Fprintf(b, "  %s\n", line)
	}
	if len(d.vis) == 0 {
		fmt.Fprintf(b, "  %s\n", styleDim.Render("nothing ran in this time range"))
		return
	}

	// The selected row.
	r := d.tr.Rows[d.vis[d.sel]]
	b.WriteByte('\n')
	fmt.Fprintf(b, "  %s %s  %s\n", styleHook.Render("▶"), traceLabelStyle(r).Render(r.Label), styleDim.Render(joinDot(r.Loc, r.Detail)))
	facts := []string{
		"starts " + narrative.FormatOpsDur(r.Start.Sub(d.tr.From)) + " into the hook",
		"runs for " + narrative.FormatOpsDur(r.Dur()),
	}
	cpuFact := func(ms float64) string {
		return "cpu " + narrative.FormatOpsDur(time.Duration(ms*float64(time.Millisecond)))
	}
	switch {
	case r.Kind == narrative.TraceFunc:
		if codeCycle[st.lvl] == narrative.DetailAll {
			facts = append(facts, cpuFact(r.CPUMS))
		}
		facts = append(facts, "memory taken by it: about "+narrative.FormatBytes(r.MemBytes))
		if r.Waiting {
			facts = append(facts, styleWarn.Render("mostly "+orWaiting(r.WaitingOn)+" (it used little CPU for the time it took)"))
		}
	case r.Cost != nil:
		cpu, growth, peak := narrative.OpsCostCells(r.Cost)
		if cpu != "" && codeCycle[st.lvl] == narrative.DetailAll {
			facts = append(facts, "cpu "+cpu)
		}
		if growth != "" {
			if codeCycle[st.lvl] != narrative.DetailAll {
				growth, peak = plainMemory(growth), plainMemory(peak)
			}
			facts = append(facts, "memory change "+growth)
			if peak != "" {
				facts = append(facts, "memory peak "+peak)
			}
		}
	}
	fmt.Fprintf(b, "    %s\n", strings.Join(facts, " · "))
	if r.Kind == recording.OpsInit {
		fmt.Fprintf(b, "    %s\n", styleDim.Render("This runs from ops starting to the first event: the charm's __init__, and a little ops setup around it."))
	}
	if r.Hidden > 0 {
		fmt.Fprintf(b, "    %s\n", styleDim.Render(fmt.Sprintf("%d short %s under it left out (. shows them)", r.Hidden, plural(r.Hidden, "call or event", "calls and events"))))
	}
	if r.Error != "" {
		fmt.Fprintf(b, "    %s\n", styleErr.Render("✗ "+r.Error))
	}
	fmt.Fprintf(b, "\n  %s %s  %s %s  %s %s  %s %s  %s %s  %s %s\n",
		lipgloss.NewStyle().Foreground(lipgloss.Color("38")).Render("█"), styleDim.Render("event, observer"),
		lipgloss.NewStyle().Foreground(lipgloss.Color("173")).Render("█"), styleDim.Render("charm function"),
		lipgloss.NewStyle().Foreground(lipgloss.Color("71")).Render("█"), styleDim.Render("ops"),
		lipgloss.NewStyle().Foreground(lipgloss.Color("178")).Render("█"), styleDim.Render("Pebble call"),
		lipgloss.NewStyle().Foreground(lipgloss.Color("103")).Render("█"), styleDim.Render("hook tool"),
		lipgloss.NewStyle().Foreground(lipgloss.Color("160")).Render("█"), styleDim.Render("failed"))
}

// traceLabelStyle colours a row's label by what it is.
func traceLabelStyle(r narrative.TraceRow) lipgloss.Style {
	switch {
	case (r.Kind == narrative.TraceFunc || r.Kind == recording.OpsInit) && r.Class == narrative.ClassCharm:
		return styleApp
	case r.Kind == narrative.TraceFunc && r.Class == narrative.ClassOps:
		return styleOK
	case r.Kind == recording.OpsRoot:
		return styleApp
	case r.Kind == recording.OpsEvent:
		return styleHook
	case r.Kind == recording.OpsTool, r.Kind == recording.OpsPebble:
		return styleDim
	}
	return styleText
}

// traceMemory draws the process's resident memory over the time shown, one
// column for each of barW, from its lowest value there to its highest. The
// value holds from one reading to the next.
func traceMemory(d *traceData, barW int) string {
	pts := d.tr.RSS
	if len(pts) == 0 || barW <= 0 {
		return ""
	}
	span := float64(d.to.Sub(d.from))
	vals := make([]int64, barW)
	set := make([]bool, barW)
	var carry int64
	have := false
	pi := 0
	for c := 0; c < barW; c++ {
		end := d.from.Add(time.Duration(span * float64(c+1) / float64(barW)))
		for pi < len(pts) && pts[pi].T.Before(end) {
			if !pts[pi].T.Before(d.from) {
				vals[c], set[c] = max(vals[c], pts[pi].RSS), true
			}
			carry, have = pts[pi].RSS, true
			pi++
		}
		if !set[c] && have {
			vals[c], set[c] = carry, true
		}
	}
	lo, hi := int64(math.MaxInt64), int64(0)
	for c := range vals {
		if set[c] {
			lo, hi = min(lo, vals[c]), max(hi, vals[c])
		}
	}
	if hi == 0 {
		return ""
	}
	var sb strings.Builder
	for c := range vals {
		switch {
		case !set[c]:
			sb.WriteByte(' ')
		case hi == lo:
			sb.WriteRune(levels[3])
		default:
			sb.WriteRune(levels[int(7*(vals[c]-lo)/(hi-lo))])
		}
	}
	return sb.String()
}

// orWaiting is what a function was waiting for, or just "waiting".
func orWaiting(what string) string {
	if what == "" {
		return "waiting"
	}
	return what
}

// traceMem is the memory a row took, when it is at least 1 MB: "+16MB".
func traceMem(r narrative.TraceRow) string {
	v, ok := r.Mem()
	if !ok || v > -(1<<20) && v < 1<<20 {
		return ""
	}
	if v < 0 {
		return narrative.FormatBytes(v)
	}
	return "+" + narrative.FormatBytes(v)
}

// joinDot joins the parts that aren't empty with " · ".
func joinDot(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, " · ")
}
