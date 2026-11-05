package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

type composerRow struct{ start, end int }

// composerNoop triggers a textarea update, which scrolls its viewport to the cursor.
type composerNoop struct{}

func composerLayout(value string, width int) []composerRow {
	if value == "" {
		return nil
	}
	var rows []composerRow
	base := 0
	for _, line := range strings.Split(value, "\n") {
		runes := []rune(line)
		start := 0
		for _, n := range wrapRows(runes, width) {
			rows = append(rows, composerRow{base + start, base + min(start+n, len(runes))})
			start += n
		}
		base += len(runes) + 1
	}
	return rows
}

// wrapRows returns the rune count of each row the textarea wraps one line into,
// including the padding spaces it adds. It follows the textarea's word wrap but
// keeps running widths, so it is linear in the line length.
func wrapRows(runes []rune, width int) []int {
	rows := []int{0}
	row, rowWidth := 0, 0
	wordLen, wordWidth, lastWidth, spaces := 0, 0, 0, 0
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			lastWidth = runeWidth(r)
			wordLen, wordWidth = wordLen+1, wordWidth+lastWidth
		}
		switch {
		case spaces > 0:
			if rowWidth+wordWidth+spaces > width {
				row++
				rows = append(rows, 0)
				rowWidth = 0
			}
			rows[row] += wordLen + spaces
			rowWidth += wordWidth + spaces
			wordLen, wordWidth, spaces = 0, 0, 0
		case wordWidth+lastWidth > width:
			if rows[row] > 0 {
				row++
				rows = append(rows, 0)
				rowWidth = 0
			}
			rows[row] += wordLen
			rowWidth += wordWidth
			wordLen, wordWidth = 0, 0
		}
	}
	if rowWidth+wordWidth+spaces >= width {
		rows = append(rows, wordLen+spaces+1)
	} else {
		rows[row] += wordLen + spaces + 1
	}
	return rows
}

func runeWidth(r rune) int {
	if r < 0x80 {
		return 1
	}
	return ansi.StringWidth(string(r))
}

func (m *model) composerRows() []composerRow {
	value, width := m.input.Value(), m.input.Width()
	if m.layoutValue != value || m.layoutWidth != width {
		m.layoutValue, m.layoutWidth = value, width
		m.layout = composerLayout(value, width)
	}
	return m.layout
}

func (m *model) composerCursorRow(rows []composerRow) int {
	lines := strings.Split(m.input.Value(), "\n")
	base := 0
	for _, line := range lines[:min(m.input.Line(), len(lines))] {
		base += len([]rune(line)) + 1
	}
	first := 0
	for _, row := range rows {
		if row.start < base {
			first++
		}
	}
	return first + m.input.LineInfo().RowOffset
}

// composerScroll returns the first visual row shown by the composer. The textarea keeps its
// viewport offset private, so the offset is recovered by matching its rendered rows against
// the layout. Offsets near the cursor are tried first.
func (m *model) composerScroll(rows []composerRow) int {
	height := m.input.Height()
	if len(rows) <= height {
		return 0
	}
	value := []rune(m.input.Value())
	text := func(i int) string {
		if i >= len(rows) {
			return ""
		}
		return strings.TrimRight(string(value[rows[i].start:rows[i].end]), " ")
	}
	view := strings.Split(m.input.View(), "\n")
	matches := func(offset int) bool {
		for i := 0; i < height && i < len(view); i++ {
			if strings.TrimRight(ansi.Strip(view[i]), " ") != text(offset+i) {
				return false
			}
		}
		return true
	}
	cursor := m.composerCursorRow(rows)
	low := max(0, cursor-height+1)
	for offset := low; offset <= min(cursor, len(rows)); offset++ {
		if matches(offset) {
			return offset
		}
	}
	for offset := 0; offset <= len(rows); offset++ {
		if matches(offset) {
			return offset
		}
	}
	return low
}

func (m *model) composerOffset(x, y int) (int, bool) {
	value := m.input.Value()
	if value == "" || m.keyInComposer() {
		return 0, false
	}
	rows := m.composerRows()
	height := m.input.Height()
	r := y + m.viewport.YOffset - m.inputTop
	if r < 0 || r >= height {
		return 0, false
	}
	row := rows[min(len(rows)-1, m.composerScroll(rows)+r)]
	runes := []rune(value)
	cx := max(0, x-2)
	offset, cells := row.start, 0
	for offset < row.end && cells < cx {
		cells += ansi.StringWidth(string(runes[offset]))
		offset++
	}
	return offset, true
}

func (m *model) setCursorOffset(offset int) tea.Cmd {
	lines := strings.Split(m.input.Value(), "\n")
	row, col := 0, offset
	for row < len(lines)-1 && col > len([]rune(lines[row])) {
		col -= len([]rune(lines[row])) + 1
		row++
	}
	col = min(max(col, 0), len([]rune(lines[row])))
	// Vertical movement works on visual rows, so step until the logical row matches.
	guard := len(m.composerRows()) + 1
	for ; m.input.Line() > row && guard > 0; guard-- {
		m.input.CursorUp()
	}
	for ; m.input.Line() < row && guard > 0; guard-- {
		m.input.CursorDown()
	}
	m.input.SetCursor(col)
	return m.updateInput(composerNoop{})
}

func (m *model) composerCursorOffset() int {
	lines := strings.Split(m.input.Value(), "\n")
	row := min(m.input.Line(), len(lines)-1)
	if row < 0 {
		return 0
	}
	offset := 0
	for i := 0; i < row; i++ {
		offset += len([]rune(lines[i])) + 1
	}
	info := m.input.LineInfo()
	col := info.StartColumn + info.ColumnOffset
	lineRunes := len([]rune(lines[row]))
	return offset + min(max(0, col), lineRunes)
}
