package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// pager is a vertical viewport over prepared rows. Unlike the bubbles viewport it
// takes the rows as they are, so a refresh does not split or measure the whole
// timeline again.
type pager struct {
	Width, Height, YOffset int
	lines                  []string
}

// SetLines replaces the content; the pager keeps lines without copying.
func (p *pager) SetLines(lines []string) {
	p.lines = lines
	if p.YOffset > len(lines)-1 {
		p.GotoBottom()
	}
}

func (p pager) maxYOffset() int { return max(0, len(p.lines)-p.Height) }

func (p pager) AtBottom() bool { return p.YOffset >= p.maxYOffset() }

func (p *pager) SetYOffset(n int) { p.YOffset = min(max(0, n), p.maxYOffset()) }

func (p *pager) GotoTop() { p.YOffset = 0 }

func (p *pager) GotoBottom() { p.SetYOffset(p.maxYOffset()) }

func (p *pager) ScrollUp(n int) { p.SetYOffset(p.YOffset - n) }

func (p *pager) ScrollDown(n int) { p.SetYOffset(p.YOffset + n) }

// Update scrolls by a page for PgUp/PgDn and by three rows for the mouse wheel.
func (p pager) Update(msg tea.Msg) (pager, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		switch v.String() {
		case "pgdown":
			p.ScrollDown(p.Height)
		case "pgup":
			p.ScrollUp(p.Height)
		}
	case tea.MouseMsg:
		if v.Action != tea.MouseActionPress {
			break
		}
		switch v.Button {
		case tea.MouseButtonWheelDown:
			p.ScrollDown(3)
		case tea.MouseButtonWheelUp:
			p.ScrollUp(3)
		}
	}
	return p, nil
}

// View renders the visible rows padded and cut to the pager size.
func (p pager) View() string {
	top := min(max(0, p.YOffset), len(p.lines))
	visible := p.lines[top:min(len(p.lines), top+p.Height)]
	return lipgloss.NewStyle().Width(p.Width).Height(p.Height).MaxWidth(p.Width).MaxHeight(p.Height).Render(strings.Join(visible, "\n"))
}
