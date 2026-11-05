package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func TestShiftEnterTerminalSequences(t *testing.T) {
	for _, sequence := range []string{"\x1b[13;2u", "\x1b[27;2;13~", "\x1b[13;2~"} {
		t.Run(fmt.Sprintf("%q", sequence), func(t *testing.T) {
			m := bareModel(t, "/project")
			p := tea.NewProgram(m,
				tea.WithInput(strings.NewReader("one"+sequence+"two\x03")),
				tea.WithOutput(&strings.Builder{}),
				tea.WithoutRenderer(),
				tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
					if key, ok := msg.(tea.KeyMsg); ok && key.Type == tea.KeyCtrlC {
						return tea.QuitMsg{}
					}
					return msg
				}),
			)
			result, err := p.Run()
			if err != nil {
				t.Fatal(err)
			}
			got := result.(model)
			if got.input.Value() != "one\ntwo" || got.input.Height() != 2 || len(got.entries) != 0 {
				t.Fatalf("value %q, height %d, entries %v", got.input.Value(), got.input.Height(), got.entries)
			}
		})
	}
}

func TestInputHeightMatchesWrappedRows(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("a", 78),
		strings.Repeat("word ", 40),
		strings.Repeat("界", 80),
		"first\n\nlast\n",
	} {
		m := bareModel(t, "/project")
		m.input.SetValue(text)
		m.refresh(false)
		probe := m.input
		rows := 0
		for _, line := range strings.Split(text, "\n") {
			probe.SetValue(line)
			rows += probe.LineInfo().Height
		}
		if m.input.Height() != min(rows, m.height/2) {
			t.Errorf("%q: height %d, rows %d", text, m.input.Height(), rows)
		}
		for _, line := range strings.Split(m.inputBlock(), "\n") {
			if ansi.StringWidth(line) != m.width {
				t.Errorf("width %d want %d", ansi.StringWidth(line), m.width)
			}
		}
	}
}

func TestCommandsWaitForTurnCompletion(t *testing.T) {
	for _, name := range []string{"/new", "/quit"} {
		t.Run(name, func(t *testing.T) {
			m, a, _ := heldModel(t)
			a.History = []json.RawMessage{json.RawMessage(`{"role":"user","content":"keep"}`)}
			m = startBusy(t, m)
			m.input.SetValue(name)
			m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
			m = awaitIdle(t, m)
			if name == "/new" {
				if len(a.History) != 0 || len(m.entries) != 0 {
					t.Fatalf("new did not reset session: history=%d entries=%+v", len(a.History), m.entries)
				}
				return
			}
			select {
			case <-m.ctl.Closed():
			case <-time.After(5 * time.Second):
				t.Fatal("quit command did not close the session")
			}
		})
	}
}
