package viewer

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// inspMode is what the inspector shows.
type inspMode int

const (
	inspDetail inspMode = iota // the event's detail
	inspFlame                  // `p`: where the hook's Python spent its time
	inspMemory                 // `m`: memory around the hook
	inspTrace                  // `t`: the hook along time, as a trace
)

// profileView is how the profile view draws a profile.
type profileView int

const (
	profTree  profileView = iota // the call tree
	profTable                    // the functions, with their own cost and with what they called
)

func (v profileView) String() string {
	if v == profTable {
		return "table of functions"
	}
	return "call tree"
}

// profThreshold is the share of the scope below which the views hide a
// function, unless asked to show everything.
const profThreshold = 0.01

// flameScope is a part of the hook a profile can show: the whole hook,
// charm init, or one observer, with the spans whose stacks it covers.
type flameScope struct {
	label string
	spans map[string]bool // nil: every stack of the hook's processes
}

// flameState is what the profile view remembers between keys. Zoom and
// selection are frame paths from the outermost frame, so they survive a
// change of metric or detail, which reorder or drop frames.
type flameState struct {
	ready  bool
	key    string // event the scopes were built for
	scopes []flameScope
	scope  int
	metric narrative.Metric
	view   profileView
	lvl    int // index into codeCycle: how much code to show

	zoom     []int // tree: the node drawn at the top
	sel      []int // tree: the selected node
	tsel     int   // table: the selected function's frame
	focus    []int // table: functions peeked at, innermost last
	focusSel int   // table: the selected caller or callee
	selRow   int   // the selected row's line in the last render, from the top of the body

	query     string
	re        *regexp.Regexp
	searching bool
	input     string
	notice    string
}

// codeCycle is how much code the trace and the profile can show: the charm's
// code and the calls it makes (the default), and all code, which `.` switches
// between.
var codeCycle = []narrative.Detail{narrative.DetailCharmCallee, narrative.DetailAll}

// detailLevel is the code the profile shows.
func (st *flameState) detailLevel() narrative.Detail { return codeCycle[st.lvl] }

// levelLabel says what a level shows.
func levelLabel(d narrative.Detail) string {
	if d == narrative.DetailAll {
		return "everything: all code, short calls and functions, and imports"
	}
	return "the charm's code and the calls it makes"
}

// levelHelp says what `.` does at level lvl.
func levelHelp(lvl int) string {
	if lvl == 0 {
		return ". shows more"
	}
	return ". shows less"
}

func (st *flameState) ensure() {
	if st.ready {
		return
	}
	st.ready = true
	st.tsel = -1
}

// resetNav forgets where the view was moved to, for a new scope or event.
func (st *flameState) resetNav() {
	st.zoom, st.sel, st.focus, st.focusSel, st.tsel = nil, nil, nil, 0, -1
}

// reset undoes the view's narrowing, the most recent kind first, and reports
// whether there was any.
func (st *flameState) reset() bool {
	switch {
	case st.searching:
		st.searching, st.input = false, ""
	case len(st.focus) > 0:
		st.focus, st.focusSel = nil, 0
	case len(st.zoom) > 0:
		st.zoom = nil
	case st.query != "":
		st.query, st.re = "", nil
	default:
		return false
	}
	return true
}

// flameScopes lists what a profile can be drawn for: the hook, then
// charm init and each observer, in order, when stacks were sampled in them.
func flameScopes(trees []*narrative.OpsNode) []flameScope {
	out := []flameScope{{label: "the whole hook"}}
	var walk func(n *narrative.OpsNode, prefix string)
	walk = func(n *narrative.OpsNode, prefix string) {
		var label string
		switch n.Kind {
		case recording.OpsInit:
			label = "the charm's __init__"
		case recording.OpsObserver:
			label = "observer " + narrative.ShortObserver(n.Name)
		}
		if label != "" && n.Cost != nil && n.Cost.Samples > 0 {
			out = append(out, flameScope{label: prefix + label, spans: narrative.SubtreeSpans(n)})
		}
		for _, c := range n.Children {
			walk(c, prefix)
		}
	}
	for i, t := range trees {
		prefix := ""
		if len(trees) > 1 {
			prefix = fmt.Sprintf("attempt %d: ", i+1)
		}
		walk(t, prefix)
	}
	return out
}

// flameData is a scope's profile laid out for the current view.
type flameData struct {
	prof  *narrative.Profile // at the chosen detail
	root  *narrative.Node    // every stack of the scope
	zoom  *narrative.Node    // the node the tree starts from
	sel   *narrative.Node    // tree: the selected node
	width int

	lines  []treeLine // tree
	hidden int        // tree: functions left out for being under the threshold

	table []narrative.FuncStat // table: functions over the threshold
	below int                  // table: functions under it
	peek  *peekData            // table: set when peeking at one function
}

// treeLine is one function listed in the tree view.
type treeLine struct {
	node *narrative.Node
	lead string // branch characters before the name
}

// peekData is one function with the functions around it.
type peekData struct {
	fs      narrative.FuncStat
	callers []narrative.Edge
	callees []narrative.Edge
	edges   []narrative.Edge // the callers, then the callees, that can be selected
}

// flameData builds the profile of the selected scope for the current view,
// resolving the zoom and selection paths to nodes.
func (m *model) flameData(ev event, width int) (*flameData, bool) {
	trees, profs := m.hookTrees(ev)
	if len(profs) == 0 {
		return nil, false
	}
	st := &m.flame
	st.ensure()
	if st.key != ev.spanID || len(st.scopes) == 0 {
		st.key, st.scopes, st.scope = ev.spanID, flameScopes(trees), 0
		st.resetNav()
	}
	st.scope = clamp(st.scope, 0, len(st.scopes)-1)
	d := &flameData{width: width}
	d.prof = narrative.NewProfile(profs, st.scopes[st.scope].spans).AtDetail(st.detailLevel())
	d.root = d.prof.Tree()
	d.root.Sort(st.metric)
	d.zoom = follow(d.root, st.zoom)
	st.zoom = d.zoom.Path()
	threshold := profThreshold
	switch st.view {
	case profTable:
		d.table, d.below = functionTable(d.prof, st.metric, d.root.Total.Get(st.metric), threshold)
		if len(st.focus) > 0 {
			self, callers, callees := d.prof.Peek(st.focus[len(st.focus)-1], st.metric)
			if self.Total.Samples == 0 {
				st.focus, st.focusSel = nil, 0
			} else {
				d.peek = &peekData{fs: self, callers: callers, callees: callees}
				for _, e := range append(append([]narrative.Edge(nil), callers...), callees...) {
					if e.Frame >= 0 {
						d.peek.edges = append(d.peek.edges, e)
					}
				}
				st.focusSel = clamp(st.focusSel, 0, max(0, len(d.peek.edges)-1))
			}
		}
		if len(d.table) > 0 && !tableHas(d.table, st.tsel) {
			st.tsel = d.table[0].Frame
		}
	default:
		d.lines, d.hidden = layoutTree(d.zoom, st.metric, threshold)
		d.sel = follow(d.zoom, st.sel[min(len(st.sel), len(st.zoom)):])
		if d.lineOf(d.sel) < 0 {
			d.sel = d.zoom
		}
		if len(st.sel) == 0 && d.sel == d.root && len(d.lines) > 1 {
			d.sel = d.lines[1].node // start on the heaviest function, not on the root
		}
		st.sel = d.sel.Path()
	}
	return d, true
}

// functionTable lists the functions with the most of m in their own code,
// leaving out those whose total is under threshold of sum; it also returns
// how many were under the threshold.
func functionTable(p *narrative.Profile, m narrative.Metric, sum, threshold float64) (rows []narrative.FuncStat, below int) {
	if sum <= 0 {
		return nil, 0
	}
	for _, fs := range p.Functions(m) {
		switch {
		case fs.Self.Get(m) <= 0:
		case fs.Total.Get(m)/sum < threshold:
			below++
		default:
			rows = append(rows, fs)
		}
	}
	return rows, below
}

func tableHas(rows []narrative.FuncStat, frame int) bool {
	for _, r := range rows {
		if r.Frame == frame {
			return true
		}
	}
	return false
}

// follow walks path (frame ids) down from n as far as it goes.
func follow(n *narrative.Node, path []int) *narrative.Node {
	for _, f := range path {
		var next *narrative.Node
		for _, c := range n.Children {
			if c.Frame == f {
				next = c
				break
			}
		}
		if next == nil {
			break
		}
		n = next
	}
	return n
}

// layoutTree lists the call tree under top, one line per function, indenting
// only where calls branch, so a long single call path reads as a straight
// column. Functions under threshold of the whole are left out; it returns
// how many.
func layoutTree(top *narrative.Node, metric narrative.Metric, threshold float64) ([]treeLine, int) {
	sum := top.Total.Get(metric)
	if sum <= 0 {
		return nil, 0
	}
	var lines []treeLine
	hidden := 0
	var walk func(n *narrative.Node, lead, cont string)
	walk = func(n *narrative.Node, lead, cont string) {
		lines = append(lines, treeLine{node: n, lead: lead})
		var kids []*narrative.Node
		for _, c := range n.Children {
			if c.Total.Get(metric)/sum >= threshold {
				kids = append(kids, c)
			} else {
				hidden++
			}
		}
		switch {
		case len(kids) == 1:
			walk(kids[0], cont, cont)
		case len(kids) > 1:
			for i, k := range kids {
				if i == len(kids)-1 {
					walk(k, cont+"└─ ", cont+"   ")
				} else {
					walk(k, cont+"├─ ", cont+"│  ")
				}
			}
		}
	}
	walk(top, "", "")
	return lines, hidden
}

// lineOf is the index of n in the tree's lines, or -1.
func (d *flameData) lineOf(n *narrative.Node) int {
	for i, l := range d.lines {
		if l.node == n {
			return i
		}
	}
	return -1
}

// matches reports whether frame f is what the search is for.
func (st *flameState) matches(p *narrative.Profile, f int) bool {
	if f < 0 || st.re == nil {
		return false
	}
	fn, loc := p.FrameLabel(f)
	return st.re.MatchString(fn + " " + loc)
}

// setQuery compiles a search typed by the user: a case-insensitive regular
// expression, or the text itself if it isn't one.
func (st *flameState) setQuery(q string) {
	st.query, st.re = q, nil
	if q == "" {
		return
	}
	re, err := regexp.Compile("(?i)" + q)
	if err != nil {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(q))
	}
	st.re = re
}

// matchList is the frames the current view lists in reading order, the index
// of the selected one, and how to select another.
func (m *model) matchList(d *flameData) (frames []int, cur int, set func(i int)) {
	st := &m.flame
	cur = -1
	switch {
	case st.view == profTable && d.peek != nil:
		for _, e := range d.peek.edges {
			frames = append(frames, e.Frame)
		}
		return frames, st.focusSel, func(i int) { st.focusSel = i }
	case st.view == profTable:
		for i, r := range d.table {
			frames = append(frames, r.Frame)
			if r.Frame == st.tsel {
				cur = i
			}
		}
		return frames, cur, func(i int) { st.tsel = d.table[i].Frame }
	}
	var nodes []*narrative.Node
	for i, l := range d.lines {
		frames = append(frames, l.node.Frame)
		nodes = append(nodes, l.node)
		if l.node == d.sel {
			cur = i
		}
	}
	return frames, cur, func(i int) { st.sel = nodes[i].Path() }
}

// stepMatch selects the next (dir 1) or previous (dir -1) match of the
// search in the current view, counting the selected one when inclusive.
func (m *model) stepMatch(d *flameData, dir int, inclusive bool) {
	st := &m.flame
	st.notice = ""
	frames, cur, set := m.matchList(d)
	start := cur
	if !inclusive || cur < 0 {
		start = cur + dir
	}
	for k := range frames {
		i := ((start+dir*k)%len(frames) + len(frames)) % len(frames)
		if st.matches(d.prof, frames[i]) {
			set(i)
			return
		}
	}
	st.notice = "nothing in this view matches"
	if st.lvl == 0 {
		st.notice += " (. also searches ops and library code)"
	}
}

// matchCount is how many functions the current view lists that match the
// search.
func (m *model) matchCount(d *flameData) int {
	frames, _, _ := m.matchList(d)
	n := 0
	for _, f := range frames {
		if m.flame.matches(d.prof, f) {
			n++
		}
	}
	return n
}

// handleFlameKey applies a profile view key; it reports whether the key was
// one, so other keys fall through to scrolling.
func (m *model) handleFlameKey(msg tea.KeyMsg) bool {
	st := &m.flame
	st.ensure()
	if st.searching {
		return m.searchKey(msg)
	}
	d, ok := m.flameData(m.currentEvent(), m.flameWidth())
	if !ok {
		return false
	}
	switch msg.String() {
	case "tab", "shift+tab":
		if n := len(st.scopes); n > 0 {
			delta := 1
			if msg.String() == "shift+tab" {
				delta = n - 1
			}
			st.scope = (st.scope + delta) % n
			st.resetNav()
		}
		return true
	case "s":
		st.metric = narrative.Metrics[(int(st.metric)+1)%len(narrative.Metrics)]
		return true
	case "v":
		st.view = (st.view + 1) % 2
		return true
	case ".":
		st.lvl = (st.lvl + 1) % len(codeCycle)
		st.focus, st.focusSel = nil, 0
		return true
	case "/":
		st.searching, st.input, st.notice = true, "", ""
		return true
	case "n", "N":
		dir := 1
		if msg.String() == "N" {
			dir = -1
		}
		if st.re == nil {
			st.notice = "nothing to search for: press / first"
			return true
		}
		m.stepMatch(d, dir, false)
		return true
	}
	if st.view == profTree {
		return m.treeKey(msg, d)
	}
	return m.tableKey(msg, d)
}

// searchKey edits the search being typed; enter selects the first match.
func (m *model) searchKey(msg tea.KeyMsg) bool {
	st := &m.flame
	switch msg.Type {
	case tea.KeyEnter:
		st.searching = false
		st.setQuery(st.input)
		st.input = ""
		if d, ok := m.flameData(m.currentEvent(), m.flameWidth()); ok && st.re != nil {
			m.stepMatch(d, 1, true)
		}
	case tea.KeyEsc:
		st.searching, st.input = false, ""
	case tea.KeyBackspace:
		if r := []rune(st.input); len(r) > 0 {
			st.input = string(r[:len(r)-1])
		}
	case tea.KeyRunes, tea.KeySpace:
		st.input += string(msg.Runes)
		if msg.Type == tea.KeySpace {
			st.input += " "
		}
	}
	return true
}

func (m *model) tableKey(msg tea.KeyMsg, d *flameData) bool {
	st := &m.flame
	move := func(delta int) {
		if d.peek != nil {
			st.focusSel = clamp(st.focusSel+delta, 0, max(0, len(d.peek.edges)-1))
			return
		}
		for i, r := range d.table {
			if r.Frame == st.tsel {
				st.tsel = d.table[clamp(i+delta, 0, len(d.table)-1)].Frame
				return
			}
		}
	}
	switch {
	case key.Matches(msg, m.keys.Down):
		move(1)
	case key.Matches(msg, m.keys.Up):
		move(-1)
	case key.Matches(msg, m.keys.Inspect):
		switch {
		case d.peek != nil && len(d.peek.edges) > 0:
			st.focus = append(st.focus, d.peek.edges[st.focusSel].Frame)
			st.focusSel = 0
		case d.peek == nil && st.tsel >= 0:
			st.focus, st.focusSel = []int{st.tsel}, 0
		}
	case msg.String() == "backspace":
		if len(st.focus) > 0 {
			st.focus = st.focus[:len(st.focus)-1]
			st.focusSel = 0
		}
	default:
		return false
	}
	return true
}

func (m *model) treeKey(msg tea.KeyMsg, d *flameData) bool {
	st := &m.flame
	i := d.lineOf(d.sel)
	switch {
	case key.Matches(msg, m.keys.Down):
		if i+1 < len(d.lines) {
			st.sel = d.lines[i+1].node.Path()
		}
	case key.Matches(msg, m.keys.Up):
		if i > 0 {
			st.sel = d.lines[i-1].node.Path()
		}
	case key.Matches(msg, m.keys.Inspect):
		st.zoom = st.sel
	case msg.String() == "backspace":
		if len(st.zoom) > 0 {
			st.zoom = st.zoom[:len(st.zoom)-1]
		}
	default:
		return false
	}
	return true
}

// flameWidth is the profile's width: the inspector's, less the indent.
func (m *model) flameWidth() int { return max(20, m.width-6) }

// profileHelp is the keybinding line for the profile view.
func (m *model) profileHelp() string {
	st := &m.flame
	if st.searching {
		return "type a search (a regular expression) · enter find · esc cancel"
	}
	help := "esc back · t trace · tab other part of the hook · s time/cpu/memory · v tree/table · . more or less · / search · ↑/↓ select"
	if st.view == profTree {
		return help + " · enter start the tree here · backspace back"
	}
	return help + " · enter callers and callees"
}

// metricLabel and detailLabel say what the options mean, for the header.
func metricLabel(mt narrative.Metric) string {
	switch mt {
	case narrative.MetricCPU:
		return "cpu time"
	case narrative.MetricMem:
		return "memory taken"
	}
	return "time"
}

// metricShort names a metric in a column heading.
func metricShort(mt narrative.Metric) string {
	switch mt {
	case narrative.MetricCPU:
		return "cpu"
	case narrative.MetricMem:
		return "memory"
	}
	return "time"
}

// frameName is a function's name as the views show it: a module's top level
// is the import of the module.
func frameName(p *narrative.Profile, frame int) string {
	if frame < 0 {
		return "(everything)"
	}
	if frame >= len(p.Frames) {
		return "(other code)"
	}
	return narrative.FrameName(p.Frames[frame])
}

// renderFlameView shows where the hook's Python spent its time or took
// memory, for the hook, charm init or one observer: as a table of functions
// or as a call tree.
func (m *model) renderFlameView(b *strings.Builder, ev event) {
	st := &m.flame
	st.ensure()
	d, ok := m.flameData(ev, m.flameWidth())
	if !ok {
		fmt.Fprintf(b, "%s\n", styleDim.Render("no stack samples for this hook: record with --attach unit (and --profile, on by default) to sample them"))
		return
	}
	total := d.root.Total
	fmt.Fprintf(b, "%s  %s\n", styleSection.Render("profile of "+st.scopes[st.scope].label),
		styleDim.Render(fmt.Sprintf("part %d of %d: tab for the next", st.scope+1, len(st.scopes))))
	fmt.Fprintf(b, "  %s\n", strings.Join([]string{
		fv(narrative.MetricWall, total.Get(narrative.MetricWall)) + " sampled",
		"cpu " + fv(narrative.MetricCPU, total.Get(narrative.MetricCPU)),
		"memory taken " + fv(narrative.MetricMem, total.Get(narrative.MetricMem)),
	}, " · "))
	fmt.Fprintf(b, "  showing %s %s · %s %s · %s %s\n",
		styleHook.Render(metricLabel(st.metric)), styleDim.Render("(s)"),
		styleHook.Render(levelLabel(st.detailLevel())), styleDim.Render("("+levelHelp(st.lvl)+")"),
		styleHook.Render(st.view.String()), styleDim.Render("(v)"))
	switch {
	case st.searching:
		fmt.Fprintf(b, "  %s\n", styleHook.Render("/")+st.input+styleDim.Render("▏"))
	case st.re != nil:
		fmt.Fprintf(b, "  %s\n", styleWarn.Render(fmt.Sprintf("searching /%s/: %d here (n next, N previous, esc clears)", st.query, m.matchCount(d))))
	}
	if st.notice != "" {
		fmt.Fprintf(b, "  %s\n", styleWarn.Render(st.notice))
	}
	if len(st.zoom) > 0 && st.view == profTree {
		fmt.Fprintf(b, "  %s\n", styleWarn.Render("starting from "+frameName(d.prof, d.zoom.Frame)+" (backspace goes back)"))
	}
	b.WriteByte('\n')
	if d.root.Total.Get(st.metric) <= 0 {
		fmt.Fprintf(b, "  %s\n", styleDim.Render("nothing recorded for this: press s for another"))
		return
	}
	cols := d.width + 2
	if st.view == profTree {
		m.renderTree(b, d, cols)
	} else {
		m.renderTable(b, d, cols)
	}
	m.legend(b, d)
}

// frameStyleFor colours a function name by whose code it is: the charm's,
// the ops framework's, or anything else.
func frameStyleFor(p *narrative.Profile, frame int) lipgloss.Style {
	switch p.Class(frame) {
	case narrative.ClassCharm:
		return styleApp
	case narrative.ClassOps:
		return styleOK
	}
	return styleText
}

// styledName draws a function name in its class's colour, marked when it
// matches the search.
func (m *model) styledName(p *narrative.Profile, frame int) string {
	st := frameStyleFor(p, frame)
	if m.flame.matches(p, frame) {
		st = lipgloss.NewStyle().Foreground(lipgloss.Color("16")).Background(lipgloss.Color("220"))
	}
	return st.Render(frameName(p, frame))
}

// foldNote says how much of a function's own cost ran in hidden code.
func foldNote(mt narrative.Metric, v narrative.Value) string {
	if f := v.Get(mt); f > 0 {
		return "  ↳ " + fv(mt, f) + " of it below, in code not shown"
	}
	return ""
}

// legend explains the colours and, when something is hidden, what that means.
func (m *model) legend(b *strings.Builder, d *flameData) {
	fmt.Fprintf(b, "\n  %s %s  %s %s  %s %s\n",
		styleApp.Render("■"), styleDim.Render("charm code"),
		styleOK.Render("■"), styleDim.Render("ops"),
		styleText.Render("■"), styleDim.Render("libraries and Python"))
	if m.flame.detailLevel() != narrative.DetailAll {
		fmt.Fprintf(b, "  %s\n", styleDim.Render("Code the view doesn't show, such as ops, the standard library and imports, counts in the function that called it. . shows more."))
	}
}

// renderTable lists the functions with the cost of their own code and of
// what they called, with the other metrics' totals alongside, and what the
// selected function is called by below.
func (m *model) renderTable(b *strings.Builder, d *flameData, cols int) {
	st := &m.flame
	mt := st.metric
	if d.peek != nil {
		m.renderPeek(b, d, cols)
		return
	}
	sum := d.root.Total.Get(mt)
	var others []narrative.Metric
	for _, o := range narrative.Metrics {
		if o != mt {
			others = append(others, o)
		}
	}
	heads := fmt.Sprintf("%8s %6s %10s %6s", "own", "", "with calls", "")
	for _, o := range others {
		heads += fmt.Sprintf(" %8s", metricShort(o))
	}
	fmt.Fprintf(b, "  %s\n", styleDim.Render(heads+"  function"))
	base := strings.Count(b.String(), "\n")
	for i, fs := range d.table {
		self, t := fs.Self.Get(mt), fs.Total.Get(mt)
		nums := fmt.Sprintf("%8s %5.1f%% %10s %5.1f%%", fv(mt, self), 100*self/sum, fv(mt, t), 100*t/sum)
		for _, o := range others {
			nums += fmt.Sprintf(" %8s", fv(o, fs.Total.Get(o)))
		}
		_, loc := d.prof.FrameLabel(fs.Frame)
		var row string
		if fs.Frame == st.tsel {
			st.selRow = base + i
			row = styleSelected.Render(ansi.Truncate(fmt.Sprintf("  %s  %s  %s", nums, frameName(d.prof, fs.Frame), loc), cols, "…"))
		} else {
			row = ansi.Truncate(fmt.Sprintf("  %s  %s  %s", nums, m.styledName(d.prof, fs.Frame), styleDim.Render(loc)), cols, "…")
		}
		fmt.Fprintf(b, "%s\n", row)
	}
	if len(d.table) == 0 {
		fmt.Fprintf(b, "  %s\n", styleDim.Render("no function took that much: press . to show more"))
	}
	if d.below > 0 {
		fmt.Fprintf(b, "  %s\n", styleDim.Render(fmt.Sprintf("%d more %s under 1%% not shown", d.below, plural(d.below, "function", "functions"))))
	}
	if st.tsel < 0 || !tableHas(d.table, st.tsel) {
		return
	}
	var fs narrative.FuncStat
	for _, r := range d.table {
		if r.Frame == st.tsel {
			fs = r
		}
	}
	b.WriteByte('\n')
	_, loc := d.prof.FrameLabel(fs.Frame)
	fmt.Fprintf(b, "  %s %s  %s\n", styleHook.Render("▶"), m.styledName(d.prof, fs.Frame), styleDim.Render(loc))
	frames, paths := d.prof.HeaviestStack(fs.Frame, mt)
	fmt.Fprintf(b, "    %s with calls (%.1f%% of this part) · %s in the function itself%s · called %d %s\n",
		fv(mt, fs.Total.Get(mt)), 100*fs.Total.Get(mt)/sum, fv(mt, fs.Self.Get(mt)), foldNote(mt, fs.Folded),
		paths, plural(paths, "way", "ways"))
	var names []string
	for _, f := range frames {
		names = append(names, frameStyleFor(d.prof, f).Render(frameName(d.prof, f)))
	}
	fmt.Fprintf(b, "    %s\n", ansi.Truncate("called from: "+strings.Join(names, styleDim.Render(" ← ")), cols-4, "…"))
	fmt.Fprintf(b, "  %s\n", styleDim.Render("enter: what called it and what it called"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// renderPeek lists what called one function and what it called: each edge
// with its share of the function's total, in the style of pprof's -peek.
func (m *model) renderPeek(b *strings.Builder, d *flameData, cols int) {
	st := &m.flame
	mt := st.metric
	pk := d.peek
	total := pk.fs.Total.Get(mt)
	if total <= 0 {
		total = 1
	}
	fmt.Fprintf(b, "  %s\n", styleWarn.Render("what called and was called by "+frameName(d.prof, pk.fs.Frame)+" (backspace goes back)"))
	n := 0 // selectable edges drawn so far
	edge := func(e narrative.Edge) {
		v := e.Value.Get(mt)
		line := fmt.Sprintf("    %8s %5.1f%%  ", fv(mt, v), 100*v/total)
		switch {
		case e.Frame < 0:
			fmt.Fprintf(b, "%s\n", styleDim.Render(line+"(the function itself)"))
			return
		case n == st.focusSel:
			_, loc := d.prof.FrameLabel(e.Frame)
			st.selRow = strings.Count(b.String(), "\n")
			fmt.Fprintf(b, "%s\n", styleSelected.Render(ansi.Truncate(line+frameName(d.prof, e.Frame)+"  "+loc, cols, "…")))
		default:
			_, loc := d.prof.FrameLabel(e.Frame)
			fmt.Fprintf(b, "%s\n", ansi.Truncate(line+m.styledName(d.prof, e.Frame)+"  "+styleDim.Render(loc), cols, "…"))
		}
		n++
	}
	fmt.Fprintf(b, "  %s\n", styleDim.Render("called by"))
	for _, e := range pk.callers {
		if e.Frame < 0 {
			v := e.Value.Get(mt)
			fmt.Fprintf(b, "%s\n", styleDim.Render(fmt.Sprintf("    %8s %5.1f%%  (the top of the stack: nothing in this view called it)", fv(mt, v), 100*v/total)))
			continue
		}
		edge(e)
	}
	_, loc := d.prof.FrameLabel(pk.fs.Frame)
	fmt.Fprintf(b, "\n  %s %s  %s\n", styleHook.Render("▶"), m.styledName(d.prof, pk.fs.Frame), styleDim.Render(loc))
	fmt.Fprintf(b, "    %s with calls · %s in the function itself%s\n\n",
		fv(mt, pk.fs.Total.Get(mt)), fv(mt, pk.fs.Self.Get(mt)), foldNote(mt, pk.fs.Folded))
	fmt.Fprintf(b, "  %s\n", styleDim.Render("calls"))
	for _, e := range pk.callees {
		edge(e)
	}
	fmt.Fprintf(b, "  %s\n", styleDim.Render("enter: look at the selected function"))
}

// renderTree draws the call tree: one line per function with its total,
// share and own cost, and the selected function's numbers below.
func (m *model) renderTree(b *strings.Builder, d *flameData, cols int) {
	st := &m.flame
	mt := st.metric
	sum := d.zoom.Total.Get(mt)
	fmt.Fprintf(b, "  %s\n", styleDim.Render(fmt.Sprintf("%10s %6s %8s  %-12s %s", "with calls", "", "own", "", "function")))
	base := strings.Count(b.String(), "\n")
	for i, l := range d.lines {
		n := l.node
		t := n.Total.Get(mt)
		_, loc := d.prof.FrameLabel(n.Frame)
		nums := fmt.Sprintf("%10s %5.1f%% %8s", fv(mt, t), 100*t/sum, fv(mt, n.Self.Get(mt)))
		var row string
		if n == d.sel {
			// Only the name is highlighted: the bar keeps its colour.
			st.selRow = base + i
			row = ansi.Truncate(fmt.Sprintf("  %s  %s %s%s  %s", nums, styleHook.Render(bar(t/sum, 12)),
				styleDim.Render(l.lead), styleSelected.Render(frameName(d.prof, n.Frame)), styleDim.Render(loc)), cols, "…")
		} else {
			row = ansi.Truncate(fmt.Sprintf("  %s  %s %s%s  %s", nums, styleHook.Render(bar(t/sum, 12)),
				styleDim.Render(l.lead), m.styledName(d.prof, n.Frame), styleDim.Render(loc)), cols, "…")
		}
		fmt.Fprintf(b, "%s\n", row)
	}
	if d.hidden > 0 {
		fmt.Fprintf(b, "  %s\n", styleDim.Render(fmt.Sprintf("%d more %s under 1%% not shown", d.hidden, plural(d.hidden, "function", "functions"))))
	}
	b.WriteByte('\n')
	sel := d.sel
	all := d.root.Total.Get(mt)
	_, loc := d.prof.FrameLabel(sel.Frame)
	fmt.Fprintf(b, "  %s %s  %s\n", styleHook.Render("▶"), m.styledName(d.prof, sel.Frame), styleDim.Render(loc))
	fmt.Fprintf(b, "    %s with calls (%.1f%% of this part) · %s in the function itself%s\n",
		fv(mt, sel.Total.Get(mt)), 100*sel.Total.Get(mt)/all, fv(mt, sel.Self.Get(mt)), foldNote(mt, sel.Folded))
}

// bar draws share (0..1) as a fixed-width bar with eighth-cell precision.
func bar(share float64, width int) string {
	const blocks = " ▏▎▍▌▋▊▉█"
	cells := share * float64(width)
	full := int(cells)
	var b strings.Builder
	b.WriteString(strings.Repeat("█", min(full, width)))
	if full < width {
		frac := int(math.Round((cells - float64(full)) * 8))
		if frac > 0 && frac < 8 {
			b.WriteString(string([]rune(blocks)[frac]))
			full++
		}
		b.WriteString(strings.Repeat(" ", width-full))
	}
	return b.String()
}

// fv formats a value of a metric with a space before its unit, so "253 ms"
// and "2.9 s" are easy to tell apart.
func fv(mt narrative.Metric, v float64) string {
	s := mt.Format(v)
	for i, r := range s {
		if i > 0 && (r < '0' || r > '9') && r != '.' {
			return s[:i] + " " + s[i:]
		}
	}
	return s
}
