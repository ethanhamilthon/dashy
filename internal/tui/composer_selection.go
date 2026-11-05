package tui

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// composerSelection is a rune range of the composer value selected with the mouse.
type composerSelection struct {
	anchor, head int    // rune offsets into value
	value        string // composer value when selected; a change invalidates the selection
}

// bounds returns the ordered rune range of the selection.
func (s composerSelection) bounds() (int, int) {
	return min(s.anchor, s.head), max(s.anchor, s.head)
}

// composerRow is one soft-wrapped visual row of the composer value: rune offsets, end exclusive.
type composerRow struct{ start, end int }

// composerShortcut is Cmd/Meta+C or Cmd/Meta+X. It only acts on a composer selection.
type composerShortcut struct{ cut bool }

// composerNoop triggers a textarea update, which scrolls its viewport to the cursor.
type composerNoop struct{}

// composerLayout returns the visual rows that the composer textarea renders for value.
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
		case wordWidth+lastWidth > width: // the word fills a whole row
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

// composerRows returns the cached layout of the current composer value.
func (m *model) composerRows() []composerRow {
	value, width := m.input.Value(), m.input.Width()
	if m.layoutValue != value || m.layoutWidth != width {
		m.layoutValue, m.layoutWidth = value, width
		m.layout = composerLayout(value, width)
	}
	return m.layout
}

// composerCursorRow returns the visual row index of the textarea cursor.
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

// composerOffset maps a screen cell to a rune offset of the composer value. With clamp, cells
// outside the composer map to its nearest edge, as used while dragging.
func (m *model) composerOffset(x, y int, clamp bool) (int, bool) {
	value := m.input.Value()
	if value == "" || m.keyInComposer() {
		return 0, false
	}
	rows := m.composerRows()
	height := m.input.Height()
	r := y + m.viewport.YOffset - m.inputTop
	if clamp {
		r = min(max(r, 0), height-1)
	} else if r < 0 || r >= height {
		return 0, false
	}
	row := rows[min(len(rows)-1, m.composerScroll(rows)+r)]
	runes := []rune(value)
	cx := max(0, x-2) // the rail and its space take two cells
	offset, cells := row.start, 0
	for offset < row.end && cells < cx {
		cells += ansi.StringWidth(string(runes[offset]))
		offset++
	}
	return offset, true
}

// activeComposerSelection reports the selected range when it still matches the composer value.
func (m *model) activeComposerSelection() (int, int, bool) {
	if m.inputSel == nil {
		return 0, 0, false
	}
	if m.inputSel.value != m.input.Value() {
		m.inputSel = nil
		return 0, 0, false
	}
	lo, hi := m.inputSel.bounds()
	return lo, hi, lo < hi
}

// selectedComposerText returns the selected composer text.
func (m *model) selectedComposerText() string {
	lo, hi, ok := m.activeComposerSelection()
	if !ok {
		return ""
	}
	return string([]rune(m.input.Value())[lo:hi])
}

// setCursorOffset moves the textarea cursor to a rune offset of its value.
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

// replaceComposerSelection replaces the selected composer text with text.
func (m *model) replaceComposerSelection(text string) tea.Cmd {
	lo, hi, ok := m.activeComposerSelection()
	if !ok {
		return nil
	}
	runes := []rune(m.input.Value())
	m.input.SetValue(string(runes[:lo]) + text)
	// SetValue sanitizes input, so the cursor follows the sanitized length.
	cursor := len([]rune(m.input.Value()))
	m.input.SetValue(m.input.Value() + string(runes[hi:]))
	m.inputSel = nil
	m.selected = 0
	m.hideCompletion = false
	return m.setCursorOffset(cursor)
}

// composerHighlight reverses the selected runes in the composer rows of the rendered timeline.
func (m *model) composerHighlight(lines []string) []string {
	lo, hi, ok := m.activeComposerSelection()
	if !ok {
		return lines
	}
	rows := m.composerRows()
	if len(rows) == 0 {
		return lines
	}
	height := m.input.Height()
	scroll := m.composerScroll(rows)
	for y := range lines {
		r := y + m.viewport.YOffset - m.inputTop
		if r < 0 || r >= height {
			continue
		}
		row := rows[min(len(rows)-1, scroll+r)]
		from, to := max(lo, row.start), min(hi, row.end)
		if from >= to {
			continue
		}
		// The rendered row keeps one rune per text rune after the two-cell rail.
		plain := []rune(ansi.Strip(lines[y]))
		a, b := min(2+from-row.start, len(plain)), min(2+to-row.start, len(plain))
		if a >= b {
			continue
		}
		lines[y] = string(plain[:a]) + "\x1b[7m" + string(plain[a:b]) + "\x1b[0m" + string(plain[b:])
	}
	return lines
}

// composerKey applies a key to an active composer selection. It reports whether the key was
// consumed; otherwise the selection is cleared when the key changes the composer, and the
// key keeps its normal meaning.
func (m *model) composerKey(key tea.KeyMsg) (tea.Cmd, bool) {
	if _, _, ok := m.activeComposerSelection(); !ok {
		return nil, false
	}
	switch key.String() {
	case "backspace", "delete", "ctrl+h":
		cmd := m.replaceComposerSelection("")
		m.refresh(true)
		return tea.Batch(cmd, m.checkFileCompletion()), true
	case "alt+enter", "ctrl+j":
		cmd := m.replaceComposerSelection("\n")
		m.refresh(true)
		return tea.Batch(cmd, m.checkFileCompletion()), true
	case "ctrl+c":
		return copyText(m.selectedComposerText()), true
	case "ctrl+x":
		copied := copyText(m.selectedComposerText())
		cut := m.replaceComposerSelection("")
		m.refresh(true)
		return tea.Batch(copied, cut, m.checkFileCompletion()), true
	case "ctrl+v":
		// Clipboard text replaces the selection when the read completes.
		return nil, false
	case "esc":
		m.inputSel = nil
		return nil, false
	}
	if key.Type == tea.KeySpace || (key.Type == tea.KeyRunes && (!key.Alt || key.Paste)) {
		text := string(key.Runes)
		if key.Type == tea.KeySpace {
			text = " "
		}
		cmd := m.replaceComposerSelection(text)
		m.refresh(true)
		return tea.Batch(cmd, m.checkFileCompletion()), true
	}
	m.inputSel = nil
	return nil, false
}

// composerCursorOffset returns the current cursor position as a rune offset into value.
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
