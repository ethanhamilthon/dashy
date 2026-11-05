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

// First run: no provider yet. /connect offers "Add provider", whose three prompts
// create the provider in the settings file.
func TestFirstRunSetupThroughConnect(t *testing.T) {
	f := newFixture(t, false)
	f.command(t, "/connect")
	idx := -1
	for i, c := range f.m.menu {
		if c.name == "Add" {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no Add provider row: %v", menuNames(f.m))
	}
	f.m.selected = idx
	f.key(tea.KeyMsg{Type: tea.KeyEnter})
	if f.m.menuKind != pickerSetup {
		t.Fatalf("Add provider opened %q", f.m.menuKind)
	}
	for _, answer := range []string{"work", "http://127.0.0.1:9", ""} {
		f.m.input.SetValue(answer)
		f.key(tea.KeyMsg{Type: tea.KeyEnter})
	}
	if f.m.menuKind != "" {
		t.Fatalf("picker still open: %q", f.m.menuKind)
	}
	if _, ok := f.m.ctl.Settings().Providers["work"]; !ok {
		t.Fatal("provider was not added")
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

// The terminal is told about turns and session switches: a finished answer is done,
// /new returns to idle, a failed turn is error and resuming returns to idle.
func TestTerminalStatusFollowsTurnsAndSessionSwitches(t *testing.T) {
	f := newFixture(t, true)
	f.run(t, "first question")
	if msg := awaitReport(t, f, "done"); msg != "answer" {
		t.Fatalf("done message %q", msg)
	}
	f.command(t, "/new")
	if msg := awaitReport(t, f, "idle"); msg != "" {
		t.Fatalf("idle message after /new %q", msg)
	}
	f.fail.Store(true)
	f.run(t, "second question")
	awaitReport(t, f, "error")
	f.fail.Store(false)
	sessions, _ := f.m.ctl.Sessions()
	if len(sessions) == 0 {
		t.Fatal("no session to resume")
	}
	f.command(t, "/resume "+sessions[len(sessions)-1].ID)
	awaitReport(t, f, "idle")
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

// The terminal title is the session title: the first line of the first message, then
// the rename, and the first line again after a rename to nothing.
func TestTerminalTitleFollowsFirstLineAndRename(t *testing.T) {
	f := newFixture(t, true)
	f.run(t, "  first line\nsecond line")
	awaitTitle(t, f, "first line")
	f.command(t, "/rename Plan")
	awaitTitle(t, f, "Plan")
	f.command(t, "/rename ")
	awaitTitle(t, f, "first line")
}

// An open settings picker survives a notification that starts a turn: the toggle still applies.
func TestPickerToggleAppliesWhenNotificationStartsTurn(t *testing.T) {
	f := newFixture(t, true)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	defer close(hold)
	f.m = openPickerCmd(t, f.m, "/settings")
	if err := agent.Notify(f.project, f.a.SessionID(), "background result"); err != nil {
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
	if err := agent.Notify(f.project, f.a.SessionID(), "background result"); err != nil {
		t.Fatal(err)
	}
	awaitRequest(t, f.m)
}
