package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"testing"
)

func TestComposerClickAndDrag(t *testing.T) {
	for _, drag := range []bool{false, true} {
		m := composerModel(t, "hello brave world", 40, 20)
		x, y := composerPoint(m, 0, 6)
		m.mouseSelection(mouseAt(tea.MouseActionPress, x, y))
		if drag {
			x, y = composerPoint(m, 0, 11)
			m.mouseSelection(mouseAt(tea.MouseActionMotion, x, y))
		}
		_, _ = m.mouseSelection(mouseAt(tea.MouseActionRelease, x, y))
		if m.sel != nil || m.composerCursorOffset() != 6 {
			t.Fatalf("drag=%v cursor=%d", drag, m.composerCursorOffset())
		}
		m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
		if m.input.Value() != "hello Xbrave world" {
			t.Fatal(m.input.Value())
		}
	}
}

func TestComposerClickWrappedAndScrolled(t *testing.T) {
	m := composerModel(t, "first line\nsecond line\nthird line\nfourth line\nfifth line\nsixth line", 18, 12)
	rows := m.composerRows()
	scroll := m.composerScroll(rows)
	x, y := composerPoint(m, 0, 2)
	offset, ok := m.composerOffset(x, y)
	if !ok || offset != rows[scroll].start+2 {
		t.Fatalf("offset=%d scroll=%d rows=%v", offset, scroll, rows)
	}
	m.mouseSelection(mouseAt(tea.MouseActionPress, x, y))
	m.mouseSelection(mouseAt(tea.MouseActionRelease, x, y))
	if m.composerCursorOffset() != offset {
		t.Fatalf("cursor=%d want=%d", m.composerCursorOffset(), offset)
	}
}
