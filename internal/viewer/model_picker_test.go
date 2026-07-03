package viewer

import (
	"testing"

	"github.com/lucabello/juju-lens/internal/index"

	tea "github.com/charmbracelet/bubbletea"
)

func TestModelPickerEnterReturnsSelection(t *testing.T) {
	models := []index.Model{{ID: 1, Name: "prod"}, {ID: 2, Name: "staging"}}
	p := newModelPicker(models)

	// Down once, Enter should choose "staging".
	if done, chosen := p.update(tea.KeyMsg{Type: tea.KeyDown}); done || chosen != nil {
		t.Fatalf("Down should not finish picker: done=%v chosen=%v", done, chosen)
	}
	done, chosen := p.update(tea.KeyMsg{Type: tea.KeyEnter})
	if !done {
		t.Fatal("Enter must finish the picker")
	}
	if chosen == nil || chosen.Name != "staging" {
		t.Fatalf("expected chosen=staging, got %+v", chosen)
	}
}

func TestModelPickerEscCancels(t *testing.T) {
	p := newModelPicker([]index.Model{{ID: 1, Name: "prod"}})
	done, chosen := p.update(tea.KeyMsg{Type: tea.KeyEsc})
	if !done {
		t.Fatal("Esc must finish the picker")
	}
	if chosen != nil {
		t.Fatalf("Esc must not choose a model, got %+v", chosen)
	}
}

func TestModelPickerCursorClamps(t *testing.T) {
	p := newModelPicker([]index.Model{{Name: "a"}, {Name: "b"}})
	// Up at 0 stays at 0.
	p.update(tea.KeyMsg{Type: tea.KeyUp})
	if p.cursor != 0 {
		t.Fatalf("cursor must clamp low, got %d", p.cursor)
	}
	// End goes to last, then Down clamps.
	p.update(tea.KeyMsg{Type: tea.KeyEnd})
	if p.cursor != 1 {
		t.Fatalf("End should land on last item, got %d", p.cursor)
	}
	p.update(tea.KeyMsg{Type: tea.KeyDown})
	if p.cursor != 1 {
		t.Fatalf("cursor must clamp high, got %d", p.cursor)
	}
}
