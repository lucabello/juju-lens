package viewer

import (
	"fmt"
	"strings"
	"time"

	"github.com/lucabello/juju-lens/internal/narrative"
	"github.com/lucabello/juju-lens/internal/recording"
)

// durCell formats a duration as a number and its unit in columns of their
// own, so a glance at the unit column tells milliseconds from seconds: the
// seconds are in a different colour.
func durCell(d time.Duration) string {
	var num, unit string
	switch {
	case d >= 10*time.Second:
		num, unit = fmt.Sprintf("%.0f", d.Seconds()), "s"
	case d >= time.Second:
		num, unit = fmt.Sprintf("%.1f", d.Seconds()), "s"
	case d >= 10*time.Millisecond:
		num, unit = fmt.Sprintf("%d", d.Milliseconds()), "ms"
	case d >= 100*time.Microsecond:
		num, unit = fmt.Sprintf("%.1f", float64(d.Microseconds())/1000), "ms"
	default:
		num, unit = fmt.Sprintf("%d", d.Microseconds()), "µs"
	}
	style := styleDim
	if unit == "s" {
		style = styleWarn
	}
	return fmt.Sprintf("%5s ", num) + style.Render(fmt.Sprintf("%-2s", unit))
}

// plainMemory drops the marks from a memory cell: the approximation (~) and
// the kernel's peak (!), which only the verbose view explains.
func plainMemory(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, "~"), "!")
}

// hookTrees is the charm-side trees of ev's processes, with their costs
// attached, and the profiles of those processes. The profiles are read from
// the index once per event.
func (m *model) hookTrees(ev event) ([]*narrative.OpsNode, []recording.OpsProfile) {
	if len(ev.opsSpans) == 0 || m.ops == nil {
		return nil, nil
	}
	trees := m.ops.Trees(ev.opsSpans)
	if len(trees) == 0 {
		return nil, nil
	}
	if m.profCache.key != ev.spanID || m.profCache.model != m.activeModel.ID {
		m.profCache.key, m.profCache.model = ev.spanID, m.activeModel.ID
		m.profCache.profs = nil
		if m.db != nil {
			m.profCache.profs, _ = m.db.Profiles(narrative.ProcessRoots(trees))
		}
	}
	narrative.AttachCosts(trees, m.profCache.profs)
	return trees, m.profCache.profs
}

// profCache holds the profiles of the event the inspector last showed.
type profCache struct {
	key   string
	model int64
	profs []recording.OpsProfile
}
