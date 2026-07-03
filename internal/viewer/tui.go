// Package viewer implements the bubbletea TUI shown by `juju-lens view`.
//
// M1 keeps it deliberately minimal: a scrollable timeline of every span in
// the recording, with a details pane that shows the selected span's
// attributes and a manifest summary. The layout is a two-pane split so we
// have somewhere to grow: later milestones will fill the sidebars with
// applications, machines, models, relations, and correlated logs.
package viewer

import (
	"fmt"
	"strings"
	"time"

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
	spans, err := recording.LoadSpans(dir)
	if err != nil {
		return fmt.Errorf("loading spans: %w", err)
	}
	m := newModel(dir, man, spans)
	prog := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err = prog.Run()
	return err
}

// keymap groups the keybindings so bubbles/help can render them.
type keymap struct {
	Up, Down, PageUp, PageDown, Home, End, Quit key.Binding
}

func defaultKeymap() keymap {
	return keymap{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "b"), key.WithHelp("PgUp", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", " ", "f"), key.WithHelp("PgDn", "page down")),
		Home:     key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("Home", "top")),
		End:      key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("End", "bottom")),
		Quit:     key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

func (k keymap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.PageDown, k.Home, k.End, k.Quit}
}

func (k keymap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Up, k.Down, k.PageUp, k.PageDown, k.Home, k.End, k.Quit}}
}

type model struct {
	dir      string
	manifest *recording.Manifest
	spans    []recording.SpanRow

	cursor  int
	width   int
	height  int
	details viewport.Model
	help    help.Model
	keys    keymap
	ready   bool
}

func newModel(dir string, man *recording.Manifest, spans []recording.SpanRow) *model {
	return &model{
		dir:      dir,
		manifest: man,
		spans:    spans,
		details:  viewport.New(0, 0),
		help:     help.New(),
		keys:     defaultKeymap(),
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
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
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
		case key.Matches(msg, m.keys.End):
			m.cursor = len(m.spans) - 1
			m.refreshDetails()
		}
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
}

func (m *model) layout() {
	m.details.Width = detailsWidth(m.width)
	m.details.Height = max(1, m.height-headerHeight-footerHeight-1)
}

// timelineWidth / detailsWidth pick a 55/45 split for the two panes with
// sensible minima. All numbers include the box borders.
func timelineWidth(total int) int {
	w := (total * 55) / 100
	if w < 30 {
		w = 30
	}
	if w > total-20 {
		w = total - 20
	}
	if w < 0 {
		w = 0
	}
	return w
}

func detailsWidth(total int) int {
	w := total - timelineWidth(total)
	if w < 0 {
		w = 0
	}
	return w
}

const (
	headerHeight = 3 // one line of title + top border/padding
	footerHeight = 2 // one line of help + separator
)

func (m *model) timelineHeight() int {
	return max(1, m.height-headerHeight-footerHeight-1)
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
	styleSelected = lipgloss.NewStyle().
			Reverse(true)
	styleHook = lipgloss.NewStyle().
			Foreground(lipgloss.Color("6"))
	styleUnit = lipgloss.NewStyle().
			Foreground(lipgloss.Color("4"))
	styleDim = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))
	styleErr = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1")).
			Bold(true)
)

func (m *model) View() string {
	if !m.ready {
		return "loading recording..."
	}
	if len(m.spans) == 0 {
		return m.emptyView()
	}
	header := m.renderHeader()
	timeline := m.renderTimeline()
	details := m.renderDetails()
	body := lipgloss.JoinHorizontal(lipgloss.Top, timeline, details)
	footer := m.help.View(m.keys)
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

func (m *model) emptyView() string {
	msg := lipgloss.NewStyle().
		Padding(2, 4).
		Render(fmt.Sprintf("Recording contains no spans yet.\n\nDirectory: %s", m.dir))
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(),
		msg,
		m.help.View(m.keys),
	)
}

func (m *model) renderHeader() string {
	models := "?"
	if m.manifest != nil {
		names := []string{}
		for _, x := range m.manifest.Models {
			names = append(names, x.Name)
		}
		if len(names) > 0 {
			models = strings.Join(names, ", ")
		}
	}
	controller := "?"
	if m.manifest != nil {
		controller = m.manifest.Controller.Name
	}
	title := fmt.Sprintf("juju-lens · %s · models: %s · spans: %d",
		controller, models, len(m.spans))
	return styleHeader.Width(m.width).Render(title)
}

func (m *model) renderTimeline() string {
	w := timelineWidth(m.width)
	h := m.timelineHeight()
	rows := make([]string, 0, len(m.spans))

	// Compute a sliding window so the cursor is visible.
	start := 0
	if m.cursor >= h {
		start = m.cursor - h + 1
	}
	end := start + h
	if end > len(m.spans) {
		end = len(m.spans)
	}

	for i := start; i < end; i++ {
		sp := m.spans[i]
		hook := sp.Hook
		if hook == "" {
			hook = "-"
		}
		unit := sp.Unit
		if unit == "" {
			unit = "?"
		}
		line := fmt.Sprintf("%s %-14s %s %s",
			formatTime(sp.Start),
			styleUnit.Render(truncate(unit, 14)),
			styleHook.Render(truncate(hook, 22)),
			truncate(sp.Name, max(0, w-52)),
		)
		if i == m.cursor {
			line = styleSelected.Width(max(0, w-4)).Render(line)
		}
		rows = append(rows, line)
	}

	// Pad the box to the requested height so the layout doesn't reflow.
	for len(rows) < h {
		rows = append(rows, "")
	}
	inner := strings.Join(rows, "\n")
	return styleTimelineBox.Width(w).Height(h + 2).Render(inner)
}

func (m *model) renderDetails() string {
	w := detailsWidth(m.width)
	h := m.timelineHeight()
	m.details.Width = max(0, w-4)
	m.details.Height = h
	return styleDetailsBox.Width(w).Height(h + 2).Render(m.details.View())
}

func (m *model) refreshDetails() {
	if len(m.spans) == 0 {
		m.details.SetContent("")
		return
	}
	sp := m.spans[m.cursor]
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", lipgloss.NewStyle().Bold(true).Render(sp.Name))
	fmt.Fprintf(&b, "  span:     %s\n", sp.SpanID)
	fmt.Fprintf(&b, "  trace:    %s\n", sp.TraceID)
	if sp.ParentSpanID != "" {
		fmt.Fprintf(&b, "  parent:   %s\n", sp.ParentSpanID)
	} else {
		fmt.Fprintf(&b, "  parent:   %s\n", styleDim.Render("(root)"))
	}
	fmt.Fprintf(&b, "  service:  %s\n", withDim(sp.Service))
	fmt.Fprintf(&b, "  unit:     %s\n", withDim(sp.Unit))
	fmt.Fprintf(&b, "  model:    %s\n", withDim(sp.Model))
	fmt.Fprintf(&b, "  start:    %s\n", sp.Start.Format(time.RFC3339Nano))
	fmt.Fprintf(&b, "  duration: %s\n", sp.Duration())
	statusStyle := styleDim
	if sp.StatusCode == "STATUS_CODE_ERROR" {
		statusStyle = styleErr
	}
	fmt.Fprintf(&b, "  status:   %s\n", statusStyle.Render(sp.StatusCode))
	if sp.StatusMsg != "" {
		fmt.Fprintf(&b, "  message:  %s\n", sp.StatusMsg)
	}
	if len(sp.Attrs) > 0 {
		fmt.Fprintf(&b, "\n%s\n", lipgloss.NewStyle().Bold(true).Render("attributes"))
		// Attributes in a stable order (alphabetical) for reproducibility.
		keys := make([]string, 0, len(sp.Attrs))
		for k := range sp.Attrs {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s = %s\n", k, sp.Attrs[k])
		}
	}
	m.details.SetContent(b.String())
	m.details.GotoTop()
}

func withDim(s string) string {
	if s == "" {
		return styleDim.Render("(none)")
	}
	return s
}

func formatTime(t time.Time) string {
	return t.UTC().Format("15:04:05.000")
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len([]rune(s)) <= n {
		return s
	}
	rs := []rune(s)
	if n <= 1 {
		return string(rs[:n])
	}
	return string(rs[:n-1]) + "…"
}

func sortStrings(s []string) {
	// Small inline sort; standard library sort would work equally well,
	// but avoiding the dependency keeps the tui file self-contained.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
