package tui

import (
	"dashy/internal/osc7501"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/agent"
)

func TestUpRecallsOnlyPendingSteeringOnEmptyInput(t *testing.T) {
	m, release := busyModel(t)
	defer release()
	for _, text := range []string{"first", "second"} {
		m.input.SetValue(text)
		m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "second" || len(m.ctl.Status().Queued) != 1 {
		t.Fatal("up did not recall last pending instruction")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "second" || len(m.ctl.Status().Queued) != 1 {
		t.Fatal("nonempty draft overwritten")
	}
	m.input.Reset()
	release() // the agent accepts "first" at its next boundary
	m = awaitIdle(t, m)
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.input.Value() != "" {
		t.Fatal("accepted instruction resurrected")
	}
}

func TestResumeAutocompleteIsScopedAndExecutesSelectedSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := agent.New(cwd, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.History = append(a.History, []byte(`{"role":"user","content":"older chat"}`))
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	oldID := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err = a.NewSession(); err != nil {
		t.Fatal(err)
	}
	b, err := agent.New(t.TempDir(), agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	foreign := strings.TrimSuffix(filepath.Base(b.HistoryPath), ".jsonl")
	m := newModel(a, cwd, osc7501.New(io.Discard, false))
	m.input.SetValue("/resume")
	matches := m.completions()
	if len(matches) != 1 {
		t.Fatalf("session choices: %v", matches)
	}
	for i, c := range matches {
		if strings.Contains(c.name, foreign) {
			t.Fatal("another cwd leaked into completion")
		}
		if c.name == "/resume "+oldID {
			m.selected = i
		}
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = next.(model)
	if m.input.Value() != "/resume "+oldID {
		t.Fatal("Tab did not complete selected session")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = settle(next.(model))
	if len(m.entries) != 1 || m.entries[0].Text != "older chat" || m.input.Value() != "" {
		t.Fatalf("resume failed: %+v", m.entries)
	}
	m.input.SetValue("/resume " + oldID[:12])
	if len(m.completions()) == 0 {
		t.Fatal("session prefix filtering failed")
	}
	if _, err = os.Stat(a.HistoryPath); err != nil {
		t.Fatal(err)
	}
}
