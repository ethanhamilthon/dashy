package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestUserInputBackground(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })
	m := bareModel(t, "/project")
	m.width = 40
	m.input.SetValue("hello")
	for name, block := range map[string]string{
		"composer": m.inputBlock(),
		"sent":     renderEntry(entry{Kind: "user", Text: "hello\nworld"}, 40, -1),
		"empty":    renderRail("", 40),
		"narrow":   renderRail("long text", 1),
	} {
		width := 40
		if name == "narrow" {
			width = 1
		}
		for _, row := range strings.Split(block, "\n") {
			if ansi.StringWidth(row) != width {
				t.Errorf("%s: row width %d, want %d", name, ansi.StringWidth(row), width)
			}
			if !strings.Contains(row, "48;2;32;32;32") {
				t.Errorf("%s: missing input background: %q", name, row)
			}
		}
	}
}
