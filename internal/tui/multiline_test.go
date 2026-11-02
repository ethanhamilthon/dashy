package tui

import (
	"fmt"
	"strings"
	"testing"

	"dashy/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

func TestMultilineInputKeepsEarlierLinesVisible(t *testing.T) {
	m := newModel(&agent.Agent{}, "/project")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(model)
	for i, word := range []string{"one", "two", "three"} {
		if i > 0 {
			next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
			m = next.(model)
		}
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(word)})
		m = next.(model)
		view := m.input.View()
		for _, earlier := range []string{"one", "two", "three"}[:i+1] {
			if !strings.Contains(view, earlier) {
				t.Fatalf("after %q input lost %q:\n%s", word, earlier, view)
			}
		}
	}
}

func TestKittyEscapeClosesCompletion(t *testing.T) {
	for _, payload := range []string{"27u", "27;1u", "27;1:1u"} {
		key, ok := normalizeTerminalKey(csiTestMessage(fmt.Sprintf("?CSI%+v?", []byte(payload)))).(tea.KeyMsg)
		if !ok || key.Type != tea.KeyEsc {
			t.Fatalf("%s: not Esc: %v", payload, key)
		}
	}
	if got := normalizeTerminalKey(csiTestMessage(fmt.Sprintf("?CSI%+v?", []byte("27;1:3u")))); got != nil {
		t.Fatalf("release produced %v", got)
	}
	m := newModel(&agent.Agent{}, "/project")
	m.input.SetValue("/")
	if len(m.completions()) == 0 {
		t.Fatal("expected completions")
	}
	next, _ := m.Update(csiTestMessage(fmt.Sprintf("?CSI%+v?", []byte("27u"))))
	if len(next.(model).completions()) != 0 {
		t.Fatal("Esc did not close completions")
	}
}
