package viewer

// layout.go owns the geometry of the two-row TUI. The top row holds Events and
// Status side by side; the Logs stream spans the full width beneath them. All
// width/height helpers are pure functions of the outer terminal size so they can
// be unit-tested without a running TUI.

const (
	headerHeight = 3 // one line of title + top border/padding
	footerHeight = 2 // one line of help + separator

	minPaneWidth = 20
	minRowHeight = 3

	eventsPct = 52 // Events vs Status within the top row
	topPct    = 55 // top row vs Logs row, of the body height
)

// topRowWidths splits the top row into (events, status). Status takes whatever
// Events leaves, with a floor so neither pane collapses on a narrow terminal.
func topRowWidths(total int) (events, status int) {
	if total <= 0 {
		return 0, 0
	}
	events = (total * eventsPct) / 100
	if events < minPaneWidth {
		events = min(minPaneWidth, total)
	}
	status = total - events
	if status < minPaneWidth && total > minPaneWidth {
		status = min(minPaneWidth, total-1)
		events = total - status
	}
	return max(0, events), max(0, status)
}

// rowHeights splits the body into (top, logs). Each row keeps a minimum so a
// short terminal still shows something in both.
func rowHeights(total int) (top, logs int) {
	if total <= 0 {
		return 0, 0
	}
	top = (total * topPct) / 100
	if top < minRowHeight {
		top = min(minRowHeight, total)
	}
	logs = total - top
	if logs < minRowHeight && total > minRowHeight {
		logs = min(minRowHeight, total-1)
		top = total - logs
	}
	return max(0, top), max(0, logs)
}

func (m *model) bodyHeight() int {
	return max(1, m.height-headerHeight-footerHeight-1)
}

// layout syncs the inspector viewport with the current window; called on every
// WindowSizeMsg. Only the inspector overlay uses a viewport; the panes render to
// fixed-size strings.
func (m *model) layout() {
	m.overlay.Width = max(0, m.width-4)
	m.overlay.Height = max(1, m.bodyHeight()-2)
}
