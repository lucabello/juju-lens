package viewer

import (
	"fmt"
	"strings"

	"github.com/lucabello/juju-lens/internal/index"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// modelPicker is a minimal fullscreen list rendered when a recording
// contains more than one model, or when the user asks with "m" to switch.
// It intentionally does not use bubbles/list — the surface is tiny, and
// keeping the widget self-contained makes it easy to snapshot-test the
// output later.
type modelPicker struct {
	models []index.Model
	cursor int
}

func newModelPicker(models []index.Model) *modelPicker {
	return &modelPicker{models: models}
}

// update returns (done, chosen). When done=true the picker should be
// removed; chosen is non-nil when a selection was made and nil on cancel.
func (p *modelPicker) update(msg tea.KeyMsg) (bool, *index.Model) {
	switch {
	case key.Matches(msg, key.NewBinding(key.WithKeys("up", "k"))):
		p.cursor--
		if p.cursor < 0 {
			p.cursor = 0
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("down", "j"))):
		p.cursor++
		if p.cursor >= len(p.models) {
			p.cursor = len(p.models) - 1
		}
	case key.Matches(msg, key.NewBinding(key.WithKeys("home", "g"))):
		p.cursor = 0
	case key.Matches(msg, key.NewBinding(key.WithKeys("end", "G"))):
		p.cursor = len(p.models) - 1
	case key.Matches(msg, key.NewBinding(key.WithKeys("enter", " "))):
		if len(p.models) == 0 {
			return true, nil
		}
		m := p.models[p.cursor]
		return true, &m
	case key.Matches(msg, key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"))):
		return true, nil
	}
	return false, nil
}

func (p *modelPicker) view(termW, termH int) string {
	title := lipgloss.NewStyle().Bold(true).Render("Pick a model")
	hint := styleDim.Render("↑/↓ move · Enter select · Esc cancel")
	var rows []string
	for i, m := range p.models {
		line := fmt.Sprintf("  %s", m.Name)
		if m.UUID != "" {
			line += "  " + styleDim.Render(m.UUID)
		}
		if i == p.cursor {
			line = styleSelected.Render(line)
		}
		rows = append(rows, line)
	}
	inner := strings.Join(rows, "\n")
	body := lipgloss.JoinVertical(lipgloss.Left, title, "", inner, "", hint)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 3).
		Render(body)

	// Centre the box on the terminal.
	if termW <= 0 || termH <= 0 {
		return box
	}
	return lipgloss.Place(termW, termH, lipgloss.Center, lipgloss.Center, box)
}
