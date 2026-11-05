package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestSelectionSpanAcrossRows(t *testing.T) {
	sel := selection{start: point{2, 1}, end: point{4, 3}}
	if _, _, ok := sel.span(0, 10); ok {
		t.Fatal("row before the selection must be untouched")
	}
	if from, to, ok := sel.span(1, 10); !ok || from != 2 || to != 10 {
		t.Fatalf("first row span = %d..%d %v", from, to, ok)
	}
	if from, to, ok := sel.span(2, 10); !ok || from != 0 || to != 10 {
		t.Fatalf("middle row span = %d..%d %v", from, to, ok)
	}
	if from, to, ok := sel.span(3, 10); !ok || from != 0 || to != 5 {
		t.Fatalf("last row span = %d..%d %v", from, to, ok)
	}
	if _, _, ok := sel.span(4, 10); ok {
		t.Fatal("row after the selection must be untouched")
	}
}

func TestSelectionTextAndHighlight(t *testing.T) {
	rows := []string{"alpha line", "bravo line", "charlie line"}
	sel := selection{start: point{6, 0}, end: point{4, 1}}
	if got := sel.text(rows, 20); got != "line\nbravo" {
		t.Fatalf("text = %q", got)
	}
	marked := sel.highlight(rows, 20)
	if !strings.Contains(marked[0], "\x1b[7m") || !strings.Contains(marked[1], "\x1b[7m") {
		t.Fatal("selected cells must be reversed")
	}
	if marked[2] != rows[2] {
		t.Fatal("untouched rows must keep their content")
	}
	if !strings.HasPrefix(ansi.Strip(marked[1]), "bravo") {
		t.Fatalf("highlight changed the row text: %q", ansi.Strip(marked[1]))
	}
	// Highlighting the second occurrence of repeated text must not highlight the first.
	repeat := selection{start: point{6, 0}, end: point{8, 0}}.highlight([]string{"foo + foo"}, 20)
	if repeat[0] != "foo + \x1b[7mfoo\x1b[0m" {
		t.Fatalf("repeated substring highlight = %q", repeat[0])
	}
}

func TestSingleClickDoesNotCopy(t *testing.T) {
	m := bareModel(t, "/project")
	m.add("user", "hello world")
	m.refresh(true)
	if cmd, _ := m.mouseSelection(tea.MouseMsg{X: 2, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}); cmd != nil {
		t.Fatal("press should not copy")
	}
	if cmd, _ := m.mouseSelection(tea.MouseMsg{X: 2, Y: 0, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}); cmd != nil {
		t.Fatal("click without drag copied to clipboard")
	}
}

// Selection works on screen cells: wide characters take two, emoji and combining
// sequences stay whole.
func TestSelectionWideCharacters(t *testing.T) {
	for _, c := range []struct {
		line     string
		from, to int // selected cells, inclusive
		want     string
	}{
		{"界ab", 2, 3, "ab"},
		{"界ab", 1, 2, "界a"},
		{"e\u0301xy", 1, 2, "xy"},
		{"e\u0301xy", 0, 1, "e\u0301x"},
		{"👍🏽 ok", 1, 2, "👍🏽"},
		{"ab  ", 0, 3, "ab"},
	} {
		s := selection{start: point{c.from, 0}, end: point{c.to, 0}}
		if got := s.text([]string{c.line}, 20); got != c.want {
			t.Errorf("%q [%d,%d]: %q, want %q", c.line, c.from, c.to, got, c.want)
		}
	}
}
