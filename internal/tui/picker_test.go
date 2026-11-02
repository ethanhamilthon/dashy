package tui

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dashy/internal/agent"
	"dashy/internal/control"
	tea "github.com/charmbracelet/bubbletea"
)

func pickerModel(t *testing.T) (model, *agent.Agent) {
	t.Helper()
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return newModel(a, a.Shell.Dir), a
}
func enterPicker(m model) model {
	next, _, _ := m.pickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	return settle(next.(model))
}

func TestModelPickerFetchAndEffortKeys(t *testing.T) {
	m, a := pickerModel(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"first"},{"id":"second"}]}`)
	}))
	defer server.Close()
	if err := a.AddProvider("test", agent.Provider{BaseURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := a.Select("test", "original", "low"); err != nil {
		t.Fatal(err)
	}
	cmd := m.openPicker("model")
	next, _ := m.Update(cmd())
	m = next.(model)
	if len(m.completions()) != 2 {
		t.Fatal(m.completions())
	}
	next, _, _ = m.pickerKey(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if agent.Efforts[m.effort] != "medium" {
		t.Fatal("right did not change effort")
	}
	next, _, _ = m.pickerKey(tea.KeyMsg{Type: tea.KeyDown})
	m = enterPicker(next.(model))
	if m.menuKind != "" || a.Settings().Model != "second" || a.Settings().Effort != "medium" {
		t.Fatal("model selection not applied")
	}
}

func TestProviderWizardMasksKeyAndDoesNotSaveDraftToHistory(t *testing.T) {
	m, a := pickerModel(t)
	m.openPicker("login")
	if last := m.menu[len(m.menu)-1]; last.name != "Add" || last.icon != "+" {
		t.Fatal("missing final Add row")
	}
	m.selected = len(m.menu) - 1
	m = enterPicker(m)
	for _, value := range []string{"custom", "https://example.com/v1", "secret-key"} {
		m.input.SetValue(value)
		m.refresh(true)
		if len(m.setup) == 2 && strings.Contains(m.View(), "secret-key") {
			t.Fatal("credential visible")
		}
		m = enterPicker(m)
	}
	p, ok := a.Settings().Providers["custom"]
	if !ok || p.APIKey != "secret-key" {
		t.Fatalf("provider not saved: %+v", p)
	}
	if len(a.History) != 0 || strings.Contains(fmt.Sprint(m.entries), "secret-key") {
		t.Fatal("credentials leaked into history")
	}
}

func TestCancelledPickerIgnoresLateModelsAndPreservesSession(t *testing.T) {
	m, a, release := heldModel(t, "")
	m = startBusy(t, m)
	defer release()
	previous := a.Settings().Model
	m.menuKind = "model"
	m.menuGeneration = 2
	next, _, _ := m.pickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	next, _ = m.Update(modelChoices{generation: 2, models: []control.ModelInfo{{Provider: "x", Model: "late"}}})
	m = next.(model)
	if m.menuKind != "" || len(m.menu) != 0 || a.Settings().Model != previous {
		t.Fatal("late response revived cancelled picker")
	}
	next, cmd := m.runCommand("/settings")
	m = next.(model)
	if cmd != nil || m.menuKind != "" {
		t.Fatal("model changed during active turn")
	}
}

func TestModelPickerListsAllProvidersAndSelectsItsProvider(t *testing.T) {
	m, a := pickerModel(t)
	serve := func(models string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, models)
		}))
	}
	one, two := serve(`{"data":[{"id":"alpha"}]}`), serve(`{"data":[{"id":"beta"}]}`)
	defer one.Close()
	defer two.Close()
	for name, server := range map[string]*httptest.Server{"one": one, "two": two} {
		if err := a.AddProvider(name, agent.Provider{BaseURL: server.URL}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Select("one", "alpha", "low"); err != nil {
		t.Fatal(err)
	}
	cmd := m.openPicker("model")
	next, _ := m.Update(cmd())
	m = next.(model)
	matches := m.completions()
	found := map[string]string{}
	for _, c := range matches {
		found[c.name] = c.description
	}
	if found["alpha"] != "one" || found["beta"] != "two" {
		t.Fatalf("models from all providers expected, got %v", found)
	}
	for i, c := range matches {
		if c.name == "beta" {
			m.selected = i
		}
	}
	m = enterPicker(m)
	if s := a.Settings(); s.Provider != "two" || s.Model != "beta" {
		t.Fatalf("provider not switched with model: %+v", s)
	}
}

func TestClosingModelPickerCancelsModelListing(t *testing.T) {
	m, a := pickerModel(t)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var startOnce, cancelOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			cancelOnce.Do(func() { close(cancelled) })
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	if err := a.AddProvider("test", agent.Provider{BaseURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	cmd := m.openPicker("model")
	go func() { _ = cmd() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model listing did not reach the provider")
	}
	next, _, _ := m.pickerKey(tea.KeyMsg{Type: tea.KeyEsc})
	if next.(model).menuKind != "" {
		t.Fatal("esc did not close the picker")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the picker left the model listing running")
	}
}

func TestSettingsPickerTogglesAndModelSelection(t *testing.T) {
	m, a := pickerModel(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"}]}`)
	}))
	defer server.Close()
	if err := a.AddProvider("testprov", agent.Provider{BaseURL: server.URL}); err != nil {
		t.Fatal(err)
	}
	cmd := m.openPicker("settings")
	next, _ := m.Update(cmd())
	m = next.(model)

	// 1. Toggle Reasoning summary (item 0)
	m.selected = 0
	m = enterPicker(m)
	if a.Settings().ReasoningSummary != false {
		t.Fatalf("expected reasoning_summary false, got %v", a.Settings().ReasoningSummary)
	}

	// 2. Toggle Auto compact (item 1)
	m.selected = 1
	m = enterPicker(m)
	if a.Settings().AutoCompact != false {
		t.Fatalf("expected auto_compact false, got %v", a.Settings().AutoCompact)
	}

	// 3. Cycle Show lines (item 2)
	m.selected = 2
	m = enterPicker(m)
	if a.Settings().ShowLines != "medium" {
		t.Fatalf("expected show_lines medium, got %v", a.Settings().ShowLines)
	}

	// 4. Change effort and select model (item 4 is gpt-4o)
	next, _, _ = m.pickerKey(tea.KeyMsg{Type: tea.KeyRight})
	m = next.(model)
	if agent.Efforts[m.effort] != "minimal" {
		t.Fatalf("expected minimal, got %v", agent.Efforts[m.effort])
	}
	m.selected = 4
	m = enterPicker(m)
	if m.menuKind != "" {
		t.Fatal("expected picker closed after model selection")
	}
	if a.Settings().Model != "gpt-4o" || a.Settings().Provider != "testprov" || a.Settings().Effort != "minimal" {
		t.Fatalf("model selection mismatch: %+v", a.Settings())
	}
}
