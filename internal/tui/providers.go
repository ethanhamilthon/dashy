package tui

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/agent"
)

const pickerProvider = "provider"

type providerForm struct {
	old        string
	fields     [3]string
	field      int
	welcome    bool
	checking   bool
	err        string
	failedWith [3]string
	generation int
}

var formLabels = [3]string{"Name", "Base URL", "API key"}

type providerChecked struct {
	generation int
	models     []string
	err        error
}

func (m *model) rebuildConnectMenu() {
	s := m.ctl.Settings()
	names := make([]string, 0, len(s.Providers))
	for name := range s.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	m.menu = nil
	for _, name := range names {
		label := fmt.Sprintf("%-16s %s", name, s.Providers[name].BaseURL)
		if name == s.Provider {
			label += "  · in use"
		}
		m.menu = append(m.menu, command{name: name, label: label})
	}
	m.menu = append(m.menu, command{name: "Add", icon: "+", label: "+ Add provider", action: actionAdd})
	m.selected = min(m.selected, len(m.menu)-1)
}

func (m model) connectKey(key tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	chosen := m.menu[m.selected%len(m.menu)]
	isProvider := chosen.action != actionAdd
	confirm := m.confirmDelete
	m.confirmDelete = ""
	switch key.String() {
	case "enter":
		if !isProvider {
			m.openProviderForm("", false)
			return m, nil, true
		}
		cmd := m.openPicker(pickerModels)
		m.modelsFor = chosen.name
		m.rebuildModelsMenu()
		m.refresh(true)
		return m, cmd, true
	case "e":
		if isProvider {
			m.openProviderForm(chosen.name, false)
		}
		return m, nil, true
	case "d":
		if !isProvider {
			return m, nil, true
		}
		if confirm != chosen.name {
			m.confirmDelete = chosen.name
			m.refresh(true)
			return m, nil, true
		}
		if err := m.ctl.DeleteProvider(chosen.name); err != nil {
			next, cmd := m.reject(err)
			return next, cmd, true
		}
		m.rebuildConnectMenu()
		m.refresh(true)
		return m, nil, true
	case "esc":
		if confirm != "" {
			m.refresh(true)
			return m, nil, true
		}
		return m, nil, false
	case "ctrl+c", "up", "down", "tab":
		return m, nil, false
	}
	return m, nil, true
}

func (m *model) openProviderForm(old string, welcome bool) {
	m.closePicker()
	m.menuKind = pickerProvider
	f := &providerForm{old: old, welcome: welcome}
	if p, ok := m.ctl.Settings().Providers[old]; ok && old != "" {
		f.fields = [3]string{old, p.BaseURL, ""}
	}
	m.form = f
	m.loadFormField()
	m.refresh(true)
}

func (m *model) loadFormField() {
	f := m.form
	m.input.Reset()
	m.input.SetValue(f.fields[f.field])
	m.input.CursorEnd()
	m.input.Placeholder = [3]string{"Provider name, e.g. openai", "Base URL, e.g. https://api.openai.com/v1", "API key (optional)"}[f.field]
	if f.field == 2 && f.old != "" {
		m.input.Placeholder = "API key (empty keeps the saved one)"
	}
}

func (m model) formKey(key tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	f := m.form
	f.fields[f.field] = m.input.Value()
	switch key.String() {
	case "esc", "ctrl+c":
		welcome := f.welcome
		m.closePicker()
		if !welcome {
			m.openPicker(pickerConnect)
		}
		m.refresh(true)
		return m, nil, true
	case "tab", "down":
		f.field = (f.field + 1) % len(f.fields)
	case "shift+tab", "up":
		f.field = (f.field + len(f.fields) - 1) % len(f.fields)
	case "enter":
		if f.checking {
			return m, nil, true
		}
		return m.submitProviderForm()
	default:
		return m, nil, false
	}
	m.loadFormField()
	m.refresh(true)
	return m, nil, true
}

func (m model) submitProviderForm() (tea.Model, tea.Cmd, bool) {
	f := m.form
	for i := range f.fields {
		f.fields[i] = strings.TrimSpace(f.fields[i])
	}
	missing := -1
	switch u, err := url.Parse(f.fields[1]); {
	case f.fields[0] == "":
		missing, f.err = 0, "Enter a name for the provider."
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		missing, f.err = 1, "The base URL must start with http:// or https://."
	}
	if missing >= 0 {
		f.field = missing
		m.loadFormField()
		m.refresh(true)
		return m, nil, true
	}
	if f.err != "" && f.failedWith == f.fields {
		return m.saveProviderForm(nil)
	}
	f.checking, f.err = true, ""
	f.generation++
	m.menuGeneration++
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	m.menuCancel = cancel
	generation, ctl, fields := f.generation, m.ctl, f.fields
	m.refresh(true)
	return m, func() tea.Msg {
		defer cancel()
		models, err := ctl.ProviderModels(ctx, f.old, fields[1], fields[2])
		return providerChecked{generation: generation, models: models, err: err}
	}, true
}

func (m model) providerResult(v providerChecked) (tea.Model, tea.Cmd) {
	f := m.form
	if f == nil || v.generation != f.generation {
		return m, nil
	}
	f.checking = false
	if v.err != nil {
		f.err, f.failedWith = "Could not connect: "+v.err.Error(), f.fields
		m.refresh(true)
		return m, nil
	}
	next, cmd, _ := m.saveProviderForm(v.models)
	return next, cmd
}

func (m model) saveProviderForm(models []string) (tea.Model, tea.Cmd, bool) {
	f := m.form
	if err := m.ctl.SaveProvider(f.old, f.fields[0], f.fields[1], f.fields[2]); err != nil {
		f.err = err.Error()
		m.refresh(true)
		return m, nil, true
	}
	name := f.fields[0]
	m.closePicker()
	cmd := m.openPicker(pickerModels)
	m.modelsFor = name
	if models != nil {
		if m.menuCancel != nil {
			m.menuCancel()
			m.menuCancel, cmd = nil, nil
		}
		for _, id := range models {
			m.pickerModels = append(m.pickerModels, agent.ModelChoice{Provider: name, Model: id})
		}
		if len(models) == 0 {
			m.setFlash("notice", "No models returned by providers.")
		}
	}
	m.rebuildModelsMenu()
	m.refresh(true)
	return m, cmd, true
}

func (m model) formRows() (rows []string, errRow string) {
	f := m.form
	switch {
	case f.welcome:
		rows = append(rows, "Welcome to dashy. Connect an OpenAI-compatible provider to start.")
	case f.old != "":
		rows = append(rows, "Edit provider "+f.old)
	default:
		rows = append(rows, "Add provider")
	}
	stored := m.ctl.Settings().Providers[f.old]
	for i, label := range formLabels {
		value := f.fields[i]
		if i == f.field {
			value = m.input.Value()
		}
		if i == 2 {
			switch {
			case value != "":
				value = strings.Repeat("•", min(len([]rune(value)), 24))
			case f.old != "" && stored.APIKeyEnv != "":
				value = "from $" + stored.APIKeyEnv
			case f.old != "" && stored.APIKey != "":
				value = "saved"
			default:
				value = "optional"
			}
		}
		marker := "  "
		if i == f.field {
			marker = "› "
		}
		rows = append(rows, fmt.Sprintf("%s%-9s %s", marker, label, value))
	}
	if f.checking {
		rows = append(rows, "Checking the connection…")
	} else {
		errRow = f.err
	}
	return rows, errRow
}

func (f *providerForm) failedAsTyped(active string) bool {
	fields := f.fields
	fields[f.field] = strings.TrimSpace(active)
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	return f.err != "" && fields == f.failedWith
}

func (m *model) onboard() tea.Cmd {
	s := m.ctl.Settings()
	switch {
	case len(s.Providers) == 0:
		m.openProviderForm("", true)
	case s.Model == "":
		cmd := m.openPicker(pickerModels)
		m.refresh(true)
		return cmd
	}
	return nil
}
