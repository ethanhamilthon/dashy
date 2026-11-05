package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"dashy/internal/agent"
)

// openPickerCmd runs a slash command that opens a picker and applies the list it fetches.
func openPickerCmd(t *testing.T, m model, name string) model {
	t.Helper()
	next, cmd := m.runCommand(name)
	m = next.(model)
	if cmd != nil {
		next, _ = m.Update(cmd())
		m = next.(model)
	}
	return m
}

// selectRow moves the picker selection to the row called name.
func selectRow(t *testing.T, m model, name string) model {
	t.Helper()
	for i, c := range m.completions() {
		if c.name == name {
			m.selected = i
			return m
		}
	}
	t.Fatalf("no picker row %q in %+v", name, m.completions())
	return m
}

// modelServer answers every request with list, as a provider's /models does.
func modelServer(t *testing.T, list string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, list)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// /settings toggles display options; /models lists the models of every provider and
// applies and saves the effort and a chosen model at once.
func TestSettingsAndModelsPickers(t *testing.T) {
	m, a := freshModel(t)
	if err := a.SaveProvider("", "p", agent.Provider{BaseURL: modelServer(t, `{"data":[{"id":"m1"},{"id":"m2"}]}`)}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveProvider("", "q", agent.Provider{BaseURL: modelServer(t, `{"data":[{"id":"m3"}]}`)}); err != nil {
		t.Fatal(err)
	}
	m = openPickerCmd(t, m, "/settings")
	m = selectRow(t, m, "Reasoning summary")
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if a.Settings().ReasoningSummary {
		t.Fatal("reasoning summary not turned off")
	}
	if view := ansi.Strip(m.View()); !strings.Contains(view, "[off]") || !strings.Contains(view, " on ") {
		t.Fatalf("both options must be listed: %q", view)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyLeft})
	if !a.Settings().ReasoningSummary {
		t.Fatal("reasoning summary not turned back on")
	}
	m = selectRow(t, m, "Show lines")
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if a.Settings().ShowLines != "full" {
		t.Fatalf("show lines %q", a.Settings().ShowLines)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRight})
	if a.Settings().ShowLines != "full" {
		t.Fatalf("show lines passed its last option: %q", a.Settings().ShowLines)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m = openPickerCmd(t, m, "/models")
	m = key(m, tea.KeyMsg{Type: tea.KeyRight}) // effort: "" -> none
	m = key(m, tea.KeyMsg{Type: tea.KeyRight}) // none -> minimal
	m = selectRow(t, m, "m3")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menuKind != "" || a.Settings().Provider != "q" || a.Settings().Model != "m3" || a.Settings().Effort != "minimal" {
		t.Fatalf("model selection: picker %q, settings %+v", m.menuKind, a.Settings())
	}
}

// keyRun sends a key and applies the message its command returns (a connection test
// or a models listing), as the program would.
func keyRun(t *testing.T, m model, k tea.KeyMsg) model {
	t.Helper()
	next, cmd := m.Update(k)
	m = settle(next.(model))
	if cmd != nil {
		if msg := cmd(); msg != nil {
			if _, batch := msg.(tea.BatchMsg); !batch {
				next, _ = m.Update(msg)
				m = settle(next.(model))
			}
		}
	}
	return m
}

// typeField replaces the active field of the provider form.
func typeField(m model, value string) model {
	m.input.SetValue(value)
	return m
}

// /connect adds a provider through one form: the connection test lists its models,
// which open next. The key is never shown.
func TestConnectAddsProviderWithoutShowingKey(t *testing.T) {
	m, a := freshModel(t)
	url := modelServer(t, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
	m = openPickerCmd(t, m, "/connect")
	m = selectRow(t, m, "Add")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menuKind != pickerProvider || m.form == nil || m.form.old != "" {
		t.Fatalf("Add provider opened %q", m.menuKind)
	}
	m = typeField(m, "corp")
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	m = typeField(m, url)
	m = key(m, tea.KeyMsg{Type: tea.KeyTab})
	m = typeField(m, "sk-secret-key")
	if view := ansi.Strip(m.View()); strings.Contains(view, "sk-secret-key") || !strings.Contains(view, "Name      corp") || !strings.Contains(view, "Tab: next field") {
		t.Fatalf("form view:\n%s", view)
	}
	m = keyRun(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	p, ok := a.Settings().Providers["corp"]
	if !ok || p.APIKey != "sk-secret-key" || p.BaseURL != url {
		t.Fatalf("provider %+v (found %v)", p, ok)
	}
	if m.menuKind != pickerModels || m.modelsFor != "corp" || strings.Join(menuNames(m), "|") != "m1|m2" {
		t.Fatalf("after save: picker %q for %q, rows %v", m.menuKind, m.modelsFor, menuNames(m))
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if s := a.Settings(); s.Provider != "corp" || s.Model != "m1" {
		t.Fatalf("selection %+v", s)
	}
	for _, e := range m.entries {
		if strings.Contains(e.Text, "sk-secret-key") {
			t.Fatalf("API key shown in the timeline: %q", e.Text)
		}
	}
}

// A missing field takes the focus; a failed connection test stays in the form and
// Enter again saves anyway.
func TestProviderFormValidatesAndSavesAnyway(t *testing.T) {
	m, a := freshModel(t)
	m.openProviderForm("", true)
	if view := ansi.Strip(m.View()); !strings.Contains(view, "Welcome to dashy") || !strings.Contains(view, "Esc: skip") {
		t.Fatalf("welcome view:\n%s", view)
	}
	m = typeField(m, "work")
	m = keyRun(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.form.field != 1 || !strings.Contains(ansi.Strip(m.View()), "must start with http") {
		t.Fatalf("missing URL: field %d\n%s", m.form.field, ansi.Strip(m.View()))
	}
	m = typeField(m, "http://127.0.0.1:9")
	m = keyRun(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := a.Settings().Providers["work"]; ok || !strings.Contains(ansi.Strip(m.View()), "Could not connect") || !strings.Contains(ansi.Strip(m.View()), "save anyway") {
		t.Fatalf("failed test saved or not shown:\n%s", ansi.Strip(m.View()))
	}
	m = keyRun(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if _, ok := a.Settings().Providers["work"]; !ok || m.menuKind != pickerModels {
		t.Fatalf("not saved anyway: picker %q", m.menuKind)
	}
}

// e edits a provider (a rename keeps the key when none is typed); d twice deletes it.
func TestConnectEditsAndDeletesProvider(t *testing.T) {
	m, a := freshModel(t)
	url := modelServer(t, `{"data":[{"id":"m1"}]}`)
	if err := a.SaveProvider("", "old", agent.Provider{BaseURL: url, APIKey: "k1"}); err != nil {
		t.Fatal(err)
	}
	m = openPickerCmd(t, m, "/connect")
	m = selectRow(t, m, "old")
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if m.menuKind != pickerProvider || m.form.old != "old" || m.input.Value() != "old" || !strings.Contains(ansi.Strip(m.View()), "API key   saved") {
		t.Fatalf("edit form: %q\n%s", m.menuKind, ansi.Strip(m.View()))
	}
	m = typeField(m, "new")
	m = keyRun(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if p, ok := a.Settings().Providers["new"]; !ok || p.APIKey != "k1" || len(a.Settings().Providers) != 1 {
		t.Fatalf("after edit: %+v", a.Settings().Providers)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	m = openPickerCmd(t, m, "/connect")
	m = selectRow(t, m, "new")
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if _, ok := a.Settings().Providers["new"]; !ok || !strings.Contains(ansi.Strip(m.View()), "Delete new?") {
		t.Fatal("first d deleted or asked nothing")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if _, ok := a.Settings().Providers["new"]; ok || m.menuKind != pickerConnect {
		t.Fatalf("not deleted: %v, picker %q", a.Settings().Providers, m.menuKind)
	}
}

// selectModel moves the picker selection to the model row called name (not an action row).
func selectModel(t *testing.T, m model, name string) model {
	t.Helper()
	for i, c := range m.completions() {
		if c.name == name && c.action == "" {
			m.selected = i
			return m
		}
	}
	t.Fatalf("no model row %q", name)
	return m
}

// Provider and model names equal to action labels are ordinary choices: they never
// open the provider setup and never change a setting.
func TestPickerNamesThatMatchActionsAreChoices(t *testing.T) {
	m, a := freshModel(t)
	url := modelServer(t, `{"data":[{"id":"Reasoning summary"},{"id":"Show lines"}]}`)
	for _, name := range []string{"Add", "Add provider"} {
		if err := a.SaveProvider("", name, agent.Provider{BaseURL: url}); err != nil {
			t.Fatal(err)
		}
	}
	m = openPickerCmd(t, m, "/connect")
	m = selectRow(t, m, "Add")
	m = keyRun(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menuKind != pickerModels || m.modelsFor != "Add" {
		t.Fatalf("provider row named Add opened %q for %q", m.menuKind, m.modelsFor)
	}
	before := a.Settings()
	m = selectModel(t, m, "Reasoning summary")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if s := a.Settings(); s.Model != "Reasoning summary" || s.ReasoningSummary != before.ReasoningSummary {
		t.Fatalf("model named like a setting: model %q, reasoning summary %v", s.Model, s.ReasoningSummary)
	}
	m = openPickerCmd(t, m, "/models")
	before = a.Settings()
	m = selectModel(t, m, "Show lines")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if s := a.Settings(); s.Model != "Show lines" || s.ShowLines != before.ShowLines {
		t.Fatalf("model named Show lines: model %q, show lines %q", s.Model, s.ShowLines)
	}
}

// With a configured model, Left/Right saves the effort at once: Escape keeps it.
func TestEffortChangeIsSavedBeforeEscape(t *testing.T) {
	m, a := freshModel(t)
	if err := a.SaveProvider("", "p", agent.Provider{BaseURL: modelServer(t, `{"data":[{"id":"m1"}]}`)}); err != nil {
		t.Fatal(err)
	}
	if err := a.Select("p", "m1", ""); err != nil {
		t.Fatal(err)
	}
	m = openPickerCmd(t, m, "/models")
	m = key(m, tea.KeyMsg{Type: tea.KeyRight}) // effort: "" -> none
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	saved, err := agent.LoadSettings(filepath.Dir(a.DBPath()))
	if m.menuKind != "" || a.Settings().Effort != "none" || err != nil || saved.Effort != "none" {
		t.Fatalf("picker %q, effective effort %q, saved effort %q (%v)", m.menuKind, a.Settings().Effort, saved.Effort, err)
	}
}

// The /connect menu shows its add row once, as "+ Add provider".
func TestConnectMenuAddRowRendersOnce(t *testing.T) {
	m, _ := freshModel(t)
	view := ansi.Strip(openPickerCmd(t, m, "/connect").View())
	if !strings.Contains(view, "+ Add provider") || strings.Contains(view, "+ +") {
		t.Fatalf("add row: %q", view)
	}
}

// /tasks lists the session's tasks newest first; k stops the selected running one,
// and Enter shows its output in the timeline.
func TestTasksPickerStopsAndShows(t *testing.T) {
	m, a := freshModel(t)
	if next, _ := m.runCommand("/tasks"); next.(model).menuKind != "" {
		t.Fatal("empty task list opened")
	}
	for _, command := range []string{"echo first-output", "sleep 30"} {
		if _, err := agent.RunTask(a.Shell.Dir, a.SessionID(), command); err != nil {
			t.Fatal(err)
		}
	}
	m = openPickerCmd(t, m, "/tasks")
	rows := m.completions()
	if m.menuKind != pickerTasks || len(rows) != 2 || rows[0].name != "t2" || rows[0].description != "running" {
		t.Fatalf("rows %+v", rows)
	}
	if view := ansi.Strip(m.View()); !strings.Contains(view, "k: stop") || !strings.Contains(view, "$ sleep 30") {
		t.Fatalf("view:\n%s", view)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	for deadline := time.Now().Add(5 * time.Second); m.completions()[0].description == "running"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("task not stopped")
		}
		m.rebuildTasksMenu()
	}
	if got := m.completions()[0].description; got != "stopped" {
		t.Fatalf("status %q", got)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyDown})
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menuKind != "" || !strings.Contains(ansi.Strip(m.timeline()), "first-output") {
		t.Fatalf("output not shown:\n%s", ansi.Strip(m.timeline()))
	}
}
