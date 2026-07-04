package viewer

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// renderDetails renders the currently-selected span's attributes inside a
// bordered viewport. The viewport itself is a member of the model so the
// user can scroll long attribute lists independently.
func (m *model) renderDetails(w, h int) string {
	m.details.Width = max(0, w-4)
	m.details.Height = max(1, h-2)
	return styleDetailsBox.Width(w).Height(h).Render(m.details.View())
}

func (m *model) refreshDetails() {
	if len(m.spans) == 0 {
		m.details.SetContent("")
		return
	}
	sp := m.currentSpan()
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
	if sp.StatusCode == "ERROR" {
		statusStyle = styleErr
	}
	fmt.Fprintf(&b, "  status:   %s\n", statusStyle.Render(sp.StatusCode))
	if sp.StatusMsg != "" {
		fmt.Fprintf(&b, "  message:  %s\n", sp.StatusMsg)
	}
	if len(sp.Attrs) > 0 {
		fmt.Fprintf(&b, "\n%s\n", lipgloss.NewStyle().Bold(true).Render("attributes"))
		keys := make([]string, 0, len(sp.Attrs))
		for k := range sp.Attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s = %s\n", k, sp.Attrs[k])
		}
	}
	m.details.SetContent(b.String())
	m.details.GotoTop()
}
