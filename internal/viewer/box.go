package viewer

// box.go owns the shared pane frame: fitting styled rows to the inner width
// (horizontal scroll by default, `w` toggles wrap), padding to the box height,
// and splicing each pane's title into its top border. Keeping this in one place
// means Events/Logs/Status all clip, scroll and wrap identically — and all cut
// on *visible* width, not raw byte length, so ANSI-styled text is never chopped
// early (the old rune-count truncation counted escape codes as characters).

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// paneRow is one logical line of a pane's body. A selected row is drawn as a
// reverse-video bar spanning the pane width; its text should be plain (no inline
// styling) so the reverse reads cleanly.
type paneRow struct {
	text     string
	selected bool
}

func row(text string) paneRow    { return paneRow{text: text} }
func selRow(text string) paneRow { return paneRow{text: text, selected: true} }

// renderPane frames a pane: it fits each logical row to the inner width
// (honouring horizontal scroll or wrap), pads to the box height, draws the
// bordered box, and splices the title into its top border.
func (m *model) renderPane(p paneID, w, h int, title string, rows []paneRow) string {
	inner := max(1, w-4) // border (2) + padding (2)
	bodyH := max(1, h-2) // top + bottom border
	focused := m.focus == p

	out := make([]string, 0, bodyH)
	for _, r := range rows {
		for _, phys := range m.fitLine(r.text, inner, focused) {
			if r.selected {
				phys = styleSelected.Width(inner).Render(ansi.Strip(phys))
			}
			out = append(out, phys)
			if len(out) >= bodyH {
				break
			}
		}
		if len(out) >= bodyH {
			break
		}
	}
	for len(out) < bodyH {
		out = append(out, "")
	}
	box := m.boxFor(p).Width(w).Height(h).Render(strings.Join(out, "\n"))
	return spliceTitle(box, title, focused)
}

// fitLine turns one logical (already-styled) row into the physical lines shown
// for it: wrapped to the width when wrap is on, otherwise a single line clipped
// to the horizontal-scroll window. Scroll only applies to the focused pane so
// scrolling one pane doesn't shift the others.
func (m *model) fitLine(s string, inner int, focused bool) []string {
	if inner <= 0 {
		return []string{""}
	}
	if m.wrap {
		return strings.Split(ansi.Hardwrap(s, inner, false), "\n")
	}
	off := 0
	if focused {
		off = m.hScroll
	}
	return []string{ansi.Cut(s, off, off+inner)}
}

// spliceTitle replaces a rendered box's top border with one carrying the pane's
// title, e.g. "╭─ Events ───────╮". lipgloss has no border-title support, so we
// rebuild the top line from scratch using the same border colour the box (and
// its focus state) already use.
func spliceTitle(box, title string, focused bool) string {
	if title == "" {
		return box
	}
	top, rest := box, ""
	if nl := strings.IndexByte(box, '\n'); nl >= 0 {
		top, rest = box[:nl], box[nl:]
	}
	w := ansi.StringWidth(top)
	if w < 8 {
		return box
	}

	borderColor := lipgloss.Color("8")
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("7"))
	if focused {
		borderColor = lipgloss.Color("4")
		titleStyle = styleApp
	}
	bs := lipgloss.NewStyle().Foreground(borderColor)
	label := titleStyle.Render(" " + title + " ")
	if ansi.StringWidth(label) > w-4 {
		return box // title wouldn't fit; leave the plain border alone
	}
	fill := w - 3 - ansi.StringWidth(label) // "╭─" (2) + "╮" (1)
	if fill < 0 {
		fill = 0
	}
	newTop := bs.Render("╭─") + label + bs.Render(strings.Repeat("─", fill)+"╮")
	return newTop + rest
}
