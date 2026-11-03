package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

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

// /settings lists the settings rows and the models of every provider; toggles,
// effort and a chosen model are applied and saved at once.
func TestSettingsPickerChangesSettingsAndSelectsModel(t *testing.T) {
	m, a := freshModel(t)
	if err := a.AddProvider("p", agent.Provider{BaseURL: modelServer(t, `{"data":[{"id":"m1"},{"id":"m2"}]}`)}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddProvider("q", agent.Provider{BaseURL: modelServer(t, `{"data":[{"id":"m3"}]}`)}); err != nil {
		t.Fatal(err)
	}
	m = openPickerCmd(t, m, "/settings")
	m = selectRow(t, m, "Auto compact")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if a.Settings().AutoCompact {
		t.Fatal("auto compact not toggled off")
	}
	m = selectRow(t, m, "Show lines")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if a.Settings().ShowLines != "medium" {
		t.Fatalf("show lines %q", a.Settings().ShowLines)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyRight}) // effort: "" -> none
	m = key(m, tea.KeyMsg{Type: tea.KeyRight}) // none -> minimal
	m = selectRow(t, m, "m3")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menuKind != "" || a.Settings().Provider != "q" || a.Settings().Model != "m3" || a.Settings().Effort != "minimal" {
		t.Fatalf("model selection: picker %q, settings %+v", m.menuKind, a.Settings())
	}
}

// /login lists providers and adds one through the setup prompts; the key is never shown.
func TestLoginAddsProviderWithoutShowingKey(t *testing.T) {
	m, a := freshModel(t)
	m = openPickerCmd(t, m, "/login")
	m = selectRow(t, m, "Add")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	for _, value := range []string{"corp", "https://llm.example.com/v1", "sk-secret-key"} {
		m.input.SetValue(value)
		m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	}
	p, ok := a.Settings().Providers["corp"]
	if !ok || p.APIKey != "sk-secret-key" || p.BaseURL != "https://llm.example.com/v1" || m.menuKind != "" {
		t.Fatalf("provider %+v (found %v), picker %q", p, ok, m.menuKind)
	}
	for _, e := range m.entries {
		if strings.Contains(e.Text, "sk-secret-key") {
			t.Fatalf("API key shown in the timeline: %q", e.Text)
		}
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
		if err := a.AddProvider(name, agent.Provider{BaseURL: url}); err != nil {
			t.Fatal(err)
		}
	}
	m = openPickerCmd(t, m, "/login")
	m = selectRow(t, m, "Add")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if a.Settings().Provider != "Add" || m.menuKind != "" {
		t.Fatalf("provider row named Add: provider %q, picker %q", a.Settings().Provider, m.menuKind)
	}
	m = openPickerCmd(t, m, "/settings")
	before := a.Settings()
	m = selectModel(t, m, "Reasoning summary")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if s := a.Settings(); s.Model != "Reasoning summary" || s.ReasoningSummary != before.ReasoningSummary {
		t.Fatalf("model named like a setting: model %q, reasoning summary %v", s.Model, s.ReasoningSummary)
	}
	m = openPickerCmd(t, m, "/settings")
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
	if err := a.AddProvider("p", agent.Provider{BaseURL: modelServer(t, `{"data":[{"id":"m1"}]}`)}); err != nil {
		t.Fatal(err)
	}
	if err := a.Select("p", "m1", ""); err != nil {
		t.Fatal(err)
	}
	m = openPickerCmd(t, m, "/settings")
	m = key(m, tea.KeyMsg{Type: tea.KeyRight}) // effort: "" -> none
	m = key(m, tea.KeyMsg{Type: tea.KeyEsc})
	saved, err := agent.LoadSettings(a.SettingsPath())
	if m.menuKind != "" || a.Settings().Effort != "none" || err != nil || saved.Effort != "none" {
		t.Fatalf("picker %q, effective effort %q, saved effort %q (%v)", m.menuKind, a.Settings().Effort, saved.Effort, err)
	}
}
