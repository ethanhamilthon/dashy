package tui

import (
	"regexp"
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
	placeholder := m.inputBlock()
	m.input.SetValue("hello")
	for name, block := range map[string]string{
		"placeholder": placeholder,
		"composer":    m.inputBlock(),
		"sent":        renderEntry(entry{Kind: "user", Text: "hello\nworld"}, 40, -1),
		"empty":       renderRail("", 40),
		"narrow":      renderRail("long text", 1),
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
			// Check every visible span, not just the presence of a background code.
			background, end := false, 0
			for _, match := range regexp.MustCompile(`\x1b\[[0-9;]*m`).FindAllStringIndex(row, -1) {
				if ansi.StringWidth(row[end:match[0]]) > 0 && !background {
					t.Fatalf("%s: visible text without background: %q", name, row[end:match[0]])
				}
				code := row[match[0]:match[1]]
				if code == "\x1b[m" || code == "\x1b[0m" || strings.Contains(code, "49") {
					background = false
				}
				if strings.Contains(code, "48;2;32;32;32") {
					background = true
				}
				end = match[1]
			}
			if ansi.StringWidth(row[end:]) > 0 && !background {
				t.Fatalf("%s: trailing text without background", name)
			}
		}
	}
}
