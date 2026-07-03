package viewer

// layout.go owns the geometry of the three-column TUI. All width/height
// helpers are pure functions of the outer terminal size so they can be
// unit-tested without a running TUI.

const (
	headerHeight = 3 // one line of title + top border/padding
	footerHeight = 2 // one line of help + separator

	minAppsWidth   = 20
	minCentreWidth = 40
	minStatusWidth = 24

	appsPct   = 22
	statusPct = 30
)

// columnWidths returns (apps, centre, status) widths that sum to total. The
// centre column takes whatever the sidebars leave behind, with sensible
// minima applied so narrow terminals don't collapse a pane to nothing.
func columnWidths(total int) (apps, centre, status int) {
	if total <= 0 {
		return 0, 0, 0
	}
	apps = (total * appsPct) / 100
	status = (total * statusPct) / 100
	if apps < minAppsWidth {
		apps = minAppsWidth
	}
	if status < minStatusWidth {
		status = minStatusWidth
	}
	centre = total - apps - status
	if centre < minCentreWidth {
		// Terminal is too narrow to satisfy all three minima. Trim the
		// sidebars proportionally to whatever centre still needs; the
		// alternative (fixed clipping) hides the timeline the user came
		// for.
		deficit := minCentreWidth - centre
		takeFromApps := min(apps-1, deficit/2)
		takeFromStatus := min(status-1, deficit-takeFromApps)
		apps -= takeFromApps
		status -= takeFromStatus
		centre = total - apps - status
		if centre < 0 {
			centre = 0
		}
	}
	if apps < 0 {
		apps = 0
	}
	if status < 0 {
		status = 0
	}
	return apps, centre, status
}

// centreSplit returns the (timeline, details) heights inside the centre
// column. The details pane is capped at a fraction so the timeline stays
// legible even in tall terminals.
func centreSplit(total int) (timeline, details int) {
	if total <= 0 {
		return 0, 0
	}
	details = total / 3
	if details < 6 {
		details = min(total/2, 6)
	}
	if details > 16 {
		details = 16
	}
	timeline = total - details
	if timeline < 3 {
		timeline = total
		details = 0
	}
	return timeline, details
}

func (m *model) bodyHeight() int {
	return max(1, m.height-headerHeight-footerHeight-1)
}

func (m *model) timelineHeight() int {
	tl, _ := centreSplit(m.bodyHeight())
	// -2 for the surrounding border/padding.
	return max(1, tl-2)
}

// layout syncs viewport sizes with the current window; called on every
// WindowSizeMsg.
func (m *model) layout() {
	_, centre, _ := columnWidths(m.width)
	_, det := centreSplit(m.bodyHeight())
	m.details.Width = max(0, centre-4)
	m.details.Height = max(1, det-2)
}
