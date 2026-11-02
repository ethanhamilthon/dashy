package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"dashy/internal/agent"
)

func TestComposerIsInScrollableTimeline(t *testing.T) {
	m := bareModel(t, "/project")
	view := strings.Split(ansi.Strip(m.View()), "\n")
	inputRow := -1
	for i, line := range view {
		if strings.HasPrefix(line, "▌") {
			inputRow = i
			break
		}
	}
	if inputRow < 0 || inputRow > 5 {
		t.Fatalf("input pinned at row %d", inputRow)
	}
	for i := 0; i < 30; i++ {
		m.add("user", fmt.Sprintf("message %d", i))
	}
	m.refresh(true)
	if !strings.Contains(ansi.Strip(m.View()), "▌") {
		t.Fatal("input missing at timeline end")
	}
	m.viewport.GotoTop()
	if strings.Contains(ansi.Strip(m.View()), m.input.Placeholder) {
		t.Fatal("composer must scroll with history")
	}
	before := m.viewport.YOffset
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = next.(model)
	if m.viewport.YOffset <= before {
		t.Fatal("PgDn did not scroll")
	}
	next, _ = m.Update(agentEvent(agent.Event{Kind: "delta", Text: "streaming"}))
	m = next.(model)
	if m.viewport.YOffset != before+m.viewport.Height {
		t.Fatal("streaming should not steal scroll position")
	}
}
func TestCommandCompletionAndHelp(t *testing.T) {
	m, a, release := heldModel(t)
	defer release()
	m.input.SetValue("/")
	if len(m.completions()) != len(commands) {
		t.Fatal("missing commands")
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if m.input.Value() != "/help" {
		t.Fatalf("completion %q", m.input.Value())
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if !strings.Contains(m.entries[len(m.entries)-1].Text, "/new") {
		t.Fatal("help must list commands")
	}
	if len(a.History) != 0 {
		t.Fatal("commands must not be sent to model")
	}
	m = startBusy(t, m)
	m.input.SetValue("/nope")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if queued := m.ctl.Status().Queued; len(queued) != 1 || queued[0].Text != "/nope" {
		t.Fatal("slash-prefixed text must be submitted as an ordinary instruction")
	}
}
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

func TestMultilineEditing(t *testing.T) {
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEnter, Alt: true}, {Type: tea.KeyCtrlJ}} {
		m := bareModel(t, "/project")
		m.input.SetValue("onetwo")
		m.input.SetCursor(3)
		next, _ := m.Update(key)
		m = next.(model)
		if m.input.Value() != "one\ntwo" || m.input.Height() != 2 {
			t.Fatalf("%s: value %q, height %d", key, m.input.Value(), m.input.Height())
		}
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
		m = next.(model)
		if m.input.Line() != 0 {
			t.Fatal("up did not move to previous line")
		}
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

// /new and /quit typed during a turn cancel it and run once it has ended. The turn
// ends on its own (cancellation completes asynchronously), so only the outcome is
// asserted: the session switch or close happens after the turn, never during it.
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
