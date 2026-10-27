package viewer

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

// handleOverlayKey applies a key while the inspector is open. A hook recorded
// from inside the unit opens on its trace; the profile (`p`), the memory view
// (`m`) and what the hook changed (`i`) are a key away, and esc goes back to
// the trace before it closes the inspector.
func (m *model) handleOverlayKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	hasOps := len(m.currentEvent().opsSpans) > 0
	rerender := func() (tea.Model, tea.Cmd) {
		m.renderOverlay()
		return m, nil
	}
	show := func(mode inspMode) (tea.Model, tea.Cmd) {
		m.inspMode = mode
		m.renderOverlay()
		m.overlay.GotoTop()
		return m, nil
	}
	if m.inspMode == inspFlame && m.flame.searching {
		m.handleFlameKey(msg)
		return rerender()
	}
	switch {
	case msg.String() == "esc" && m.inspMode == inspFlame && m.flame.reset():
		return rerender()
	case msg.String() == "esc" || msg.String() == "q" || msg.String() == "ctrl+c" || m.inspMode == inspDetail && key.Matches(msg, m.keys.Inspect):
		if hasOps && m.inspMode != inspTrace {
			return show(inspTrace)
		}
		m.overlayOn = false
		return m, nil
	case key.Matches(msg, m.keys.Diff) && m.inspMode != inspDetail && hasChanges(m.currentEvent()):
		m.diffMode = true
		return show(inspDetail)
	case key.Matches(msg, m.keys.Flame) && hasOps:
		return show(inspFlame)
	case key.Matches(msg, m.keys.Memory) && hasOps:
		return show(inspMemory)
	case key.Matches(msg, m.keys.Trace) && hasOps:
		return show(inspTrace)
	}
	handled := false
	switch m.inspMode {
	case inspTrace:
		handled = m.handleTraceKey(msg)
	case inspFlame:
		handled = m.handleFlameKey(msg)
	default:
		switch {
		case key.Matches(msg, m.keys.Diff):
			m.diffMode = !m.diffMode
			handled = true
		case key.Matches(msg, m.keys.CopyCur):
			return m, copyCmd(m.copyCur)
		case key.Matches(msg, m.keys.CopyPrev):
			return m, copyCmd(m.copyPrev)
		}
	}
	if handled {
		return rerender()
	}
	var cmd tea.Cmd
	m.overlay, cmd = m.overlay.Update(msg)
	return m, cmd
}
