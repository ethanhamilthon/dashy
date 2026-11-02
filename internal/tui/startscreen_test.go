package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestStartScreenShowsVersionAndSetupHint(t *testing.T) {
	m, _ := pickerModel(t)
	Version = "1.2.3"
	m.refresh(true)
	view := m.timeline()
	for _, want := range []string{"v1.2.3", "/login", "/model"} {
		if !strings.Contains(view, want) {
			t.Fatalf("start screen missing %q:\n%s", want, view)
		}
	}
	m.input.SetValue("hello")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if cmd != nil || m.ctl.Status().Busy {
		t.Fatal("turn started without provider and model")
	}
}

func TestStartScreenShowsUpdateHint(t *testing.T) {
	m, _ := pickerModel(t)
	Version = "0.2.2"
	m.latest = "v0.2.3"
	m.refresh(true)
	if view := m.timeline(); !strings.Contains(view, "v0.2.3 available — run dashy update") {
		t.Fatalf("start screen missing update hint:\n%s", view)
	}
	next, _ := m.Update(updateCheckMsg{latest: "v0.2.3"})
	if next.(model).latest != "v0.2.3" {
		t.Fatal("update hint was not stored")
	}
}
