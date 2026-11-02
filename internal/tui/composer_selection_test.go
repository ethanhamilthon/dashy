package tui

import (
	"fmt"
	"strings"
	"testing"

	"dashy/internal/agent"
	"dashy/internal/shell"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// composerModel returns a model whose composer holds value, sized to width x height.
func composerModel(value string, width, height int) model {
	m := newModel(&agent.Agent{}, "/project")
	m.width, m.height = width, height
	m.input.SetValue(value)
	m.refresh(true)
	return m
}

// composerPoint returns the screen cell of visual composer row r at text column c.
// The two-cell rail precedes the text.
func composerPoint(m model, r, c int) (int, int) {
	return 2 + c, m.inputTop - m.viewport.YOffset + r
}

func mouseAt(action tea.MouseAction, x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: action}
}

// dragComposer selects the composer text between two visual cells.
func dragComposer(m *model, fromRow, fromCol, toRow, toCol int) {
	x, y := composerPoint(*m, fromRow, fromCol)
	m.mouseSelection(mouseAt(tea.MouseActionPress, x, y))
	x, y = composerPoint(*m, toRow, toCol)
	m.mouseSelection(mouseAt(tea.MouseActionMotion, x, y))
	m.mouseSelection(mouseAt(tea.MouseActionRelease, x, y))
}

func update(m model, msg tea.Msg) model {
	next, _ := m.Update(msg)
	return next.(model)
}

// selectBrave selects "brave" in "hello brave world".
func selectBrave(t *testing.T) model {
	t.Helper()
	m := composerModel("hello brave world", 40, 20)
	dragComposer(&m, 0, 6, 0, 11)
	if got := m.selectedComposerText(); got != "brave" {
		t.Fatalf("selected %q, want brave", got)
	}
	return m
}

func TestComposerLayoutWrapsLikeTextarea(t *testing.T) {
	cases := []struct {
		value string
		width int
	}{
		{"alpha beta gamma delta epsilon zeta", 10},
		{"short\n\nsecond line that wraps around\n", 12},
		{strings.Repeat("word界 ", 12) + "tail", 9},
		{"trailing spaces   ", 6},
	}
	for _, c := range cases {
		rows := composerLayout(c.value, c.width)
		ta := textarea.New()
		ta.Prompt = ""
		ta.ShowLineNumbers = false
		ta.CharLimit = 0
		ta.MaxWidth = 10000
		ta.SetWidth(c.width)
		want := 0
		for _, line := range strings.Split(c.value, "\n") {
			ta.SetValue(line)
			want += ta.LineInfo().Height
		}
		if len(rows) != want {
			t.Fatalf("%q width %d: %d rows, textarea renders %d", c.value, c.width, len(rows), want)
		}
		runes := []rune(c.value)
		if rows[0].start != 0 || rows[len(rows)-1].end != len(runes) {
			t.Fatalf("%q: rows do not cover the value: %+v", c.value, rows)
		}
		for i := 1; i < len(rows); i++ {
			prevEnd := rows[i-1].end
			gap := rows[i].start - prevEnd
			newline := prevEnd < len(runes) && runes[prevEnd] == '\n'
			if gap < 0 || gap > 1 || (gap == 1) != newline {
				t.Fatalf("%q width %d: row %d starts at %d after %d (newline=%v)", c.value, c.width, i, rows[i].start, prevEnd, newline)
			}
		}
	}
}

func TestComposerClickMapsEveryWrappedRow(t *testing.T) {
	value := "alpha beta gamma delta epsilon zeta"
	m := composerModel(value, 14, 20)
	for r, row := range composerLayout(value, m.input.Width()) {
		x, y := composerPoint(m, r, 0)
		offset, ok := m.composerOffset(x, y, false)
		if !ok || offset != row.start {
			t.Fatalf("row %d: offset %d %v, want %d", r, offset, ok, row.start)
		}
	}
}

func TestComposerDragSelectsWithoutCopy(t *testing.T) {
	m := composerModel("hello brave world", 40, 20)
	dragComposer(&m, 0, 6, 0, 11)
	if m.sel != nil {
		t.Fatal("composer drag created a timeline selection")
	}
	lo, hi, ok := m.activeComposerSelection()
	if !ok || lo != 6 || hi != 11 {
		t.Fatalf("selection = %d..%d %v, want 6..11", lo, hi, ok)
	}
	if got := m.input.Value(); got != "hello brave world" {
		t.Fatalf("drag changed the text: %q", got)
	}
}

func TestComposerClickPlacesCursor(t *testing.T) {
	m := composerModel("hello world", 40, 20)
	x, y := composerPoint(m, 0, 6)
	m.mouseSelection(mouseAt(tea.MouseActionPress, x, y))
	m.mouseSelection(mouseAt(tea.MouseActionRelease, x, y))
	if _, _, ok := m.activeComposerSelection(); ok {
		t.Fatal("click without drag left a selection")
	}
	m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	if got := m.input.Value(); got != "hello Xworld" {
		t.Fatalf("typing after click = %q, want %q", got, "hello Xworld")
	}
}

func TestComposerBackspaceDeletesSelection(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyBackspace}, {Type: tea.KeyDelete}} {
		m := selectBrave(t)
		m = update(m, key)
		if got := m.input.Value(); got != "hello  world" {
			t.Fatalf("%s: value = %q", key.String(), got)
		}
		m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
		if got := m.input.Value(); got != "hello X world" {
			t.Fatalf("%s: typing after delete = %q", key.String(), got)
		}
	}
}

func TestComposerTypingReplacesSelection(t *testing.T) {
	m := selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("X")})
	if got := m.input.Value(); got != "hello X world" {
		t.Fatalf("rune = %q", got)
	}
	m = selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	if got := m.input.Value(); got != "hello   world" {
		t.Fatalf("space = %q", got)
	}
	m = selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pasted\ntext"), Paste: true})
	if got := m.input.Value(); got != "hello pasted\ntext world" {
		t.Fatalf("bracketed paste = %q", got)
	}
	m = selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if got := m.input.Value(); got != "hello \n world" {
		t.Fatalf("alt+enter = %q", got)
	}
}

func TestComposerCtrlCCopiesInsteadOfQuit(t *testing.T) {
	m := selectBrave(t)
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m = next.(model)
	if cmd == nil {
		t.Fatal("selected text was not copied")
	}
	if got := m.input.Value(); got != "hello brave world" {
		t.Fatalf("copy changed the text: %q", got)
	}
	if m.selectedComposerText() != "brave" {
		t.Fatal("copy cleared the selection")
	}
	if m.ctl.Status().Cancelling {
		t.Fatal("copy requested cancellation")
	}
}

func TestComposerCtrlXCutsSelectionKeepsImages(t *testing.T) {
	m := selectBrave(t)
	m.images = []shell.Image{{URL: "data:image/png;base64,one"}}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m = next.(model)
	if cmd == nil {
		t.Fatal("cut did not copy the selection")
	}
	if got := m.input.Value(); got != "hello  world" {
		t.Fatalf("cut value = %q", got)
	}
	if len(m.images) != 1 {
		t.Fatal("cut with a selection removed an image")
	}

	m = composerModel("hello world", 40, 20)
	m.images = []shell.Image{{URL: "data:image/png;base64,one"}}
	m = update(m, tea.KeyMsg{Type: tea.KeyCtrlX})
	if len(m.images) != 0 {
		t.Fatal("ctrl+x without a selection did not remove the image")
	}
}

func TestComposerShortcutOnlyActsWithSelection(t *testing.T) {
	m := composerModel("hello world", 40, 20)
	next, cmd := m.Update(composerShortcut{cut: true})
	m = next.(model)
	if cmd != nil || m.input.Value() != "hello world" {
		t.Fatalf("Cmd+X without selection changed state: cmd=%v value=%q", cmd, m.input.Value())
	}
	m = selectBrave(t)
	next, cmd = m.Update(composerShortcut{cut: false})
	m = next.(model)
	if cmd == nil || m.input.Value() != "hello brave world" {
		t.Fatalf("Cmd+C with selection: cmd=%v value=%q", cmd, m.input.Value())
	}
}

func TestComposerPasteReplacesSelection(t *testing.T) {
	m := selectBrave(t)
	m.acceptClipboard(clipboardResult{generation: m.pasteGeneration, text: "new"})
	m.refresh(true)
	if got := m.input.Value(); got != "hello new world" {
		t.Fatalf("paste = %q", got)
	}
}

func TestComposerSelectionClearedByNavigationAndExternalChange(t *testing.T) {
	m := selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyLeft})
	if _, _, ok := m.activeComposerSelection(); ok || m.input.Value() != "hello brave world" {
		t.Fatal("left arrow did not clear the selection without changing text")
	}

	m = selectBrave(t)
	m = update(m, tea.KeyMsg{Type: tea.KeyEsc})
	if _, _, ok := m.activeComposerSelection(); ok || m.input.Value() != "hello brave world" {
		t.Fatal("esc did not clear the selection without changing text")
	}

	m = selectBrave(t)
	m.input.SetValue("other")
	if _, _, ok := m.activeComposerSelection(); ok {
		t.Fatal("selection survived an external value change")
	}
}

func TestComposerSelectionHighlighted(t *testing.T) {
	m := selectBrave(t)
	m.refresh(false)
	var line string
	for _, row := range strings.Split(m.View(), "\n") {
		if strings.Contains(ansi.Strip(row), "hello brave world") {
			line = row
		}
	}
	if line == "" {
		t.Fatal("composer row missing from view")
	}
	if !strings.Contains(line, "\x1b[7mbrave\x1b[0m") {
		t.Fatalf("selected text not highlighted: %q", line)
	}
	if !strings.Contains(ansi.Strip(line), "▌ hello brave world") {
		t.Fatalf("highlight changed the text: %q", ansi.Strip(line))
	}
}

func TestWheelScrollsWithComposerSelection(t *testing.T) {
	m := composerModel("hello brave world", 40, 10)
	for i := 0; i < 30; i++ {
		m.add("notice", fmt.Sprintf("entry %d", i))
	}
	m.refresh(true)
	dragComposer(&m, 0, 6, 0, 11)
	before := m.viewport.YOffset
	m = update(m, tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.viewport.YOffset >= before {
		t.Fatalf("wheel did not scroll: %d -> %d", before, m.viewport.YOffset)
	}
	if m.selectedComposerText() != "brave" {
		t.Fatal("wheel cleared the composer selection")
	}
}

func TestTimelineDragStillCopies(t *testing.T) {
	m := composerModel("hello", 40, 20)
	m.add("user", "timeline text")
	m.refresh(true)
	if m.viewport.YOffset != 0 {
		t.Fatalf("test timeline must fit: offset %d", m.viewport.YOffset)
	}
	cmd, _ := m.mouseSelection(mouseAt(tea.MouseActionPress, 0, 0))
	if cmd != nil || m.sel == nil {
		t.Fatal("timeline press did not start a selection")
	}
	m.mouseSelection(mouseAt(tea.MouseActionMotion, 4, 0))
	cmd, _ = m.mouseSelection(mouseAt(tea.MouseActionRelease, 4, 0))
	if cmd == nil {
		t.Fatal("timeline drag was not copied on release")
	}
	if m.inputSel != nil {
		t.Fatal("timeline drag created a composer selection")
	}
}

func TestComposerSelectionDisabledForMaskedKey(t *testing.T) {
	m := newModel(&agent.Agent{}, "/project")
	m.width, m.height = 40, 20
	m.menuKind = "setup"
	m.setup = []string{"name", "https://example.com"}
	m.input.SetValue("secret")
	m.refresh(true)
	x, y := composerPoint(m, 0, 2)
	m.mouseSelection(mouseAt(tea.MouseActionPress, x, y))
	m.mouseSelection(mouseAt(tea.MouseActionMotion, x+3, y))
	if m.inputSel != nil {
		t.Fatal("masked API key field produced a composer selection")
	}
}

func TestComposerSelectionInsideScrolledValue(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	value := strings.Join(lines, "\n")
	m := composerModel(value, 40, 20)
	m = update(m, struct{}{}) // the textarea scrolls to its cursor on the next update

	top := m.inputTop - m.viewport.YOffset
	shown := strings.TrimSpace(strings.TrimPrefix(ansi.Strip(strings.Split(m.View(), "\n")[top]), "▌"))
	var first int
	if _, err := fmt.Sscanf(shown, "line %d", &first); err != nil || first != 20 {
		t.Fatalf("first visible row = %q, want line 20", shown)
	}

	dragComposer(&m, 0, 5, 1, 3)
	start, end := first*8+5, (first+1)*8+3
	if got, want := m.selectedComposerText(), value[start:end]; got != want {
		t.Fatalf("selected %q, want %q", got, want)
	}
}

func TestComposerSelectionMatchesRowsBeforeTextareaScrolls(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	value := strings.Join(lines, "\n")
	// SetValue leaves the textarea showing its first rows while the cursor sits at the end.
	m := composerModel(value, 40, 20)
	top := m.inputTop - m.viewport.YOffset
	shown := strings.TrimSpace(strings.TrimPrefix(ansi.Strip(strings.Split(m.View(), "\n")[top]), "▌"))
	if shown != "line 00" {
		t.Fatalf("first visible row = %q, want line 00", shown)
	}
	dragComposer(&m, 0, 5, 0, 7)
	if got := m.selectedComposerText(); got != "00" {
		t.Fatalf("selected %q, want 00", got)
	}
}

func TestComposerDragDoesNotScrollOnPress(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("line %02d", i))
	}
	m := composerModel(strings.Join(lines, "\n"), 40, 20)
	x, y := composerPoint(m, 0, 5)
	m.mouseSelection(mouseAt(tea.MouseActionPress, x, y))
	x, y = composerPoint(m, 1, 7)
	m.mouseSelection(mouseAt(tea.MouseActionMotion, x, y))
	m.mouseSelection(mouseAt(tea.MouseActionRelease, x, y))
	if got := m.selectedComposerText(); got != "00\nline 01" {
		t.Fatalf("selected %q", got)
	}
}
