// Package viewer implements the bubbletea TUI shown by `juju-lens view`.
//
// M2 introduces a three-column layout backed by the SQLite index:
//
//   - Left column   — apps sidebar (apps -> units tree).
//   - Centre column — timeline of spans (top) and details of the selection
//     (bottom).
//   - Right column  — status pane with Applications, Units and Relations
//     sections. By default it shows the latest known snapshot per scope;
//     `s` switches to point-in-time (state as of the timeline cursor). The
//     Relations section is focusable (Tab) to inspect a relation's databags.
//
// The Model owns the SQLite handle and routes messages to the individual
// panes; each pane is a small struct with its own render function. Queries
// happen on the event-loop goroutine for now because M2 recordings are
// small; later milestones will move heavy queries into commands.
package viewer

import (
	"fmt"
	"sort"

	"github.com/lucabello/juju-lens/internal/index"
	"github.com/lucabello/juju-lens/internal/recording"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Run opens the recording at dir and blocks in the TUI until the user
// quits. It returns nil on a clean exit.
func Run(dir string) error {
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
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = prog.Run()
	return err
}

// keymap groups the keybindings so bubbles/help can render them.
type keymap struct {
	Up, Down, PageUp, PageDown, Home, End key.Binding
	Tab, ModelPick, PointInTime, Diff     key.Binding
	Quit                                  key.Binding
}

func defaultKeymap() keymap {
	return keymap{
		Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:      key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("PgUp", "page up")),
		PageDown:    key.NewBinding(key.WithKeys("pgdown", " ", "f"), key.WithHelp("PgDn", "page down")),
		Home:        key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("Home", "top")),
		End:         key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("End", "bottom")),
		Tab:         key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "cycle focus")),
		ModelPick:   key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "pick model")),
		PointInTime: key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "status @cursor")),
		Diff:        key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "databag diff")),
		Quit:        key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

func (k keymap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Tab, k.PointInTime, k.Diff, k.ModelPick, k.Quit}
}

func (k keymap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Up, k.Down, k.PageUp, k.PageDown, k.Home, k.End},
		{k.Tab, k.PointInTime, k.Diff, k.ModelPick, k.Quit}}
}

// paneID identifies the focusable centre-column selection surface. The
// sidebars render but do not (yet) take focus in M2; the timeline drives
// all queries.
type paneID int

const (
	paneTimeline  paneID = iota
	paneDetails          // scrollable details/logs pane below the timeline
	paneRelations        // navigable relations list in the status column
)

type model struct {
	dir      string
	manifest *recording.Manifest
	db       *index.DB

	allModels    []index.Model
	activeModel  index.Model // zero-value means "all models"
	spans        []recording.SpanRow
	appTree      appTree
	appStatuses  map[string]statusValue       // app -> status (latest, or as-of cursor in pit mode)
	unitStatuses map[string]statusValue       // unit -> status
	relations    []relationSummary            // relation databags (as-of cursor in pit mode)
	databags     map[string]index.SnapshotRow // scope -> latest/as-of databag row
	relCursor    int                          // selected relation when the Relations pane is focused
	pit          bool                         // point-in-time: status/relations as of the cursor
	diff         bool                         // databag diff mode in the details pane

	cursor  int
	focus   paneID
	width   int
	height  int
	details viewport.Model
	help    help.Model
	keys    keymap
	ready   bool

	picker *modelPicker // non-nil while the picker overlay is active
}

func newModel(dir string, man *recording.Manifest, db *index.DB, models []index.Model) *model {
	m := &model{
		dir:       dir,
		manifest:  man,
		db:        db,
		allModels: models,
		details:   viewport.New(0, 0),
		help:      help.New(),
		keys:      defaultKeymap(),
		focus:     paneTimeline,
	}
	// Auto-open the picker if there is more than one model, otherwise
	// select the single one silently.
	if len(models) > 1 {
		m.picker = newModelPicker(models)
	} else if len(models) == 1 {
		m.setActiveModel(models[0])
	}
	return m
}

// setActiveModel refreshes every derived view when the model context
// changes (recording load, model picker selection, etc.).
func (m *model) setActiveModel(mm index.Model) {
	m.activeModel = mm
	spans, err := m.db.Spans(mm.ID)
	if err != nil {
		spans = nil
	}
	m.spans = spans
	m.cursor = 0
	m.appTree = buildAppTree(spans)
	m.refreshStatus()
	m.refreshDetails()
}

// refreshStatus recomputes the Applications/Units/Relations panes. In
// point-in-time mode (toggled with `s`) it asks for the state as it stood at
// the selected span's timestamp; otherwise it shows the latest known value.
func (m *model) refreshStatus() {
	rows := func(kind string) []index.SnapshotRow {
		if m.pit && len(m.spans) > 0 {
			r, _ := m.db.LatestPerScopeAsOf(m.activeModel.ID, kind, m.currentSpan().Start)
			return r
		}
		r, _ := m.db.LatestPerScope(m.activeModel.ID, kind)
		return r
	}
	m.appStatuses = scopeMap(rows(string(index.KindAppStatus)), "app-status:")
	m.unitStatuses = scopeMap(rows(string(index.KindUnitStatus)), "unit-status:")
	databagRows := rows(string(index.KindDatabag))
	m.relations = buildRelations(databagRows)
	m.databags = map[string]index.SnapshotRow{}
	for _, r := range databagRows {
		m.databags[r.Scope] = r
	}
	if m.relCursor >= len(m.relations) {
		m.relCursor = max(0, len(m.relations)-1)
	}
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		m.refreshDetails()
		m.ready = true
	case tea.KeyMsg:
		// The picker eats every keystroke while it's up.
		if m.picker != nil {
			done, chosen := m.picker.update(msg)
			if done {
				m.picker = nil
				if chosen != nil {
					m.setActiveModel(*chosen)
				} else if m.activeModel.ID == 0 && len(m.allModels) > 0 {
					// User cancelled without ever choosing; fall back
					// to the first model so the TUI has something to
					// show.
					m.setActiveModel(m.allModels[0])
				}
			}
			return m, nil
		}
		// Tab cycles focus: timeline -> details -> relations -> timeline
		// (relations is skipped when the recording has none).
		if key.Matches(msg, m.keys.Tab) {
			m.cycleFocus()
			return m, nil
		}
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.ModelPick):
			if len(m.allModels) > 1 {
				m.picker = newModelPicker(m.allModels)
			}
			return m, nil
		case key.Matches(msg, m.keys.PointInTime):
			// Toggle point-in-time: status/relations as of the selected span.
			m.pit = !m.pit
			m.refreshStatus()
			return m, nil
		case key.Matches(msg, m.keys.Diff):
			// Toggle databag diff mode in the details pane.
			m.diff = !m.diff
			m.refreshDetails()
			return m, nil
		}
		// When the details pane is focused, navigation keys scroll it instead of
		// moving the timeline cursor (so long details + correlated logs are
		// reachable). Otherwise they drive the timeline selection.
		if m.focus == paneDetails {
			var cmd tea.Cmd
			m.details, cmd = m.details.Update(msg)
			return m, cmd
		}
		// The Relations pane, when focused, takes ↑/↓ to move its selection and
		// shows the chosen relation's databags in the details pane.
		if m.focus == paneRelations {
			switch {
			case key.Matches(msg, m.keys.Up):
				m.moveRelCursor(-1)
			case key.Matches(msg, m.keys.Down):
				m.moveRelCursor(+1)
			}
			return m, nil
		}
		switch {
		case key.Matches(msg, m.keys.Up):
			m.moveCursor(-1)
		case key.Matches(msg, m.keys.Down):
			m.moveCursor(+1)
		case key.Matches(msg, m.keys.PageUp):
			m.moveCursor(-max(1, m.timelineHeight()-1))
		case key.Matches(msg, m.keys.PageDown):
			m.moveCursor(+max(1, m.timelineHeight()-1))
		case key.Matches(msg, m.keys.Home):
			m.cursor = 0
			m.refreshDetails()
			if m.pit {
				m.refreshStatus()
			}
		case key.Matches(msg, m.keys.End):
			m.cursor = len(m.spans) - 1
			m.refreshDetails()
			if m.pit {
				m.refreshStatus()
			}
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.details, cmd = m.details.Update(msg)
	return m, cmd
}

func (m *model) moveCursor(delta int) {
	if len(m.spans) == 0 {
		m.cursor = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.spans) {
		m.cursor = len(m.spans) - 1
	}
	m.refreshDetails()
	// In point-in-time mode the Status/Relations panes track the cursor.
	if m.pit {
		m.refreshStatus()
	}
}

// cycleFocus advances the focused pane: timeline -> details -> relations ->
// timeline, skipping the relations pane when there are no relations. The
// details pane re-renders so it reflects the newly focused mode (span vs
// relation databags).
func (m *model) cycleFocus() {
	switch m.focus {
	case paneTimeline:
		m.focus = paneDetails
	case paneDetails:
		if len(m.relations) > 0 {
			m.focus = paneRelations
		} else {
			m.focus = paneTimeline
		}
	default:
		m.focus = paneTimeline
	}
	m.refreshDetails()
}

// moveRelCursor changes the selected relation and refreshes the details pane
// (which shows that relation's databags while the Relations pane is focused).
func (m *model) moveRelCursor(delta int) {
	if len(m.relations) == 0 {
		return
	}
	m.relCursor += delta
	if m.relCursor < 0 {
		m.relCursor = 0
	}
	if m.relCursor >= len(m.relations) {
		m.relCursor = len(m.relations) - 1
	}
	m.refreshDetails()
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

	if len(m.spans) == 0 {
		body := lipgloss.NewStyle().
			Padding(2, 4).
			Render(fmt.Sprintf("Recording contains no spans yet.\n\nDirectory: %s", m.dir))
		return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
	}

	apps := m.renderAppsPane()
	centre := m.renderCentreColumn()
	status := m.renderStatusPane()
	body := lipgloss.JoinHorizontal(lipgloss.Top, apps, centre, status)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *model) renderHeader() string {
	activeName := "?"
	if m.activeModel.Name != "" {
		activeName = m.activeModel.Name
	}
	allNames := []string{}
	for _, mm := range m.allModels {
		allNames = append(allNames, mm.Name)
	}
	sort.Strings(allNames)
	modelsSummary := activeName
	if len(m.allModels) > 1 {
		modelsSummary = fmt.Sprintf("%s (of %d — press m)", activeName, len(m.allModels))
	}
	controller := "?"
	if m.manifest != nil {
		controller = m.manifest.Controller.Name
	}
	title := fmt.Sprintf("juju-lens · %s · model: %s · spans: %d",
		controller, modelsSummary, len(m.spans))
	return styleHeader.Width(m.width).Render(title)
}

// currentSpan returns the span under the cursor, or the zero SpanRow when
// the recording is empty.
func (m *model) currentSpan() recording.SpanRow {
	if len(m.spans) == 0 {
		return recording.SpanRow{}
	}
	return m.spans[m.cursor]
}

// Styles

var (
	styleHeader = lipgloss.NewStyle().
			Bold(true).
			Padding(0, 1).
			Border(lipgloss.RoundedBorder(), false, false, true, false)
	styleTimelineBox = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				Padding(0, 1)
	styleDetailsBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)
	styleAppsBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)
	styleStatusBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			Padding(0, 1)
	styleSelected = lipgloss.NewStyle().
			Reverse(true)
	styleHook = lipgloss.NewStyle().
			Foreground(lipgloss.Color("6"))
	styleUnit = lipgloss.NewStyle().
			Foreground(lipgloss.Color("4"))
	styleApp = lipgloss.NewStyle().
			Foreground(lipgloss.Color("5")).
			Bold(true)
	styleDim = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))
	styleErr = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1")).
			Bold(true)
	styleOK = lipgloss.NewStyle().
		Foreground(lipgloss.Color("2"))
	styleWarn = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3"))
	styleSection = lipgloss.NewStyle().
			Bold(true).
			Underline(true)
)

func withDim(s string) string {
	if s == "" {
		return styleDim.Render("(none)")
	}
	return s
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
