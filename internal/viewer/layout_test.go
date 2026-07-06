package viewer

import "testing"

func TestTopRowWidthsSumsToTotal(t *testing.T) {
	for _, total := range []int{60, 80, 100, 120, 200, 400} {
		events, status := topRowWidths(total)
		if events+status != total {
			t.Errorf("total=%d: events=%d + status=%d != total", total, events, status)
		}
		if events <= 0 || status <= 0 {
			t.Errorf("total=%d: no pane should collapse to zero (%d/%d)", total, events, status)
		}
	}
}

func TestTopRowWidthsNarrowTerminal(t *testing.T) {
	// A skinny terminal should still hand out something usable to both panes.
	events, status := topRowWidths(50)
	if events < 1 || status < 1 {
		t.Fatalf("narrow terminal collapsed a pane: %d/%d", events, status)
	}
	if events+status != 50 {
		t.Fatalf("narrow terminal split does not sum: %d+%d", events, status)
	}
}

func TestTopRowWidthsZero(t *testing.T) {
	events, status := topRowWidths(0)
	if events != 0 || status != 0 {
		t.Fatalf("zero-width terminal must yield zero everywhere: %d/%d", events, status)
	}
}

func TestRowHeightsSumsToTotal(t *testing.T) {
	for _, total := range []int{5, 8, 12, 20, 40, 80} {
		top, logs := rowHeights(total)
		if top+logs != total {
			t.Errorf("total=%d: top=%d + logs=%d != total", total, top, logs)
		}
		if top < 1 || logs < 1 {
			t.Errorf("total=%d: neither row should collapse (%d/%d)", total, top, logs)
		}
	}
}

func TestRowHeightsZero(t *testing.T) {
	top, logs := rowHeights(0)
	if top != 0 || logs != 0 {
		t.Fatalf("zero body height must yield zero rows: %d/%d", top, logs)
	}
}
