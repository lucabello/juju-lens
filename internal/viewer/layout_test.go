package viewer

import "testing"

func TestColumnWidthsSumsToTotal(t *testing.T) {
	for _, total := range []int{60, 80, 100, 120, 200, 400} {
		apps, centre, status := columnWidths(total)
		if apps+centre+status != total {
			t.Errorf("total=%d: apps=%d + centre=%d + status=%d != total",
				total, apps, centre, status)
		}
		if apps <= 0 || centre <= 0 || status <= 0 {
			t.Errorf("total=%d: no column should collapse to zero (%d/%d/%d)",
				total, apps, centre, status)
		}
	}
}

func TestColumnWidthsNarrowTerminal(t *testing.T) {
	// A skinny terminal should still hand out something usable to every
	// column; the sidebars shrink first, the centre never disappears.
	apps, centre, status := columnWidths(50)
	if apps < 1 || centre < 1 || status < 1 {
		t.Fatalf("narrow terminal collapsed a column: %d/%d/%d", apps, centre, status)
	}
	if apps+centre+status != 50 {
		t.Fatalf("narrow terminal split does not sum: %d+%d+%d", apps, centre, status)
	}
}

func TestColumnWidthsZero(t *testing.T) {
	apps, centre, status := columnWidths(0)
	if apps != 0 || centre != 0 || status != 0 {
		t.Fatalf("zero-width terminal must yield zero everywhere: %d/%d/%d", apps, centre, status)
	}
}

func TestCentreSplitAlwaysFits(t *testing.T) {
	for _, total := range []int{5, 8, 12, 20, 40, 80} {
		tl, det := centreSplit(total)
		if tl+det != total {
			t.Errorf("total=%d: timeline=%d + details=%d != total", total, tl, det)
		}
		if tl < 3 && total >= 3 {
			t.Errorf("total=%d: timeline must always keep at least 3 rows, got %d", total, tl)
		}
	}
}
