package tui

import (
	"context"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/agent"
	"dashy/internal/control"
)

// Picker kinds. "settings" lists the settings rows and every model; "login" lists
// providers, the provider part of /settings.
const (
	pickerSettings = "settings"
	pickerLogin    = "login"
	pickerSetup    = "setup" // provider prompts after "Add provider"
)

// Action rows of the pickers. Provider and model rows have none, so their names can be anything.
const (
	actionAdd         = "add"
	actionReasoning   = "reasoning"
	actionAutoCompact = "autocompact"
	actionLines       = "lines"
)

// modelChoices carries the result of listing models for the settings picker.
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
	if kind == pickerLogin {
		for name := range m.ctl.Settings().Providers {
			m.menu = append(m.menu, command{name: name, description: "Provider"})
		}
		sort.Slice(m.menu, func(i, j int) bool { return m.menu[i].name < m.menu[j].name })
		m.menu = append(m.menu, command{name: "Add", icon: "+", description: "Create provider", label: "+ Add provider", action: actionAdd})
		return nil
	}
	m.effort = slices.Index(agent.Efforts, m.ctl.Settings().Effort)
	m.pickerModels = nil
	m.rebuildSettingsMenu()
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

// rebuildSettingsMenu lists the settings rows followed by the models known so far.
func (m *model) rebuildSettingsMenu() {
	s := m.ctl.Settings()
	m.menu = []command{
		{name: "Reasoning summary", icon: "*", label: "Reasoning summary: " + onOff(s.ReasoningSummary), action: actionReasoning},
		{name: "Auto compact", icon: "*", label: "Auto compact: " + onOff(s.AutoCompact), action: actionAutoCompact},
		{name: "Show lines", icon: "*", label: "Show lines: " + s.ShowLines, action: actionLines},
		{name: "Add provider", icon: "+", description: "Create provider", label: "+ Add provider", action: actionAdd},
	}
	for _, c := range m.pickerModels {
		m.menu = append(m.menu, command{name: c.Model, description: c.Provider, label: c.Model + "  " + c.Provider})
		if c.Provider == s.Provider && c.Model == s.Model {
			m.selected = len(m.menu) - 1
		}
	}
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func (m *model) closePicker() {
	m.pasteGeneration++
	m.pasting = false
	if m.menuCancel != nil {
		m.menuCancel()
		m.menuCancel = nil
	}
	m.menuKind, m.menu = "", nil
	m.pickerModels = nil
	m.setup = nil
	m.input.Placeholder = "Ask anything, or type / for commands"
	m.input.Reset()
}

// pickerKey handles keys while a picker is open. Rejected operations keep the
// picker open and show the reason.
func (m model) pickerKey(key tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.menuKind == "" {
		return m, nil, false
	}
	switch key.String() {
	case "esc", "ctrl+c":
		m.closePicker()
	case "left", "right":
		if m.menuKind != pickerSettings {
			return m, nil, false
		}
		delta := map[string]int{"left": -1, "right": 1}[key.String()]
		effort := (m.effort + delta + len(agent.Efforts)) % len(agent.Efforts)
		if s := m.ctl.Settings(); s.Provider != "" && s.Model != "" {
			// A configured model's effort is applied and saved at once; a rejected change keeps the saved value.
			if err := m.ctl.SelectModel(s.Provider, s.Model, agent.Efforts[effort]); err != nil {
				next, cmd := m.reject(err)
				return next, cmd, true
			}
		}
		m.effort = effort
	case "up", "down", "tab":
		if matches := m.completions(); len(matches) > 0 {
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
		if m.menuKind == pickerSetup {
			return m.finishSetup()
		}
		return m.pickerEnter()
	default:
		return m, nil, false
	}
	m.refresh(true)
	return m, nil, true
}

// pickerEnter applies the selected row. Toggles keep the picker open; a rejected
// toggle shows its reason. A chosen model or provider closes the picker.
func (m model) pickerEnter() (tea.Model, tea.Cmd, bool) {
	matches := m.completions()
	if len(matches) == 0 {
		return m, nil, true
	}
	chosen := matches[m.selected%len(matches)]
	if chosen.action == actionAdd {
		m.menuKind = pickerSetup
		m.menu = nil
		m.selected = 0
		m.input.Reset()
		m.input.Placeholder = "Provider name"
		m.refresh(true)
		return m, nil, true
	}
	var toggle error
	switch chosen.action {
	case actionReasoning:
		toggle = m.ctl.ToggleReasoningSummary()
	case actionAutoCompact:
		toggle = m.ctl.ToggleAutoCompact()
	case actionLines:
		toggle = m.ctl.CycleShowLines()
	}
	if chosen.action != "" {
		m.rebuildSettingsMenu()
		m.refresh(true)
		if toggle != nil {
			next, cmd := m.reject(toggle)
			return next, cmd, true
		}
		return m, nil, true
	}
	var err error
	if m.menuKind == pickerLogin {
		err = m.ctl.SelectProvider(chosen.name)
	} else {
		err = m.ctl.SelectModel(chosen.description, chosen.name, agent.Efforts[m.effort])
	}
	m.closePicker()
	if err != nil {
		next, cmd := m.reject(err)
		return next, cmd, true
	}
	m.refresh(true)
	return m, nil, true
}

// finishSetup collects the provider setup prompts and adds the provider. The API key is optional.
func (m model) finishSetup() (tea.Model, tea.Cmd, bool) {
	value := strings.TrimSpace(m.input.Value())
	if value == "" && len(m.setup) != 2 {
		return m, nil, true
	}
	m.setup = append(m.setup, value)
	m.input.Reset()
	prompts := []string{"Provider name", "HTTP(S) base URL", "API key (optional)"}
	if len(m.setup) < 3 {
		m.input.Placeholder = prompts[len(m.setup)]
		m.refresh(true)
		return m, nil, true
	}
	err := m.ctl.AddProvider(m.setup[0], m.setup[1], m.setup[2])
	m.closePicker()
	if err != nil {
		next, cmd := m.reject(err)
		return next, cmd, true
	}
	m.refresh(true)
	return m, nil, true
}
