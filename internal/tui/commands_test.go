package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/agent"
)

func menuNames(m model) []string {
	var names []string
	for _, c := range m.menu {
		names = append(names, c.name)
	}
	return names
}

// /settings opens the display settings rows, /models the model list and /connect the providers.
func TestSettingsModelsAndConnectCommandsOpenTheirPickers(t *testing.T) {
	f := newFixture(t, true)
	f.command(t, "/settings")
	if f.m.menuKind != pickerSettings {
		t.Fatalf("/settings opened %q", f.m.menuKind)
	}
	names := strings.Join(menuNames(f.m), "|")
	for _, row := range []string{"Reasoning summary", "Show lines"} {
		if !strings.Contains(names, row) {
			t.Fatalf("settings rows %q lack %q", names, row)
		}
	}
	f.key(tea.KeyMsg{Type: tea.KeyEsc})
	f.command(t, "/models")
	if f.m.menuKind != pickerModels {
		t.Fatalf("/models opened %q", f.m.menuKind)
	}
	f.key(tea.KeyMsg{Type: tea.KeyEsc})
	f.command(t, "/connect")
	if f.m.menuKind != pickerConnect || !strings.Contains(strings.Join(menuNames(f.m), "|"), "p|Add") {
		t.Fatalf("/connect opened %q with %v", f.m.menuKind, menuNames(f.m))
	}
}

// First run: no provider yet, so the provider form opens; with providers but no
// model, the models open; with both, nothing does.
func TestOnboarding(t *testing.T) {
	f := newFixture(t, false)
	if f.m.onboard(); f.m.menuKind != pickerProvider || !f.m.form.welcome {
		t.Fatalf("first run opened %q", f.m.menuKind)
	}
	f.key(tea.KeyMsg{Type: tea.KeyEsc})
	if f.m.menuKind != "" {
		t.Fatalf("Esc left %q open", f.m.menuKind)
	}
	if err := f.m.ctl.SaveProvider("", "work", "http://127.0.0.1:9", ""); err != nil {
		t.Fatal(err)
	}
	if f.m.onboard(); f.m.menuKind != pickerModels {
		t.Fatalf("no model opened %q", f.m.menuKind)
	}
	ready := newFixture(t, true)
	if ready.m.onboard(); ready.m.menuKind != "" {
		t.Fatalf("configured run opened %q", ready.m.menuKind)
	}
}

// A toggle made while a turn runs is applied; the turn is not interrupted.
func TestToggleWhileBusyChangesSetting(t *testing.T) {
	m, _, release := heldModel(t)
	defer release()
	m = openPickerCmd(t, m, "/settings")
	m = selectRow(t, m, "Reasoning summary")
	before := m.ctl.Settings().ReasoningSummary
	m = startBusy(t, m)
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if m.ctl.Settings().ReasoningSummary == before || !m.ctl.Status().Busy {
		t.Fatalf("setting unchanged (%v) or turn stopped", before)
	}
}

func TestStatusEstimatesContextOfResumedHistory(t *testing.T) {
	f := newFixture(t, true)
	f.run(t, strings.Repeat("long text ", 4000))
	f.command(t, "/new")
	sessions, _ := f.m.ctl.Sessions()
	f.command(t, "/resume "+sessions[len(sessions)-1].ID)
	if _, right := f.m.status(); !strings.Contains(right, "ctx ~") || strings.Contains(right, "ctx ~0K") {
		t.Fatalf("status right side = %q", right)
	}
}

// An open settings picker survives a notification that starts a turn: the toggle still applies.
func TestPickerToggleAppliesWhenNotificationStartsTurn(t *testing.T) {
	f := newFixture(t, true)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	defer close(hold)
	f.m = openPickerCmd(t, f.m, "/settings")
	if _, err := agent.RunTask(f.project, f.a.SessionID(), "echo background result"); err != nil {
		t.Fatal(err)
	}
	awaitRequest(t, f.m) // the notification starts a turn by itself
	f.m = settle(f.m)
	f.m = selectRow(t, f.m, "Reasoning summary")
	before := f.m.ctl.Settings().ReasoningSummary
	f.m = key(f.m, tea.KeyMsg{Type: tea.KeyRight})
	if f.m.ctl.Settings().ReasoningSummary == before || f.m.menuKind != pickerSettings {
		t.Fatalf("picker %q, setting %v", f.m.menuKind, f.m.ctl.Settings().ReasoningSummary)
	}
}

// An idle TUI starts a turn for an external notification at once, without any tick.
func TestNotificationStartsTurnWhenIdle(t *testing.T) {
	f := newFixture(t, true)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	defer close(hold)
	if _, err := agent.RunTask(f.project, f.a.SessionID(), "echo background result"); err != nil {
		t.Fatal(err)
	}
	awaitRequest(t, f.m)
}
