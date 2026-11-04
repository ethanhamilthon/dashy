package tui

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"dashy/internal/agent"
	"dashy/internal/control"
	tea "github.com/charmbracelet/bubbletea"
)

type command struct{ name, icon, description, label, action string }

func commandMenu() []command {
	var menu []command
	for _, info := range control.Commands() {
		menu = append(menu, command{name: info.Name, icon: info.Icon, description: info.Description})
	}
	return menu
}

var commands = commandMenu()

// sessionTitle is the first user message, shortened for the resume picker.
func sessionTitle(s agent.SessionSummary) string {
	title := []rune(s.Title)
	if len(title) == 0 {
		return "(no message)"
	}
	if len(title) > 40 {
		return string(title[:39]) + "…"
	}
	return string(title)
}

// sessionTime shows the last update: time today, day this year, date before.
func sessionTime(t time.Time) string {
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	if t.Year() == now.Year() {
		return t.Format("Jan 2")
	}
	return t.Format("2006-01-02")
}

func (m model) completions() []command {
	text := m.input.Value()
	if m.menuKind != "" {
		var matches []command
		for _, c := range m.menu {
			haystack := c.name
			if m.menuKind != pickerConnect {
				haystack += " " + c.description + " " + c.label
			}
			if strings.Contains(strings.ToLower(haystack), strings.ToLower(text)) {
				matches = append(matches, c)
			}
		}
		return matches
	}
	if !m.hideCompletion && (text == "/resume" || strings.HasPrefix(text, "/resume ")) {
		sessions := m.sessions
		if sessions == nil {
			var err error
			if sessions, err = m.ctl.Sessions(); err != nil {
				return nil
			}
		}
		prefix := strings.TrimSpace(strings.TrimPrefix(text, "/resume"))
		lower := strings.ToLower(prefix)
		titleWidth := 0
		for _, s := range sessions {
			titleWidth = max(titleWidth, min(len([]rune(sessionTitle(s))), 40))
		}
		var matches []command
		for _, s := range sessions {
			title := sessionTitle(s)
			if !strings.HasPrefix(s.ID, prefix) && !strings.Contains(strings.ToLower(title), lower) {
				continue
			}
			label := fmt.Sprintf("%-*s  %s", titleWidth, title, sessionTime(s.Updated))
			matches = append(matches, command{name: "/resume " + s.ID, icon: "<", label: label})
		}
		return matches
	}

	// Commands with a free-text argument: Enter runs the typed text as is.
	if !m.hideCompletion && !strings.ContainsAny(text, "\n\t") {
		if name, ok := strings.CutPrefix(text, "/rename "); ok {
			label := "Rename session to: " + strings.TrimSpace(name)
			if strings.TrimSpace(name) == "" {
				label = "Type a name; Enter on empty restores the default title"
			}
			return []command{{name: text, icon: "#", label: label}}
		}
		if prompt, ok := strings.CutPrefix(text, "/compact "); ok {
			label := "Compact with guidance: " + strings.TrimSpace(prompt)
			if strings.TrimSpace(prompt) == "" {
				label = "Type guidance for the summary, or Enter to compact"
			}
			return []command{{name: text, icon: "=", label: label}}
		}
	}
	if m.hideCompletion || !strings.HasPrefix(text, "/") || strings.ContainsAny(text, " \n\t") {
		return nil
	}
	var result []command
	for _, cmd := range commands {
		if strings.HasPrefix(cmd.name, text) {
			result = append(result, cmd)
		}
	}
	return result
}

func (m *model) complete() bool {
	matches := m.completions()
	if len(matches) == 0 {
		return false
	}
	m.input.SetValue(matches[m.selected%len(matches)].name)
	m.input.CursorEnd()
	m.selected = 0
	return true
}

// keyboardHelp lists the interface key bindings shown by /help.
const keyboardHelp = "Enter: execute selected completion or send / steer while working · Shift+Enter / Alt+Enter / Ctrl+J: newline\n" +
	"Clipboard: Ctrl+V (Cmd+V when forwarded)\n" +
	"Tab: complete · Up/Down: select completion · unmatched slash text: send normally · Up on empty input: recall queued steering\n" +
	"PgUp/PgDn or mouse wheel: scroll timeline\n" +
	"Esc / Ctrl+C: cancel current turn · Ctrl+C when idle or Ctrl+D on empty input: quit"

// runCommand parses a slash command. Picker and completion steps stay in the
// interface; every application operation goes through the controller.
func (m model) runCommand(name string) (tea.Model, tea.Cmd) {
	verb := strings.Fields(name)
	if len(verb) == 0 {
		return m, nil
	}
	if verb[0] != "/resume" && verb[0] != "/compact" && verb[0] != "/rename" && len(verb) != 1 {
		return m.stop("error", "Unexpected command arguments.")
	}
	m.input.Reset()
	m.selected = 0
	m.hideCompletion = false
	switch verb[0] {
	case "/resume":
		if len(verb) == 1 {
			if sessions, err := m.ctl.Sessions(); err == nil && len(sessions) > 0 {
				m.sessions = sessions
				m.input.SetValue("/resume ")
				m.input.CursorEnd()
				m.refresh(true)
				return m, nil
			}
		}
		if len(verb) != 2 {
			return m.stop("notice", "No saved sessions in this project.")
		}
		m.resetCompletions()
		return m.submit(m.ctl.Resume(verb[1]))
	case "/settings", "/models", "/connect":
		kind := map[string]string{"/settings": pickerSettings, "/models": pickerModels, "/connect": pickerConnect}[verb[0]]
		cmd := m.openPicker(kind)
		m.refresh(true)
		return m, cmd

	case "/rename":
		if name == "/rename" {
			m.input.SetValue("/rename ")
			m.input.CursorEnd()
			m.refresh(true)
			return m, nil
		}
		return m.submit(m.ctl.Rename(strings.TrimSpace(strings.TrimPrefix(name, "/rename"))))
	case "/compact":
		return m.submit(m.ctl.Compact(strings.TrimSpace(strings.TrimPrefix(name, "/compact"))))
	case "/help":
		return m.stop("info", control.Help(keyboardHelp))
	case "/events":
		return m.stop("info", cmp.Or(strings.Join(m.ctl.Events(), "\n"), "No events in this session."))
	case "/reload":
		m.ctl.Reload()
		m.refresh(true)
		return m, nil
	case "/new":
		m.resetCompletions()
		return m.submit(m.ctl.NewSession())
	case "/quit":
		m.dismissFileCompletion()
		closed, err := m.ctl.Quit()
		if err != nil {
			return m.reject(err)
		}
		if closed {
			return m, tea.Quit
		}
		m.refresh(true)
		return m, nil
	}
	return m.stop("error", "Unknown command: "+name+". Type /help.")
}

// resetCompletions drops file and session completion state before a session or settings change.
func (m *model) resetCompletions() {
	m.dismissFileCompletion()
	m.fileIndexCache = nil
}

// submit sends an operation to the controller. Rejections keep the composer
// intact and are shown as notices or errors.
func (m model) submit(err error) (tea.Model, tea.Cmd) {
	if err != nil {
		return m.reject(err)
	}
	m.refresh(true)
	return m, nil
}

// reject shows a controller error. State rejections are notices; everything else is an error.
func (m model) reject(err error) (tea.Model, tea.Cmd) {
	kind := "error"
	if e := (*control.Error)(nil); errors.As(err, &e) && slices.Contains([]string{"busy", "cancelling", "queue_full", "setup_required", "not_recallable", "compacting"}, e.Code) {
		kind = "notice"
	}
	return m.stop(kind, err.Error())
}
