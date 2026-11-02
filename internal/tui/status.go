package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

func renderBottomBar(width int, left, right string) string {
	if width <= 0 {
		return ""
	}
	if width < 5 {
		return strings.Repeat(" ", width)
	}
	avail := width - 4
	lw := ansi.StringWidth(left)
	rw := ansi.StringWidth(right)
	var content string
	switch {
	case lw == 0 && rw == 0:
		content = strings.Repeat(" ", avail)
	case rw == 0:
		if lw > avail {
			left = ansi.Truncate(left, avail, "…")
		}
		gap := max(0, avail-ansi.StringWidth(left))
		content = ansi.Truncate(left+strings.Repeat(" ", gap), avail, "")
	case lw == 0:
		if rw > avail {
			right = ansi.TruncateLeft(right, avail, "…")
		}
		gap := max(0, avail-ansi.StringWidth(right))
		content = ansi.Truncate(strings.Repeat(" ", gap)+right, avail, "")
	default:
		if lw+rw+1 > avail {
			right = ansi.TruncateLeft(right, max(0, avail/3), "…")
			availLeft := max(0, avail-ansi.StringWidth(right)-1)
			left = ansi.Truncate(left, availLeft, "…")
		}
		gap := max(0, avail-ansi.StringWidth(left)-ansi.StringWidth(right))
		content = ansi.Truncate(left+strings.Repeat(" ", gap)+right, avail, "")
	}
	return "  " + content + "  "
}

func (m model) status() (string, string) {
	var provider, model, effort string
	if m.ctl != nil {
		s := m.ctl.Settings()
		provider, model, effort = s.Provider, s.Model, s.Effort
	}
	mdl := model
	if effort != "" {
		mdl += "/" + effort
	}
	left := provider + " · " + mdl
	if provider == "" || model == "" {
		left = "no model selected"
	}

	right := "ctx ~" + formatContext(m.ctl.Context()) + " · " + shortPath(m.cwd)
	return left, right
}

func (m model) bottomBar() string {
	left, right := m.status()
	return renderBottomBar(m.width, left, right)
}
