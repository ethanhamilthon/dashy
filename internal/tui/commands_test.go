package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"dashy/internal/agent"
	tea "github.com/charmbracelet/bubbletea"
)

func TestEnterExecutesSelectedCommandWithoutExtraConfirmation(t *testing.T) {
	for _, text := range []string{"/", "/he", "/help"} {
		m, _ := testModel(t, nil)
		m.input.SetValue(text)
		if text == "/" {
			m.selected = 1
		}
		m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
		if m.ctl.Status().Busy || len(m.entries) != 1 || !strings.Contains(m.entries[0].Text, "Enter:") {
			t.Fatalf("%q: command not executed immediately: %+v", text, m.entries)
		}
		if m.input.Value() != "" {
			t.Fatalf("%q: composer not cleared", text)
		}
	}
}

func TestSlashTextIsOrdinarySteeringWithoutCommandCompletion(t *testing.T) {
	for _, text := range []string{"/tmp/example.go", "/unknown", "/help me with this", "/new\nexplain this", "/resume missing-id"} {
		m, release := busyModel(t)
		before := len(m.entries)
		m.input.SetValue(text)
		m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
		queued := m.ctl.Status().Queued
		if len(queued) != 1 || queued[0].Text != text || len(m.entries) != before {
			t.Fatalf("%q: got queued=%v, entries=%v", text, queued, m.entries)
		}
		release()
		awaitIdle(t, m)
	}
}

func TestDismissedCompletionAllowsLiteralCommandText(t *testing.T) {
	m, release := busyModel(t)
	defer release()
	m.input.SetValue("/help")
	m.hideCompletion = true
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.ctl.Status().Queued; len(got) != 1 || got[0].Text != "/help" {
		t.Fatalf("dismissed completion still executed command: %v", got)
	}
}

func TestNavigationCommandsAndUnsentSessionNotSaved(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	writer, err := agent.New(cwd, agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if sessions, _ := writer.Sessions(); len(sessions) != 0 {
		t.Fatalf("empty session created before first message: %v", sessions)
	}
	writer.History = append(writer.History, []byte(`{"role":"user","content":"saved"}`))
	if err = writer.Save(); err != nil {
		t.Fatal(err)
	}
	m := newModel(writer, cwd)
	defer m.ctl.Close()
	for _, r := range "/help" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.entries) == 0 || !strings.Contains(m.entries[len(m.entries)-1].Text, "/new") {
		t.Fatalf("help failed: %+v", m.entries)
	}
	m.input.SetValue("/new")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.input.SetValue("/quit")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd == nil {
		t.Fatal("/quit did not exit")
	}
	if sessions, _ := writer.Sessions(); len(sessions) != 1 {
		t.Fatalf("unsent /new session was saved on /quit: %v", sessions)
	}
}

func TestRenameSessionPersistsTitle(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := agent.New(cwd, agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.History = append(a.History, []byte(`{"role":"user","content":"first message"}`))
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	m := newModel(a, cwd)
	defer m.ctl.Close()
	run := func(text string) {
		m.input.SetValue(text)
		m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	}
	title := func() string {
		sessions, err := a.Sessions()
		if err != nil || len(sessions) != 1 {
			t.Fatalf("sessions: %v %v", sessions, err)
		}
		return sessions[0].Title
	}
	run("/rename")
	if m.input.Value() != "/rename " {
		t.Fatalf("bare /rename should prompt for a name, got %q", m.input.Value())
	}
	run("/rename  My   project ")
	if got := title(); got != "My project" {
		t.Fatalf("title after rename = %q", got)
	}
	id := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(id); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if got := title(); got != "My project" {
		t.Fatalf("title lost after resume and save = %q", got)
	}
	run("/rename ")
	if got := title(); got != "first message" {
		t.Fatalf("empty rename should restore default, got %q", got)
	}
}
