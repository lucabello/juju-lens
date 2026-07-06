// Package viewer implements the bubbletea TUI shown by `juju-lens view`.
//
// The layout is two rows, all synchronised to a single selected instant
// (VISION §6). The top row holds Events and Status side by side; the Logs
// stream spans the full width beneath them:
//
//   - [1] Events — the timeline of meaningful transitions (hooks, status
//     changes, relations), distilled from the raw RPC stream. The navigator.
//   - [3] Status — application/unit workload and agent status plus relations,
//     reconstructed at the selected instant (point-in-time by default), with
//     change pips marking what moved.
//   - [2] Logs   — every log source merged into one chronological stream with
//     the timeline's events dropped in as ruler lines. Locks to the selected
//     event by default; `f` frees it for lazyjournal-style scrolling.
//
// Long lines clip at the pane edge and scroll horizontally (←/→); `w` wraps
// them instead. The Model owns the SQLite handle and routes messages to the
// panes. Panes are focused by number (1/2/3) or cycled with Tab; `enter` opens
// an inspector overlay for the selected event (databags + the RPCs behind it).
package viewer

import (
	"fmt"
	"sort"
	"time"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Run opens the recording at dir and blocks in the TUI until the user quits.
// When follow is true the viewer tails the (possibly still-growing) recording,
// refreshing as `record`'s live indexer appends to index.db.
func Run(dir string, follow bool) error {
	man, err := recording.Load(dir)
	if err != nil {
		return fmt.Errorf("loading manifest at %s: %w", dir, err)
	}
	layout := recording.NewLayout(dir)
	db, err := index.Open(layout.IndexDB())
	if err != nil {
		return fmt.Errorf("opening index (run `juju-lens index %s`): %w", dir, err)
	}
	defer db.Close()

	models, err := db.Models()
	if err != nil {
		return fmt.Errorf("listing models: %w", err)
	}

	m := newModel(dir, man, db, models)
	m.follow = follow
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = prog.Run()
	return err
}

const followInterval = 2 * time.Second

type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(followInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// keymap groups the keybindings so bubbles/help can render them.
type keymap struct {
	Up, Down, PageUp, PageDown, Home, End key.Binding
	ScrollLeft, ScrollRight, Wrap         key.Binding
	Focus1, Focus2, Focus3, Tab, ShiftTab key.Binding
	Inspect, Free                         key.Binding
	Diff, CopyCur, CopyPrev               key.Binding
	Verbose, ModelPick, Quit              key.Binding
}

func defaultKeymap() keymap {
	return keymap{
		Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:      key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("PgUp", "page up")),
		PageDown:    key.NewBinding(key.WithKeys("pgdown", " ", "f"), key.WithHelp("PgDn", "page down")),
		Home:        key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "top")),
		End:         key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "bottom")),
		ScrollLeft:  key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "scroll left")),
		ScrollRight: key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "scroll right")),
		Wrap:        key.NewBinding(key.WithKeys("w"), key.WithHelp("w", "wrap")),
		Focus1:      key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "events")),
		Focus2:      key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "logs")),
		Focus3:      key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "status")),
		Tab:         key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "cycle")),
		ShiftTab:    key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("⇧Tab", "cycle back")),
		Inspect:     key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "inspect")),
		Free:        key.NewBinding(key.WithKeys("f"), key.WithHelp("f", "free-scroll")),
		Diff:        key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "diff")),
		CopyCur:     key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy after")),
		CopyPrev:    key.NewBinding(key.WithKeys("Y"), key.WithHelp("Y", "copy before")),
		Verbose:     key.NewBinding(key.WithKeys("."), key.WithHelp(".", "verbose")),
		ModelPick:   key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "model")),
		Quit:        key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

func (k keymap) ShortHelp() []key.Binding {
	return []key.Binding{k.Focus1, k.Focus2, k.Focus3, k.Inspect, k.Verbose, k.ModelPick, k.Quit}
}

func (k keymap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.PageUp, k.PageDown, k.Home, k.End},
		{k.ScrollLeft, k.ScrollRight, k.Wrap, k.Focus1, k.Focus2, k.Focus3, k.Tab, k.ShiftTab, k.Free},
		{k.Inspect, k.Verbose, k.ModelPick, k.Quit},
	}
}

// paneID identifies the focusable column.
type paneID int

const (
	paneEvents paneID = iota
	paneLogs
	paneStatus
)

type model struct {
	dir      string
	manifest *recording.Manifest
	db       *index.DB

	allModels   []index.Model
	activeModel index.Model

	// Events (top-left) — the navigator.
	spans        []recording.SpanRow
	allEvents    []event // hooks + raw transitions, unfiltered
	events       []event // the visible slice: hooks only, unless verbose
	cursor       int
	verbose      bool            // `.`: reveal the raw RPC transitions behind the hooks
	databagSpans map[string]bool // span ids that changed a databag (M7 marker)

	// Logs (bottom) — the merged stream.
	stream         []streamItem
	eventStreamIdx []int // events[i] -> index into stream
	logCursor      int   // free-scroll position into stream
	logFree        bool  // decoupled from the event cursor
	logSource      string

	// Status (top-right).
	appTree       appTree
	appStatuses   map[string]statusValue
	unitStatuses  map[string]statusValue
	agentStatuses map[string]statusValue
	relations      []relationSummary
	databags       map[string]index.SnapshotRow
	relCursor      int
	relationBroken map[string][]time.Time // relation key -> relation-broken timestamps

	// Inspector overlay.
	overlayOn bool
	overlay   viewport.Model
	diffMode  bool   // `d`: overlay a git-style diff on the pretty-printed value
	copyCur   string // current databag/config JSON for `y`
	copyPrev  string // previous databag/config JSON for `Y`

	follow bool
	atTail bool

	focus   paneID
	width   int
	height  int
	hScroll int  // horizontal scroll offset for the focused pane
	wrap    bool // `w`: wrap long lines instead of clipping/scrolling
	help    help.Model
	keys    keymap
	ready   bool

	picker *modelPicker
}

func newModel(dir string, man *recording.Manifest, db *index.DB, models []index.Model) *model {
	m := &model{
		dir:       dir,
		manifest:  man,
		db:        db,
		allModels: models,
		overlay:   viewport.New(0, 0),
		help:      help.New(),
		keys:      defaultKeymap(),
		focus:     paneEvents,
	}
	if len(models) > 1 {
		m.picker = newModelPicker(models)
	} else if len(models) == 1 {
		m.setActiveModel(models[0])
	}
	return m
}

// setActiveModel refreshes every derived view when the model context changes.
func (m *model) setActiveModel(mm index.Model) {
	m.activeModel = mm
	spans, err := m.db.Spans(mm.ID)
	if err != nil {
		spans = nil
	}
	m.spans = spans
	m.databagSpans, _ = m.db.ProducingSpanIDs(mm.ID, string(index.KindDatabag))
	m.allEvents = buildEvents(spans, m.databagSpans)
	m.events = nil // force applyVerboseFilter to select the newest visible event
	m.appTree = buildAppTree(spans)
	m.relationBroken = relationBrokenTimes(spans)
	m.applyVerboseFilter()
}

// applyVerboseFilter recomputes the visible event slice from allEvents,
// honouring the `.` toggle (default: hooks only; verbose: raw transitions too).
// It keeps the selection pinned to the same event when possible — and lands on
// the newest visible event when there was no prior selection. Every derived view
// (log stream, status pane) is refreshed to the new selection.
func (m *model) applyVerboseFilter() {
	prev := ""
	if m.cursor >= 0 && m.cursor < len(m.events) {
		prev = m.events[m.cursor].ident()
	}
	var visible []event
	for _, ev := range m.allEvents {
		if m.verbose || !ev.verboseOnly {
			visible = append(visible, ev)
		}
	}
	m.events = visible
	if prev == "" {
		m.cursor = max(0, len(visible)-1)
	} else {
		m.cursor = clamp(m.cursor, 0, max(0, len(visible)-1))
		for i, ev := range visible {
			if ev.ident() == prev {
				m.cursor = i
				break
			}
		}
	}
	m.buildStream()
	m.refreshStatus()
}

// currentEvent returns the selected event, or a zero event when empty.
func (m *model) currentEvent() event {
	if len(m.events) == 0 {
		return event{}
	}
	return m.events[m.cursor]
}

// currentTs is the instant every synchronised pane reflects.
func (m *model) currentTs() time.Time {
	if len(m.events) > 0 {
		return m.events[m.cursor].ts
	}
	if len(m.spans) > 0 {
		return m.spans[len(m.spans)-1].Start
	}
	return time.Now()
}

// spanByID looks up a span by id (for the inspector).
func (m *model) spanByID(id string) (recording.SpanRow, bool) {
	for _, sp := range m.spans {
		if sp.SpanID == id {
			return sp, true
		}
	}
	return recording.SpanRow{}, false
}

// refreshStatus recomputes the Status pane, always point-in-time: the state as
// of the selected event, so the pane follows the cursor.
func (m *model) refreshStatus() {
	rows := func(kind string) []index.SnapshotRow {
		if len(m.events) > 0 {
			r, _ := m.db.LatestPerScopeAsOf(m.activeModel.ID, kind, m.currentTs())
			return r
		}
		r, _ := m.db.LatestPerScope(m.activeModel.ID, kind)
		return r
	}
	m.appStatuses = scopeMap(rows(string(index.KindAppStatus)), "app-status:")
	m.unitStatuses = scopeMap(rows(string(index.KindUnitStatus)), "unit-status:")
	m.agentStatuses = scopeMap(rows(string(index.KindAgentStatus)), "agent-status:")
	databagRows := rows(string(index.KindDatabag))
	m.relations = m.currentRelations(buildRelations(databagRows), latestWriteByRelation(databagRows), m.currentTs())
	m.databags = map[string]index.SnapshotRow{}
	for _, r := range databagRows {
		m.databags[r.Scope] = r
	}
	if m.relCursor >= len(m.relations) {
		m.relCursor = max(0, len(m.relations)-1)
	}
}

// currentRelations drops relations that don't exist at instant now. A databag
// lingers in the index after its relation is gone, so existence is decided by
// comparing signals that are reliably attributed to a relation key: a relation
// is gone when its most recent relation-broken is newer than its most recent
// databag write. A later databag write (the relation-changed of a re-add) brings
// it back — which is what makes remove-then-re-add cycles track correctly, even
// for relations created mid-recording (whose relation-created hook, unlike
// relation-broken, rarely carries the key).
func (m *model) currentRelations(rels []relationSummary, lastWrite map[string]time.Time, now time.Time) []relationSummary {
	var out []relationSummary
	for _, r := range rels {
		broken := m.lastBrokenAsOf(r.Key, now)
		if !broken.IsZero() && broken.After(lastWrite[r.Key]) {
			continue // broken more recently than any databag write: gone
		}
		out = append(out, r)
	}
	return out
}

// lastBrokenAsOf returns the newest relation-broken time for key at or before t,
// or the zero time when none.
func (m *model) lastBrokenAsOf(key string, t time.Time) time.Time {
	var last time.Time
	for _, bt := range m.relationBroken[key] {
		if bt.After(t) {
			break
		}
		last = bt
	}
	return last
}

// relationBrokenTimes records, per canonical relation key, the timestamps of its
// relation-broken hooks (the point the relation ceased to exist), sorted. Unlike
// relation-created, relation-broken reliably carries the relation key.
func relationBrokenTimes(spans []recording.SpanRow) map[string][]time.Time {
	out := map[string][]time.Time{}
	for _, sp := range spans {
		if sp.Hook == "relation-broken" && sp.Relation != "" {
			out[sp.Relation] = append(out[sp.Relation], sp.Start)
		}
	}
	for _, ts := range out {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	}
	return out
}

// latestWriteByRelation returns, per relation key, the newest databag-write
// timestamp among the snapshot rows given (already point-in-time as of the
// cursor). It is the "relation is live" signal paired with relationBrokenTimes.
func latestWriteByRelation(rows []index.SnapshotRow) map[string]time.Time {
	out := map[string]time.Time{}
	for _, r := range rows {
		key, _ := splitDatabagScope(r.Scope)
		if key == "" {
			continue
		}
		if t, ok := out[key]; !ok || r.Ts.After(t) {
			out[key] = r.Ts
		}
	}
	return out
}

func (m *model) Init() tea.Cmd {
	if m.follow {
		return tickCmd()
	}
	return nil
}

// reload re-reads the index for the active model, preserving the selected event
// (following the tail when the cursor was already newest).
func (m *model) reload() {
	if models, err := m.db.Models(); err == nil && len(models) > len(m.allModels) {
		m.allModels = models
		if m.activeModel.ID == 0 && len(models) > 0 {
			m.setActiveModel(models[0])
			return
		}
	}
	if m.activeModel.ID == 0 {
		return
	}
	prevLen := len(m.events)
	prevID := ""
	if m.cursor < len(m.events) {
		prevID = m.events[m.cursor].spanID
	}
	m.setActiveModel(m.activeModel)
	switch {
	case m.atTail || prevLen == 0:
		m.cursor = max(0, len(m.events)-1)
	default:
		m.cursor = min(m.cursor, max(0, len(m.events)-1))
		for i, ev := range m.events {
			if ev.spanID == prevID {
				m.cursor = i
				break
			}
		}
	}
	m.refreshStatus()
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		if m.picker == nil {
			m.atTail = len(m.events) == 0 || m.cursor == len(m.events)-1
			m.reload()
		}
		return m, tickCmd()
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		m.ready = true
	case tea.KeyMsg:
		if m.picker != nil {
			done, chosen := m.picker.update(msg)
			if done {
				m.picker = nil
				if chosen != nil {
					m.setActiveModel(*chosen)
				} else if m.activeModel.ID == 0 && len(m.allModels) > 0 {
					m.setActiveModel(m.allModels[0])
				}
			}
			return m, nil
		}
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The inspector overlay captures keys while open.
	if m.overlayOn {
		switch {
		case key.Matches(msg, m.keys.Quit), key.Matches(msg, m.keys.Inspect):
			m.overlayOn = false
		case key.Matches(msg, m.keys.Diff):
			m.diffMode = !m.diffMode
			m.renderOverlay()
		case key.Matches(msg, m.keys.CopyCur):
			return m, copyCmd(m.copyCur)
		case key.Matches(msg, m.keys.CopyPrev):
			return m, copyCmd(m.copyPrev)
		default:
			var cmd tea.Cmd
			m.overlay, cmd = m.overlay.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, m.keys.Focus1):
		m.setFocus(paneEvents)
		return m, nil
	case key.Matches(msg, m.keys.Focus2):
		m.setFocus(paneLogs)
		return m, nil
	case key.Matches(msg, m.keys.Focus3):
		m.setFocus(paneStatus)
		return m, nil
	case key.Matches(msg, m.keys.Tab):
		m.cycleFocus(+1)
		return m, nil
	case key.Matches(msg, m.keys.ShiftTab):
		m.cycleFocus(-1)
		return m, nil
	case key.Matches(msg, m.keys.Wrap):
		m.wrap = !m.wrap
		m.hScroll = 0
		return m, nil
	case key.Matches(msg, m.keys.ScrollLeft):
		m.hScroll = max(0, m.hScroll-8)
		return m, nil
	case key.Matches(msg, m.keys.ScrollRight):
		if !m.wrap {
			m.hScroll += 8
		}
		return m, nil
	case key.Matches(msg, m.keys.ModelPick):
		if len(m.allModels) > 1 {
			m.picker = newModelPicker(m.allModels)
		}
		return m, nil
	case key.Matches(msg, m.keys.Inspect):
		// Only events that actually have something to show — a databag change or
		// a config change — are inspectable.
		if inspectable(m.currentEvent()) {
			m.overlayOn = true
			m.renderOverlay()
			m.overlay.GotoTop()
		}
		return m, nil
	case key.Matches(msg, m.keys.Verbose):
		m.verbose = !m.verbose
		m.applyVerboseFilter()
		return m, nil
	case key.Matches(msg, m.keys.Free):
		if m.focus == paneLogs {
			m.logFree = !m.logFree
			if m.logFree {
				m.logCursor = m.lockedStreamIdx()
			}
			return m, nil
		}
	}

	// Per-pane navigation.
	switch m.focus {
	case paneStatus:
		m.moveRelCursor(navDelta(msg, m.keys))
	case paneLogs:
		if m.logFree {
			m.moveLogCursor(navDelta(msg, m.keys))
		} else {
			m.moveCursor(navDelta(msg, m.keys)) // locked: scrolling logs moves the event cursor
		}
	default:
		m.moveCursor(navDelta(msg, m.keys))
	}
	return m, nil
}

// navDelta maps a key to a cursor delta (0 when it's not a movement key).
func navDelta(msg tea.KeyMsg, k keymap) int {
	switch {
	case key.Matches(msg, k.Up):
		return -1
	case key.Matches(msg, k.Down):
		return +1
	case key.Matches(msg, k.PageUp):
		return -8
	case key.Matches(msg, k.PageDown):
		return +8
	case key.Matches(msg, k.Home):
		return -1 << 30
	case key.Matches(msg, k.End):
		return +1 << 30
	}
	return 0
}

// focusOrder is the visual order Tab cycles through: the two top panes left to
// right, then the Logs stream beneath — Events, Status, Logs.
var focusOrder = []paneID{paneEvents, paneStatus, paneLogs}

// cycleFocus moves focus dir steps along focusOrder (+1 Tab, -1 Shift+Tab).
func (m *model) cycleFocus(dir int) {
	idx := 0
	for i, p := range focusOrder {
		if p == m.focus {
			idx = i
			break
		}
	}
	n := len(focusOrder)
	m.setFocus(focusOrder[(idx+dir+n)%n])
}

// setFocus switches the active pane, resetting the horizontal scroll so each
// pane starts from column zero rather than inheriting the previous pane's offset.
func (m *model) setFocus(p paneID) {
	if p != m.focus {
		m.hScroll = 0
	}
	m.focus = p
}

func (m *model) moveCursor(delta int) {
	if delta == 0 || len(m.events) == 0 {
		return
	}
	m.cursor = clamp(m.cursor+delta, 0, len(m.events)-1)
	m.refreshStatus() // the Status pane always follows the cursor
}

func (m *model) moveLogCursor(delta int) {
	if delta == 0 || len(m.stream) == 0 {
		return
	}
	m.logCursor = clamp(m.logCursor+delta, 0, len(m.stream)-1)
}

func (m *model) moveRelCursor(delta int) {
	if delta == 0 || len(m.relations) == 0 {
		return
	}
	m.relCursor = clamp(m.relCursor+delta, 0, len(m.relations)-1)
}

func (m *model) View() string {
	if !m.ready {
		return "loading recording..."
	}
	if m.picker != nil {
		return m.picker.view(m.width, m.height)
	}
	header := m.renderHeader()
	footer := m.help.View(m.keys)

	if m.overlayOn {
		return lipgloss.JoinVertical(lipgloss.Left, header, m.renderOverlayFrame(), footer)
	}

	// Two rows: Events + Status on top, the full-width Logs stream beneath.
	top := lipgloss.JoinHorizontal(lipgloss.Top, m.renderEventsPane(), m.renderStatusPane())
	body := lipgloss.JoinVertical(lipgloss.Left, top, m.renderLogPane())
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *model) renderHeader() string {
	activeName := "?"
	if m.activeModel.Name != "" {
		activeName = m.activeModel.Name
	}
	modelsSummary := activeName
	if len(m.allModels) > 1 {
		modelsSummary = fmt.Sprintf("%s (of %d — press m)", activeName, len(m.allModels))
	}
	controller := "?"
	if m.manifest != nil {
		controller = m.manifest.Controller.Name
	}
	live := ""
	if m.follow && m.atTail {
		live = "  " + styleErr.Render("● LIVE")
	}
	title := fmt.Sprintf("juju-lens · %s · model: %s · %d events · @%s%s",
		controller, modelsSummary, len(m.events), m.currentTs().UTC().Format("15:04:05.000"), live)
	return styleHeader.Width(m.width).Render(title)
}

// Styles
var (
	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Padding(0, 1).
			Border(lipgloss.RoundedBorder(), false, false, true, false)
	styleBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)
	styleSelected = lipgloss.NewStyle().Reverse(true)
	styleHook     = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleUnit     = lipgloss.NewStyle().Foreground(lipgloss.Color("4"))
	styleApp      = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
	styleDim      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	// styleText is the primary body-text colour. It is set explicitly (rather
	// than relying on the terminal default) so rows always contrast the pane
	// background on both light and dark themes.
	styleText    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "235", Dark: "252"})
	styleSource  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	// styleDel is the removed-line colour in the inspector diff: red, but not
	// bold — the colour alone carries the meaning.
	styleDel = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleOK  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleRuler   = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
	styleSection = lipgloss.NewStyle().Bold(true).Underline(true)
)

// boxFor returns the pane box, highlighting its border when focused. The border
// colour is always set explicitly so the title spliced into the top border (see
// spliceTitle) can match the corners it rebuilds.
func (m *model) boxFor(p paneID) lipgloss.Style {
	c := lipgloss.Color("8")
	if m.focus == p {
		c = lipgloss.Color("4")
	}
	return styleBox.BorderForeground(c)
}

func withDim(s string) string {
	if s == "" {
		return styleDim.Render("(none)")
	}
	return s
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
