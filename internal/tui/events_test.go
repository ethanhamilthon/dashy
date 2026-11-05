package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"dashy/internal/agent"
)

// Notices and errors go to the status bar and /events; the chat keeps only <notify> messages.
func TestEventsGoToStatusBar(t *testing.T) {
	m := bareModel(t, "/p")
	m.width = 120
	m.input.SetValue("hello")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.entries) != 0 {
		t.Fatalf("an error reached the chat: %+v", m.entries)
	}
	if bar := ansi.Strip(m.bottomBar()); !strings.Contains(bar, "No provider configured") || len(bar) > 200 {
		t.Fatalf("status bar: %q", bar)
	}
	next, _ := m.Update(flashExpired{m.flashAt.Add(-time.Second)})
	if m = next.(model); m.flash.Text == "" {
		t.Fatal("an older timer cleared a newer event")
	}
	next, _ = m.Update(flashExpired{m.flashAt})
	if m = next.(model); strings.Contains(ansi.Strip(m.bottomBar()), "no provider") {
		t.Fatal("event did not expire")
	}
	m.ctl.Note(agent.Event{Kind: "notice", Text: "<view>\nhi\n</view>"})
	m.input.SetValue("/events")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.entries) != 2 || m.entries[0].Kind != "notice" || !strings.Contains(m.entries[1].Text, "No provider configured.") {
		t.Fatalf("entries: %+v", m.entries)
	}
}

func TestShortEventsAndTaskCount(t *testing.T) {
	for text, want := range map[string]string{"Copied 3 lines to the clipboard.": "Copied 3 lines to the clipboard", "Selected p · gpt-5.1/high; saved to x.": "Selected p · gpt-5.1/high · /events", "Copy failed: no tool": "Copy failed · /events"} {
		if got := shortEvent(text); got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
	if tasksRunning(0, " · ") != "" || tasksRunning(1, " ") != " 1 task running" || tasksRunning(2, " · ") != " · 2 tasks running" {
		t.Fatal(tasksRunning(2, " · "))
	}
}
