package tui

import (
	"dashy/internal/shell"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"strings"
	"testing"
)

// composerModel returns a model whose composer holds value, sized to width x height.
func composerModel(t *testing.T, value string, width, height int) model {
	t.Helper()
	m := bareModel(t, "/project")
	m.width, m.height = width, height
	m.input.SetValue(value)
	m.refresh(true)
	return m
}

// composerPoint returns the screen cell of visual composer row r at text column c.
// The two-cell rail precedes the text.
func composerPoint(m model, r, c int) (int, int) {
	return 2 + c, m.inputTop - m.viewport.YOffset + r
}

func mouseAt(action tea.MouseAction, x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: action}
}

// dragComposer selects the composer text between two visual cells.
func dragComposer(m *model, fromRow, fromCol, toRow, toCol int) {
	x, y := composerPoint(*m, fromRow, fromCol)
	m.mouseSelection(mouseAt(tea.MouseActionPress, x, y))
	x, y = composerPoint(*m, toRow, toCol)
	m.mouseSelection(mouseAt(tea.MouseActionMotion, x, y))
	m.mouseSelection(mouseAt(tea.MouseActionRelease, x, y))
}

func update(m model, msg tea.Msg) model {
	next, _ := m.Update(msg)
	return next.(model)
}

// selectBrave selects "brave" in "hello brave world".
func selectBrave(t *testing.T) model {
	t.Helper()
	m := composerModel(t, "hello brave world", 40, 20)
	dragComposer(&m, 0, 6, 0, 11)
	if got := m.selectedComposerText(); got != "brave" {
		t.Fatalf("selected %q, want brave", got)
	}
	return m
}

func TestComposerLayoutWrapsLikeTextarea(t *testing.T) {
	cases := []struct {
		value string
		width int
	}{
		{"alpha beta gamma delta epsilon zeta", 10},
		{"short\n\nsecond line that wraps around\n", 12},
		{strings.Repeat("word界 ", 12) + "tail", 9},
		{"trailing spaces   ", 6},
	}
	for _, c := range cases {
		rows := composerLayout(c.value, c.width)
		ta := textarea.New()
		ta.Prompt = ""
		ta.ShowLineNumbers = false
		ta.CharLimit = 0
		ta.MaxWidth = 10000
		ta.SetWidth(c.width)
		want := 0
		for _, line := range strings.Split(c.value, "\n") {
			ta.SetValue(line)
			want += ta.LineInfo().Height
		}
		if len(rows) != want {
			t.Fatalf("%q width %d: %d rows, textarea renders %d", c.value, c.width, len(rows), want)
		}
		runes := []rune(c.value)
		if rows[0].start != 0 || rows[len(rows)-1].end != len(runes) {
			t.Fatalf("%q: rows do not cover the value: %+v", c.value, rows)
		}
		for i := 1; i < len(rows); i++ {
			prevEnd := rows[i-1].end
			gap := rows[i].start - prevEnd
			newline := prevEnd < len(runes) && runes[prevEnd] == '\n'
			if gap < 0 || gap > 1 || (gap == 1) != newline {
				t.Fatalf("%q width %d: row %d starts at %d after %d (newline=%v)", c.value, c.width, i, rows[i].start, prevEnd, newline)
			}
		}
	}
}

func TestComposerDragSelectsWithoutCopy(t *testing.T) {
	m := composerModel(t, "hello brave world", 40, 20)
	dragComposer(&m, 0, 6, 0, 11)
	if m.sel != nil {
		t.Fatal("composer drag created a timeline selection")
	}
	lo, hi, ok := m.activeComposerSelection()
	if !ok || lo != 6 || hi != 11 {
		t.Fatalf("selection = %d..%d %v, want 6..11", lo, hi, ok)
	}
	if got := m.input.Value(); got != "hello brave world" {
		t.Fatalf("drag changed the text: %q", got)
	}
}

func TestComposerTypingReplacesSelection(t *testing.T) {
	m := selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	if got := m.input.Value(); got != "hello X world" {
		t.Fatalf("rune = %q", got)
	}
	m = selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	if got := m.input.Value(); got != "hello   world" {
		t.Fatalf("space = %q", got)
	}
	m = selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pasted\ntext"), Paste: true})
	if got := m.input.Value(); got != "hello pasted\ntext world" {
		t.Fatalf("bracketed paste = %q", got)
	}
	m = selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if got := m.input.Value(); got != "hello \n world" {
		t.Fatalf("alt+enter = %q", got)
	}
}

func TestComposerCtrlXCutsSelectionKeepsImages(t *testing.T) {
	m := selectBrave(t)
	m.images = []shell.Image{{URL: "data:image/png;base64,one"}}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m = next.(model)
	if cmd == nil {
		t.Fatal("cut did not copy the selection")
	}
	if got := m.input.Value(); got != "hello  world" {
		t.Fatalf("cut value = %q", got)
	}
	if len(m.images) != 1 {
		t.Fatal("cut with a selection removed an image")
	}

	m = composerModel(t, "hello world", 40, 20)
	m.images = []shell.Image{{URL: "data:image/png;base64,one"}}
	m = update(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	if len(m.images) != 0 {
		t.Fatal("ctrl+x without a selection did not remove the image")
	}
}
