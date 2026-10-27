package viewer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// fixtureModel opens the viewer on a lab recording under
// internal/recording/testdata/ops, indexed with its stack samples and
// resource samples, with the inspector open on the first hook.
func fixtureModel(t *testing.T, name string) *model {
	t.Helper()
	root := filepath.Join("..", "recording", "testdata", "ops", name)
	spans, profs, err := recording.LoadOps(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := recording.LoadUnitResources(root)
	if err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	for _, sp := range spans {
		if err := db.InsertSpan(sp); err != nil {
			t.Fatalf("InsertSpan: %v", err)
		}
	}
	for _, p := range profs {
		if err := db.InsertProfile(p); err != nil {
			t.Fatalf("InsertProfile: %v", err)
		}
	}
	if err := db.InsertUnitResources(res); err != nil {
		t.Fatal(err)
	}
	id, err := db.UpsertModel(spans[0].Model)
	if err != nil {
		t.Fatal(err)
	}
	m := buildModel(t, db, index.Model{ID: id, Name: spans[0].Model})
	m.width, m.height = 160, 60
	m.overlay.Width, m.overlay.Height = 156, 1000
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if !m.overlayOn {
		t.Fatal("Enter did not open the inspector")
	}
	return m
}

func press(t *testing.T, m *model, keys ...tea.KeyMsg) *model {
	t.Helper()
	for _, k := range keys {
		updated, _ := m.handleKey(k)
		m = updated.(*model)
	}
	return m
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// TestProfileTreeZoom: the profile opens as a call tree; moving down
// the selection and pressing enter starts the tree from the selected function,
// and backspace goes back to its caller.
func TestProfileTreeZoom(t *testing.T) {
	m := fixtureModel(t, "stacks")
	m = press(t, m, runes("p"))
	if m.inspMode != inspFlame {
		t.Fatal("p did not open the profile")
	}
	// The profile opens as a call tree of the charm's functions and the calls
	// it makes; `.` shows every frame, down to ops and the standard library.
	m = press(t, m, runes("."))
	d, ok := m.flameData(m.currentEvent(), m.flameWidth())
	if !ok || d.sel != d.lines[1].node {
		t.Fatalf("call tree opens on the heaviest function: ok %v", ok)
	}
	for range 6 {
		m = press(t, m, runes("j"))
	}
	d, _ = m.flameData(m.currentEvent(), m.flameWidth())
	if fn := frameName(d.prof, d.sel.Frame); fn != "refresh_certs" {
		t.Fatalf("6 steps down the heaviest path select %q, want refresh_certs", fn)
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	d, _ = m.flameData(m.currentEvent(), m.flameWidth())
	if d.zoom != d.sel || d.lines[0].node != d.zoom {
		t.Fatal("enter did not start the tree from the selected function")
	}
	if !strings.Contains(m.overlay.View(), "starting from refresh_certs") {
		t.Errorf("zoomed view doesn't say so:\n%s", m.overlay.View())
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyBackspace})
	d, _ = m.flameData(m.currentEvent(), m.flameWidth())
	if fn := frameName(d.prof, d.zoom.Frame); fn != "OpenTelemetryCollectorK8sCharm._reconcile" {
		t.Errorf("backspace went back to %q, want the caller", fn)
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.inspMode != inspFlame || len(m.flame.zoom) != 0 {
		t.Error("esc should go all the way back first")
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.inspMode != inspTrace || !m.overlayOn {
		t.Error("esc in the profile should go back to the trace")
	}
}

// profileOpen opens the profile view of the stacks fixture.
func profileOpen(t *testing.T) *model {
	t.Helper()
	m := fixtureModel(t, "stacks")
	m = press(t, m, runes("p"))
	if m.inspMode != inspFlame {
		t.Fatal("p did not open the profile")
	}
	return m
}

// TestProfileTable: `v` shows the profile as a table of the charm's functions
// with their own cost and their cost with calls, the most expensive first.
func TestProfileTable(t *testing.T) {
	m := profileOpen(t)
	m = press(t, m, runes("v"))
	d, ok := m.flameData(m.currentEvent(), m.flameWidth())
	if !ok || m.flame.view != profTable || len(d.table) < 3 {
		t.Fatalf("table view %v, %d rows", m.flame.view, len(d.table))
	}
	for i, r := range d.table {
		if i > 0 && r.Self.WallNs > d.table[i-1].Self.WallNs {
			t.Errorf("row %d has more own time than row %d", i, i-1)
		}
	}
	view := m.overlay.View()
	for _, want := range []string{"with calls", "called from:", "the charm's code and the calls it makes", "refresh_certs"} {
		if !strings.Contains(view, want) {
			t.Errorf("table lacks %q:\n%s", want, view)
		}
	}
}

// TestProfileCodeLevels: the profile opens on the charm's code and the calls
// it makes, and `.` shows all code and back. Hiding code moves its cost to the
// function that called it and never changes the total; only all code lists a
// module import.
func TestProfileCodeLevels(t *testing.T) {
	m := profileOpen(t)
	lines := func() (int, int, int64, bool) {
		d, _ := m.flameData(m.currentEvent(), m.flameWidth())
		ops, imports := 0, false
		for _, l := range d.lines {
			if d.prof.Class(l.node.Frame) == narrative.ClassOps {
				ops++
			}
			imports = imports || strings.HasPrefix(frameName(d.prof, l.node.Frame), "importing")
		}
		return len(d.lines), ops, d.root.Total.WallNs, imports
	}
	nDefault, opsDefault, total, imp := lines()
	if imp || opsDefault == 0 {
		t.Errorf("default: %d ops functions, imports %v: want the ops calls the charm makes, no imports", opsDefault, imp)
	}
	m = press(t, m, runes("."))
	nAll, _, totalAll, _ := lines()
	if totalAll != total || nAll <= nDefault {
		t.Errorf("all code: %d lines, total %d; default %d lines, total %d", nAll, totalAll, nDefault, total)
	}
	if view := m.overlay.View(); !strings.Contains(view, "all code") {
		t.Errorf("header doesn't say what is shown:\n%s", view)
	}
	m = press(t, m, runes("."))
	if n, _, _, _ := lines(); n != nDefault {
		t.Errorf(". again: %d lines, want the default's %d", n, nDefault)
	}
}

// TestProfileSearchAndPeek: / selects the first function matching, n moves to
// the next, enter lists what it called and was called by, and esc steps back
// out one narrowing at a time before leaving.
func TestProfileSearchAndPeek(t *testing.T) {
	m := profileOpen(t)
	m = press(t, m, runes("v"))
	m = press(t, m, runes("/"), runes("ExecProcess.wait"), tea.KeyMsg{Type: tea.KeyEnter})
	d, _ := m.flameData(m.currentEvent(), m.flameWidth())
	if fn := frameName(d.prof, m.flame.tsel); fn != "ExecProcess.wait" {
		t.Fatalf("search selected %q", fn)
	}
	if view := m.overlay.View(); !strings.Contains(view, "searching /ExecProcess.wait/") {
		t.Errorf("no search line:\n%s", view)
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	d, _ = m.flameData(m.currentEvent(), m.flameWidth())
	if d.peek == nil || len(d.peek.callers) == 0 || len(d.peek.callees) == 0 {
		t.Fatalf("peek %+v", d.peek)
	}
	view := m.overlay.View()
	for _, want := range []string{"what called and was called by ExecProcess.wait", "called by", "calls"} {
		if !strings.Contains(view, want) {
			t.Errorf("peek lacks %q:\n%s", want, view)
		}
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.flame.focus) != 0 || m.inspMode != inspFlame {
		t.Error("esc should leave the peek first")
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.flame.re != nil || m.inspMode != inspFlame {
		t.Error("esc should clear the search next")
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.inspMode != inspTrace || !m.overlayOn {
		t.Error("esc with nothing narrowed should go back to the trace")
	}
}

// TestFlameGraphScopes: tab cycles the flame graph through the hook, charm
// init and each observer that was sampled; init's graph holds init's
// samples only.
func TestFlameGraphScopes(t *testing.T) {
	m := profileOpen(t)
	var labels []string
	for _, s := range m.flame.scopes {
		labels = append(labels, s.label)
	}
	if len(labels) < 3 || labels[0] != "the whole hook" || labels[1] != "the charm's __init__" ||
		!strings.HasPrefix(labels[2], "observer ") {
		t.Fatalf("scopes %q", labels)
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyTab})
	d, _ := m.flameData(m.currentEvent(), m.flameWidth())
	trees, _ := m.hookTrees(m.currentEvent())
	var init *narrative.OpsNode
	for _, c := range trees[0].Children {
		if c.Kind == recording.OpsInit {
			init = c
		}
	}
	if init == nil || d.root.Total.Samples != init.Cost.Samples {
		t.Errorf("init scope has %d samples, init's cost %v", d.root.Total.Samples, init.Cost)
	}
}

// TestTraceRowDetails: the trace has a time and a memory column, shows the
// constructor where charm init is, and says what the function that waits
// waits for; `.` adds the CPU time and the marks that say how a figure was measured.
func TestTraceRowDetails(t *testing.T) {
	m := fixtureModel(t, "stacks")
	view := m.overlay.View()
	for _, want := range []string{"time", "mem Δ", "__init__"} {
		if !strings.Contains(view, want) {
			t.Errorf("trace lacks %q:\n%s", want, view)
		}
	}
	for _, hidden := range []string{"importing src", "charm init"} {
		if strings.Contains(view, hidden) {
			t.Errorf("trace shows %q:\n%s", hidden, view)
		}
	}
	d, _ := m.traceBuild(m.currentEvent())
	for i, ri := range d.vis {
		if d.tr.Rows[ri].Waiting {
			for range i {
				m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
			}
			break
		}
	}
	view = m.overlay.View()
	if !strings.Contains(view, "mostly waiting for Pebble") {
		t.Errorf("the waiting function doesn't say what it waits for:\n%s", view)
	}
	if strings.Contains(view, "cpu ") {
		t.Errorf("trace shows the CPU time before `.`:\n%s", view)
	}
	m = press(t, m, runes("."))
	m.overlay.GotoBottom() // the selected row's details are under all the rows
	if view = m.overlay.View(); !strings.Contains(view, "cpu ") {
		t.Errorf("`.` doesn't add the CPU time:\n%s", view)
	}
}

// TestTraceLevels: 1-9 open that many levels of the whole trace; + and - open
// or close one level under the selected row only, leaving the rest of the
// trace as it was; selection moves out of a row that closes.
func TestTraceLevels(t *testing.T) {
	m := fixtureModel(t, "stacks")
	vis := func() map[string]bool {
		d, _ := m.traceBuild(m.currentEvent())
		out := map[string]bool{}
		for _, ri := range d.vis {
			out[d.tr.Rows[ri].Key] = true
		}
		return out
	}
	all := vis()
	m = press(t, m, runes("1"))
	if n := len(vis()); n != 1 || m.trace.level != 1 {
		t.Fatalf("1: %d rows at level %d, want only the process", n, m.trace.level)
	}
	m = press(t, m, runes("3"))
	if n := len(vis()); n <= 1 || n >= len(all) || m.trace.level != 3 {
		t.Errorf("3: %d rows at level %d, of %d", n, m.trace.level, len(all))
	}
	m = press(t, m, runes("9"))
	if len(vis()) != len(all) || m.trace.level != 0 {
		t.Errorf("9: %d rows at level %d, want everything open", len(vis()), m.trace.level)
	}

	// Under the charm's __init__ (the second row): - closes a level at a time,
	// and + opens it again, without touching the rest of the trace.
	m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	d, _ := m.traceBuild(m.currentEvent())
	init := d.tr.Rows[d.vis[d.sel]]
	rows := []int{len(vis())}
	underInit := func() int {
		n := 0
		for key := range vis() {
			if strings.HasPrefix(key, init.Key+"/") {
				n++
			}
		}
		return n
	}
	for range 12 {
		if underInit() == 0 {
			break // shut: a further - would close the row it is under
		}
		m = press(t, m, runes("-"))
		rows = append(rows, len(vis()))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i] > rows[i-1] {
			t.Errorf("- grew the trace: %v", rows)
		}
	}
	if rows[1] >= rows[0] || rows[len(rows)-1] >= rows[1] {
		t.Errorf("- should close one level at a time until %s is shut: %v", init.Label, rows)
	}
	for key := range all {
		if !vis()[key] && !strings.HasPrefix(key, init.Key+"/") {
			t.Errorf("- closed %s, outside the selected row", key)
		}
	}
	if !vis()[init.Key] || len(vis()) <= 1 {
		t.Error("- shut the selected row's own row or the whole trace")
	}
	open := []int{len(vis())}
	for range 12 {
		m = press(t, m, runes("+"))
		open = append(open, len(vis()))
	}
	for i := 1; i < len(open); i++ {
		if open[i] < open[i-1] {
			t.Errorf("+ shrank the trace: %v", open)
		}
	}
	if open[1] <= open[0] || len(vis()) != len(all) {
		t.Errorf("+ should open one level at a time until all %d rows are back: %v", len(all), open)
	}

	// On a row with nothing open under it, - closes the section it is in: the
	// row it is under is folded and selected.
	m = press(t, m, runes("9"))
	d, _ = m.traceBuild(m.currentEvent())
	leaf := -1
	for i, ri := range d.vis {
		if !d.tr.Rows[ri].HasKids && d.tr.Rows[ri].Depth > 1 {
			leaf = i
		}
	}
	if leaf < 0 {
		t.Fatal("no leaf row")
	}
	for range leaf {
		m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	before := len(vis())
	m = press(t, m, runes("-"))
	d, _ = m.traceBuild(m.currentEvent())
	if after := len(vis()); after >= before || !d.tr.Rows[d.vis[d.sel]].HasKids {
		t.Errorf("- on a leaf: %d rows, was %d; selected %q", after, before, d.tr.Rows[d.vis[d.sel]].Label)
	}

	// Selection moves out of a row that closes.
	m = press(t, m, runes("9"), tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown})
	m = press(t, m, runes("1"))
	d, _ = m.traceBuild(m.currentEvent())
	if d.tr.Rows[d.vis[d.sel]].Depth != 0 {
		t.Errorf("selection stays on a hidden row at depth %d", d.tr.Rows[d.vis[d.sel]].Depth)
	}
}

// TestMemoryView: `m` says, for each container and the hook process, where
// memory started, how high it went and where it ended, draws the shape of
// each under the hook, and lists what took memory in the hook process with
// the observer or init and the charm's functions that were running.
func TestMemoryView(t *testing.T) {
	m := fixtureModel(t, "cost-k8s")
	m = press(t, m, runes("m"))
	view := m.overlay.View()
	for _, want := range []string{"memory on", "What happened", "before", "highest", "after", "charm container", "otelcol container", "hook process",
		"this hook ━", "Pebble change started", "What took memory in the hook process", "during the charm's __init__", "code:"} {
		if !strings.Contains(view, want) {
			t.Errorf("memory view lacks %q:\n%s", want, view)
		}
	}
	st := &m.mem
	if !st.to.After(st.hookEnd.Add(10 * time.Second)) {
		t.Errorf("readings end %v, hook %v: should run on past the hook", st.to, st.hookEnd)
	}
	// A jump names the charm function that was running.
	if !strings.Contains(view, "refresh_certs") && !strings.Contains(view, "forward_dashboards") && !strings.Contains(view, "compress") {
		t.Errorf("no jump names the code that took memory:\n%s", view)
	}
	if m = press(t, m, runes("p")); m.inspMode != inspFlame {
		t.Error("p from the memory view should open the profile")
	}
}

// TestTraceView: Enter opens the trace, with the memory of the hook process
// above and one bar per span and function; enter on a row folds it, and esc
// closes the inspector.
func TestTraceView(t *testing.T) {
	m := fixtureModel(t, "stacks")
	if m.inspMode != inspTrace {
		t.Fatal("Enter did not open the trace")
	}
	view := m.overlay.View()
	for _, want := range []string{"trace of config-changed", "memory of the hook process", "__init__", "refresh_certs", "GET /v1/changes/"} {
		if !strings.Contains(view, want) {
			t.Errorf("trace lacks %q:\n%s", want, view)
		}
	}
	d, _ := m.traceBuild(m.currentEvent())
	n := len(d.vis)
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if d, _ = m.traceBuild(m.currentEvent()); len(d.vis) != 1 {
		t.Errorf("enter on the process row left %d of %d rows, want it folded shut", len(d.vis), n)
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyEnter})
	if d, _ = m.traceBuild(m.currentEvent()); len(d.vis) >= n || len(d.vis) <= 1 {
		t.Errorf("enter on a row with children left %d of %d rows", len(d.vis), n)
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.overlayOn {
		t.Error("esc should close the inspector")
	}
}
