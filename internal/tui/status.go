package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func renderBottomBar(width int, left, right string) string {
	if width < 5 {
		return strings.Repeat(" ", max(0, width))
	}
	avail := width - 4
	if left != "" && right != "" && ansi.StringWidth(left)+ansi.StringWidth(right)+1 > avail {
		right = ansi.TruncateLeft(right, max(0, avail/3), "…")
		left = ansi.Truncate(left, max(0, avail-ansi.StringWidth(right)-1), "…")
	}
	left, right = ansi.Truncate(left, avail, "…"), ansi.TruncateLeft(right, max(0, ansi.StringWidth(right)-avail), "…")
	gap := max(0, avail-ansi.StringWidth(left)-ansi.StringWidth(right))
	return "  " + ansi.Truncate(left+strings.Repeat(" ", gap)+right, avail, "") + "  "
}

func (m model) status() (string, string) {
	s := m.ctl.Settings()
	left := s.Provider + " · " + s.Model
	if s.Effort != "" {
		left += "/" + s.Effort
	}
	if s.Provider == "" || s.Model == "" {
		left = "no model selected"
	}
	return left, "ctx ~" + formatContext(m.ctl.Context()) + " · " + shortPath(m.cwd)
}

func (m model) bottomBar() string {
	left, right := m.status()
	bar, style := renderBottomBar(m.width, left, right), lipgloss.NewStyle().Foreground(muted)
	gap := m.width - 8 - ansi.StringWidth(left) - ansi.StringWidth(right)
	if m.flash.Text == "" || gap < 8 || ansi.StringWidth(bar) != m.width {
		return style.Render(bar)
	}
	flash := ansi.Truncate(m.flash.Text, gap, "…")
	at := min(max(4+ansi.StringWidth(left), (m.width-ansi.StringWidth(flash))/2), m.width-4-ansi.StringWidth(right)-ansi.StringWidth(flash))
	color := map[bool]lipgloss.TerminalColor{true: errorColor, false: foreground}[m.flash.Kind == "error"]
	return style.Render(ansi.Truncate(bar, at, "")) + lipgloss.NewStyle().Foreground(color).Render(flash) + style.Render(ansi.TruncateLeft(bar, at+ansi.StringWidth(flash), ""))
}
