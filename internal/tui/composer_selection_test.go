package tui

import (
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
)

func composerModel(t *testing.T, value string, width, height int) model {
	t.Helper()
	m := bareModel(t, "/project")
	m.width, m.height = width, height
	m.input.SetValue(value)
	m.refresh(true)
	return m
}

func composerPoint(m model, r, c int) (int, int) {
	return 2 + c, m.inputTop - m.viewport.YOffset + r
}

func mouseAt(action tea.MouseAction, x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: action}
}

func update(m model, msg tea.Msg) model {
	next, _ := m.Update(msg)
	return next.(model)
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

func probeLayout(value string, width int) []composerRow {
	if value == "" {
		return nil
	}
	probe := textarea.New()
	probe.CharLimit = 0
	probe.MaxWidth = 10000
	probe.Prompt = ""
	probe.ShowLineNumbers = false
	probe.SetWidth(width)
	var rows []composerRow
	base := 0
	for _, line := range strings.Split(value, "\n") {
		runes := []rune(line)
		probe.SetValue(line)
		col := 0
		for {
			probe.SetCursor(col)
			info := probe.LineInfo()
			end := min(info.StartColumn+info.Width, len(runes))
			rows = append(rows, composerRow{base + info.StartColumn, base + end})
			if info.RowOffset+1 >= info.Height || info.Width <= 0 {
				break
			}
			col = info.StartColumn + info.Width
		}
		base += len(runes) + 1
	}
	return rows
}

func TestComposerLayoutMatchesTextareaProbe(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	pieces := []string{"a", "word", "longerword", " ", "  ", "界", "é", "-", "\n", "supercalifragilistic"}
	for i := 0; i < 400; i++ {
		var b strings.Builder
		for n := r.Intn(30); n > 0; n-- {
			b.WriteString(pieces[r.Intn(len(pieces))])
		}
		value, width := b.String(), 1+r.Intn(20)
		if got, want := composerLayout(value, width), probeLayout(value, width); !slices.Equal(got, want) {
			t.Fatalf("%q width %d: %v, textarea %v", value, width, got, want)
		}
	}
}
