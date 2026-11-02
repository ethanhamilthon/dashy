package tui

import (
	"dashy/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"
)

func TestComposerAtHeightLimitDoesNotJumpBetweenUpdates(t *testing.T) {
	for _, width := range []int{18, 80} {
		m := newModel(&agent.Agent{}, "/project")
		m.width, m.height = width, 12
		m.input.SetValue(strings.Repeat("line\n", 20) + "tail")
		m.refresh(false)
		for i := 0; i < 20; i++ {
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
			m = next.(model)
			before := ansi.Strip(m.inputBlock())
			next, _ = m.Update(struct{}{})
			m = next.(model)
			after := ansi.Strip(m.inputBlock())
			if before != after {
				t.Fatalf("width %d: idle update moved input after key %d:\nBEFORE:\n%s\nAFTER:\n%s", width, i, before, after)
			}
			if !strings.Contains(before, "tail") {
				t.Fatalf("width %d: cursor line not visible after typing: %q", width, before)
			}
			if m.input.Height() != 6 {
				t.Fatalf("height = %d, want 6", m.input.Height())
			}
		}
	}
}

func TestComposerMeasurementPreservesScrollAcrossResizeAndDelete(t *testing.T) {
	m := newModel(&agent.Agent{}, "/project")
	for _, width := range []int{80, 18, 40} {
		m.width, m.height = width, 12
		m.input.SetValue(strings.Repeat("word 界 ", 100) + "tail")
		m.refresh(false)
		for _, key := range []tea.KeyMsg{
			{Type: tea.KeyRunes, Runes: []rune("x")},
			{Type: tea.KeyBackspace},
			{Type: tea.KeyUp},
			{Type: tea.KeyDown},
		} {
			next, _ := m.Update(key)
			m = next.(model)
			before := ansi.Strip(m.inputBlock())
			next, _ = m.Update(struct{}{})
			m = next.(model)
			if after := ansi.Strip(m.inputBlock()); before != after {
				t.Fatalf("width %d, key %s: scroll changed between updates", width, key.String())
			}
		}
		m.input.SetValue("short")
		m.refresh(false)
		if m.input.Height() != 1 {
			t.Fatalf("composer failed to shrink: %d", m.input.Height())
		}
	}
}
