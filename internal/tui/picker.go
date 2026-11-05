package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/agent"
	"dashy/internal/control"
)

// Picker kinds. "settings" lists the display settings rows (/settings), "models" every
// model with the reasoning effort (/models), "connect" the providers (/connect).
const (
	pickerSettings = "settings"
	pickerModels   = "models"
	pickerConnect  = "connect"
	pickerTasks    = "tasks" // background tasks of the session (/tasks)
)

// actionAdd is the "Add provider" row. Settings rows have a control.Setting* action;
// provider and model rows have none, so their names can be anything.
const actionAdd = "add"

// modelChoices carries the result of listing models for the models picker.
type modelChoices struct {
	generation int
	models     []agent.ModelChoice
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
	m.input.Placeholder = map[string]string{pickerConnect: "Providers", pickerTasks: "Background tasks",
		pickerModels: "Type to filter models", pickerSettings: "Type to filter settings"}[kind]
	m.menuGeneration++
	if kind == pickerConnect {
		m.rebuildConnectMenu()
		if s := m.ctl.Settings(); s.Provider != "" {
			for i, c := range m.menu {
				if c.name == s.Provider && c.action == "" {
					m.selected = i
				}
			}
		}
		return nil
	}
	if kind == pickerSettings {
		m.rebuildSettingsMenu()
		return nil
	}
	if kind == pickerTasks {
		m.rebuildTasksMenu()
		return tasksTickAfter(m.menuGeneration)
	}
	m.effort = slices.Index(agent.Efforts, m.ctl.Settings().Effort)
	m.pickerModels = nil
	m.rebuildModelsMenu()
	ctx, cancel := context.WithCancel(context.Background())
	m.menuCancel = cancel
	generation, ctl := m.menuGeneration, m.ctl
	return func() tea.Msg {
		models, errs, err := ctl.Models(ctx)
		return modelChoices{generation: generation, models: models, errs: errs, err: err}
	}
}

// rebuildSettingsMenu lists the display settings rows.
func (m *model) rebuildSettingsMenu() {
	s := m.ctl.Settings()
	m.menu = nil
	for _, setting := range []string{control.SettingReasoning, control.SettingLines, control.SettingShellOutput} {
		options, current := control.SettingOptions(setting, s)
		parts := make([]string, len(options))
		for i, option := range options {
			parts[i] = " " + option + " "
			if i == current {
				parts[i] = "[" + option + "]"
			}
		}
		name := control.SettingName(setting)
		m.menu = append(m.menu, command{name: name, icon: "*", label: fmt.Sprintf("%-18s%s", name, strings.Join(parts, "")), action: setting})
	}
}

// rebuildModelsMenu lists the models known so far (of modelsFor only, when set) and
// selects the current one.
func (m *model) rebuildModelsMenu() {
	s := m.ctl.Settings()
	m.menu = nil
	for _, c := range m.pickerModels {
		if m.modelsFor != "" && c.Provider != m.modelsFor {
			continue
		}
		m.menu = append(m.menu, command{name: c.Model, description: c.Provider, label: c.Model + "  " + c.Provider})
		if c.Provider == s.Provider && c.Model == s.Model {
			m.selected = len(m.menu) - 1
		}
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
	m.pickerModels, m.modelsFor = nil, ""
	m.form, m.confirmDelete = nil, ""
	m.input.Placeholder = "Ask anything, or type / for commands"
	m.input.Reset()
}

// pickerKey handles keys while a picker is open. Rejected operations keep the
// picker open and show the reason.
func (m model) pickerKey(key tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	if m.menuKind == "" {
		return m, nil, false
	}
	if m.menuKind == pickerProvider {
		return m.formKey(key)
	}
	if m.menuKind == pickerConnect && len(m.menu) > 0 {
		if next, cmd, handled := m.connectKey(key); handled {
			return next, cmd, true
		}
	}
	if m.menuKind == pickerTasks {
		switch key.String() {
		case "k":
			return m.stopSelectedTask()
		case "enter":
			return m.showSelectedTask()
		case "esc", "ctrl+c", "up", "down", "tab":
		default:
			return m, nil, true // the list has no filter: other keys do nothing
		}
	}
	switch key.String() {
	case "esc", "ctrl+c":
		m.closePicker()
	case "left", "right":
		delta := map[string]int{"left": -1, "right": 1}[key.String()]
		switch m.menuKind {
		case pickerSettings:
			matches := m.completions()
			if len(matches) == 0 {
				return m, nil, true
			}
			err := m.ctl.ChangeSetting(matches[m.selected%len(matches)].action, delta)
			m.rebuildSettingsMenu()
			m.refresh(true)
			if err != nil {
				next, cmd := m.reject(err)
				return next, cmd, true
			}
			return m, nil, true
		case pickerModels:
		default:
			return m, nil, false
		}
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
	if m.menuKind == pickerSettings {
		return m, nil, true // settings rows change with Left/Right
	}
	err := m.ctl.SelectModel(chosen.description, chosen.name, agent.Efforts[m.effort])
	m.closePicker()
	if err != nil {
		next, cmd := m.reject(err)
		return next, cmd, true
	}
	m.refresh(true)
	return m, nil, true
}

// tasksTick refreshes the open /tasks list: statuses and durations change while it is shown.
type tasksTick struct{ generation int }

func tasksTickAfter(generation int) tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tasksTick{generation} })
}

// rebuildTasksMenu lists the tasks newest first, keeping the selected task selected.
func (m *model) rebuildTasksMenu() {
	var keep string
	if len(m.menu) > 0 {
		keep = m.menu[m.selected%len(m.menu)].name
	}
	tasks, err := m.ctl.Tasks()
	if err != nil {
		m.setFlash("error", err.Error())
	}
	m.menu, m.selected = nil, 0
	for i := len(tasks) - 1; i >= 0; i-- {
		t := tasks[i]
		status := t.Status
		if t.ExitCode != nil && *t.ExitCode != 0 {
			status += fmt.Sprintf(" (%d)", *t.ExitCode)
		}
		end := t.Ended
		if end.IsZero() {
			end = time.Now()
		}
		took := end.Sub(t.Started).Round(time.Second).String()
		line := strings.Join(strings.Fields(t.Command), " ")
		m.menu = append(m.menu, command{name: t.ID, description: t.Status, action: t.Log,
			label: fmt.Sprintf("%-4s %-12s %7s  $ %s", t.ID, status, took, line)})
		if t.ID == keep {
			m.selected = len(m.menu) - 1
		}
	}
}

func (m model) selectedTask() (command, bool) {
	if len(m.menu) == 0 {
		return command{}, false
	}
	return m.menu[m.selected%len(m.menu)], true
}

// stopSelectedTask kills the selected task if it runs; the list stays open.
func (m model) stopSelectedTask() (tea.Model, tea.Cmd, bool) {
	task, ok := m.selectedTask()
	if !ok || task.description != "running" {
		return m, nil, true
	}
	if err := m.ctl.StopTask(task.name); err != nil {
		next, cmd := m.reject(err)
		return next, cmd, true
	}
	m.setFlash("notice", "Task "+task.name+" stopped.")
	m.rebuildTasksMenu()
	m.refresh(true)
	return m, nil, true
}

// showSelectedTask closes the list and shows the end of the task's output in the timeline.
func (m model) showSelectedTask() (tea.Model, tea.Cmd, bool) {
	task, ok := m.selectedTask()
	if !ok {
		return m, nil, true
	}
	m.closePicker()
	m.add("info", task.label+"\n\n"+agent.TaskOutput(agent.Task{Log: task.action})+"\n\nlog: "+task.action)
	m.refresh(true)
	return m, nil, true
}
