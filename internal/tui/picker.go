package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dashy/internal/agent"
	"dashy/internal/control"
	tea "github.com/charmbracelet/bubbletea"
)

// modelChoices carries the result of listing models for the /model picker.
type modelChoices struct {
	generation int
	models     []control.ModelInfo
	errs       map[string]string
	err        error
}

func (m *model) openPicker(kind string) tea.Cmd {
	m.pasteGeneration++
	m.pasting = false
	if m.menuCancel != nil {
		m.menuCancel()
		m.menuCancel = nil
	}
	m.menuKind, m.selected = kind, 0
	m.menu = nil
	m.input.Reset()
	m.menuGeneration++
	if kind == "login" {
		settings := m.ctl.Settings()
		for name := range settings.Providers {
			m.menu = append(m.menu, command{name: name, description: "Provider"})
		}
		sort.Slice(m.menu, func(i, j int) bool { return m.menu[i].name < m.menu[j].name })
		m.menu = append(m.menu, command{name: "Add", icon: "+", description: "Create provider"})
		if dest := m.ctl.PrimaryConfig(); dest != "" {
			m.add("notice", fmt.Sprintf("Credentials destination: %s", dest))
		}
		return nil
	}
	m.effort = 0
	for i, e := range agent.Efforts {
		if e == m.ctl.Settings().Effort {
			m.effort = i
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.menuCancel = cancel
	generation, ctl := m.menuGeneration, m.ctl
	return func() tea.Msg {
		list, err := ctl.Models(ctx)
		if err != nil {
			return modelChoices{generation: generation, err: err}
		}
		return modelChoices{generation: generation, models: list.Models, errs: list.Errors}
	}
}

func (m *model) closePicker() {
	m.pasteGeneration++
	m.pasting = false
	if m.menuCancel != nil {
		m.menuCancel()
		m.menuCancel = nil
	}
	m.menuKind, m.menu = "", nil
	m.setup = nil
	m.input.Placeholder = "Ask anything, or type / for commands"
	m.input.Reset()
}

func (m model) pickerKey(key tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.menuKind == "" {
		return m, nil, false
	}
	switch key.String() {
	case "esc", "ctrl+c":
		m.closePicker()
	case "left", "right":
		if m.menuKind != "model" {
			return m, nil, false
		}
		delta := 1
		if key.String() == "left" {
			delta = -1
		}
		m.effort = (m.effort + delta + len(agent.Efforts)) % len(agent.Efforts)
	case "up", "down", "tab":
		matches := m.completions()
		if len(matches) > 0 {
			delta := 1
			if key.String() == "up" {
				delta = -1
			}
			m.selected = (m.selected + delta + len(matches)) % len(matches)
		}
	case "enter":
		if m.pasting {
			return m, nil, true
		}
		if m.menuKind == "setup" {
			return m.finishSetup()
		}
		matches := m.completions()
		if len(matches) == 0 {
			return m, nil, true
		}
		choice, kind := matches[m.selected%len(matches)].name, m.menuKind
		if kind == "login" && matches[m.selected%len(matches)].icon == "+" {
			m.menuKind = "setup"
			m.menu = nil
			m.selected = 0
			m.input.Reset()
			m.input.Placeholder = "Provider name"
			if dest := m.ctl.PrimaryConfig(); dest != "" {
				m.add("notice", fmt.Sprintf("Credentials will be saved to %s", dest))
			}
			break
		}
		var err error
		if kind == "model" {
			provider := matches[m.selected%len(matches)].description
			err = m.ctl.SelectModel(provider, choice, agent.Efforts[m.effort])
		} else {
			err = m.ctl.SelectProvider(choice)
		}
		m.closePicker()
		if err != nil {
			next, cmd := m.reject(err)
			return next, cmd, true
		}
	default:
		return m, nil, false
	}
	m.refresh(true)
	return m, nil, true
}

// finishSetup collects the provider setup prompts and adds the provider. The
// API key is optional, so an empty third step is valid.
func (m model) finishSetup() (tea.Model, tea.Cmd, bool) {
	value := strings.TrimSpace(m.input.Value())
	if value == "" && len(m.setup) != 2 {
		return m, nil, true
	}
	m.setup = append(m.setup, value)
	m.input.Reset()
	m.selected = 0
	m.menu = nil
	prompts := []string{"Provider name", "HTTP(S) base URL", "API key (optional)"}
	if len(m.setup) < 3 {
		m.input.Placeholder = prompts[len(m.setup)]
		if len(m.setup) == 2 {
			if dest := m.ctl.PrimaryConfig(); m.ctl.ProjectConfig(dest) {
				m.add("notice", fmt.Sprintf("Warning: API key will be saved to project configuration (%s); avoid committing secrets to version control.", dest))
			}
		}
		m.refresh(true)
		return m, nil, true
	}
	dest := m.ctl.PrimaryConfig()
	if m.setup[2] != "" && m.ctl.ProjectConfig(dest) {
		m.add("notice", fmt.Sprintf("Warning: API key saved in project configuration (%s); avoid committing secrets.", dest))
	}
	err := m.ctl.AddProvider(m.setup[0], m.setup[1], m.setup[2])
	m.closePicker()
	m.refresh(true)
	if err != nil {
		next, cmd := m.reject(err)
		return next, cmd, true
	}
	return m, nil, true
}
