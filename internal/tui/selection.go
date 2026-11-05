package tui

import (
	"strings"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
)

// point is a screen cell inside the timeline viewport.
type point struct{ x, y int }

type selection struct{ start, end point }

// span returns the selected cells of one row and whether the row is touched at all.
func (s selection) span(row, width int) (int, int, bool) {
	if s.start == s.end {
		return 0, 0, false
	}
	first, last := s.start, s.end
	if first.y > last.y || first.y == last.y && first.x > last.x {
		first, last = last, first
	}
	if row < first.y || row > last.y {
		return 0, 0, false
	}
	from, to := 0, width
	if row == first.y {
		from = first.x
	}
	if row == last.y {
		to = last.x + 1
	}
	return max(0, min(from, width)), max(0, min(to, width)), true
}

// cells splits the plain text of one row around its selected cells. Characters are
// whole grapheme clusters: a wide character is selected if any of its cells is.
// Trailing spaces are never selected.
func (s selection) cells(line string, row, width int) (before, selected, after string, ok bool) {
	from, to, ok := s.span(row, width)
	if !ok {
		return "", "", "", false
	}
	full := ansi.Strip(line)
	plain := len(strings.TrimRight(full, " "))
	var parts [3]strings.Builder
	phase, col := 0, 0
	g := uniseg.NewGraphemes(full)
	for g.Next() {
		start, _ := g.Positions()
		w := max(g.Width(), 1)
		inside := start < plain && col < to && col+w > from
		switch {
		case phase == 0 && inside:
			phase = 1
		case phase == 1 && !inside:
			phase = 2
		}
		parts[phase].WriteString(g.Str())
		col += w
	}
	before, selected, after = parts[0].String(), parts[1].String(), parts[2].String()
	trimmed := strings.TrimRight(selected, " ")
	selected, after = trimmed, selected[len(trimmed):]+after
	return before, selected, after, selected != ""
}

// highlight reverses the selected cells while leaving other rows untouched.
func (s selection) highlight(rows []string, width int) []string {
	out := make([]string, len(rows))
	for row, line := range rows {
		before, selected, after, ok := s.cells(line, row, width)
		if !ok {
			out[row] = line
			continue
		}
		out[row] = before + "\x1b[7m" + selected + "\x1b[0m" + after
	}
	return out
}

// text extracts the selected rows as plain text.
func (s selection) text(rows []string, width int) string {
	var out []string
	for row, line := range rows {
		if _, selected, _, ok := s.cells(line, row, width); ok {
			out = append(out, selected)
		}
	}
	return strings.Join(out, "\n")
}

// copyText puts text on the system clipboard.
func copyText(text string) tea.Cmd {
	return func() tea.Msg {
		return clipboardCopied{lines: strings.Count(text, "\n") + 1, err: clipboard.WriteAll(text)}
	}
}

type clipboardCopied struct {
	lines int
	err   error
}

// mouseSelection handles left-button selection in the composer and the timeline. It reports
// whether the event was consumed; the wheel and other buttons keep scrolling the viewport.
// A drag that starts in the composer selects its text without copying. A drag that starts
// elsewhere selects timeline text and copies it on release.
func (m *model) mouseSelection(msg tea.MouseMsg) (tea.Cmd, bool) {
	if msg.Button != tea.MouseButtonLeft {
		return nil, false
	}
	cell := point{msg.X, msg.Y}
	switch msg.Action {
	case tea.MouseActionPress:
		m.sel, m.inputSel, m.inputDrag = nil, nil, false
		if offset, ok := m.composerOffset(msg.X, msg.Y, false); ok {
			m.inputSel = &composerSelection{anchor: offset, head: offset, value: m.input.Value()}
			m.inputDrag = true
			return nil, true
		}
		m.sel = &selection{start: cell, end: cell}
	case tea.MouseActionMotion:
		if m.inputDrag && m.inputSel != nil {
			if offset, ok := m.composerOffset(msg.X, msg.Y, true); ok {
				m.inputSel.head = offset
			}
		} else if m.sel != nil {
			m.sel.end = cell
		}
	case tea.MouseActionRelease:
		if m.inputDrag {
			m.inputDrag = false
			if m.inputSel == nil {
				return nil, true
			}
			if m.inputSel.anchor == m.inputSel.head {
				// A click without drag only places the cursor.
				offset := m.inputSel.head
				m.inputSel = nil
				return m.setCursorOffset(offset), true
			}
			// The cursor moves to the end of the drag so the next key starts from there.
			return m.setCursorOffset(m.inputSel.head), true
		}
		if m.sel == nil {
			return nil, true
		}
		text := m.sel.text(strings.Split(m.viewport.View(), "\n"), m.viewport.Width)
		m.sel = nil
		if strings.TrimSpace(text) != "" {
			return copyText(text), true
		}
		return nil, true
	}
	return nil, true
}
