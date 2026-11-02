package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"dashy/internal/control"
	tea "github.com/charmbracelet/bubbletea"
)

type command struct{ name, icon, description, label string }

// commandIcons are the completion glyphs; the names and descriptions are shared with the JSON transport.
var commandIcons = map[string]string{"/new": "+", "/help": "?", "/quit": "->", "/resume": "<", "/rename": "#", "/compact": "=", "/model": "~", "/login": "+"}

func commandMenu() []command {
	var menu []command
	for _, info := range control.Commands() {
		menu = append(menu, command{name: info.Name, icon: commandIcons[info.Name], description: info.Description})
	}
	return menu
}

var commands = commandMenu()

// sessionTitle is the first user message, shortened for the resume picker.
func sessionTitle(s control.SessionInfo) string {
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
			if m.menuKind == "model" {
				haystack += " " + c.description
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
		titleWidth, modelWidth := 0, 0
		for _, s := range sessions {
			titleWidth, modelWidth = max(titleWidth, min(len([]rune(sessionTitle(s))), 40)), max(modelWidth, len(s.Model))
		}
		var matches []command
		for _, s := range sessions {
			title := sessionTitle(s)
			if !strings.HasPrefix(s.ID, prefix) && !strings.Contains(strings.ToLower(title), lower) {
				continue
			}
			label := fmt.Sprintf("%-*s  %-*s  %s", titleWidth, title, modelWidth, s.Model, sessionTime(s.Updated))
			if modelWidth == 0 {
				label = fmt.Sprintf("%-*s  %s", titleWidth, title, sessionTime(s.Updated))
			}
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
	"Clipboard: Ctrl+V (Cmd+V when forwarded) · Ctrl+X: remove last image\n" +
	"Tab: complete · Up/Down: select completion · unmatched slash text: send normally · Up on empty input: recall queued steering\n" +
	"PgUp/PgDn or mouse wheel: scroll timeline\n" +
	"Esc / Ctrl+C: cancel current turn"

// runCommand parses a slash command. Picker and completion steps stay in the
// interface; every application operation goes through the controller.
func (m model) runCommand(name string) (tea.Model, tea.Cmd) {
	verb := strings.Fields(name)
	if len(verb) == 0 {
		return m, nil
	}
	if (verb[0] == "/new" || verb[0] == "/resume") && len(m.images) > 0 {
		return m.stop("notice", "Submit or remove pending images before switching sessions.")
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
		sessions := m.sessions
		if sessions == nil {
			sessions, _ = m.ctl.Sessions()
		}
		if len(verb) != 2 || !slices.ContainsFunc(sessions, func(s control.SessionInfo) bool { return s.ID == verb[1] }) {
			return m.stop("error", "choose a session from /resume completions")
		}
		m.resetCompletions()
		return m.submit(control.Request{Type: "resume", Args: map[string]string{"session_id": verb[1]}})
	case "/model", "/login":
		if m.ctl.Status().Busy {
			return m.stop("notice", "Wait for the active turn before changing settings.")
		}
		cmd := m.openPicker(strings.TrimPrefix(verb[0], "/"))
		m.refresh(true)
		return m, cmd

	case "/rename":
		if name == "/rename" {
			m.input.SetValue("/rename ")
			m.input.CursorEnd()
			m.refresh(true)
			return m, nil
		}
		return m.submit(control.Request{Type: "rename", Args: map[string]string{"title": strings.TrimSpace(strings.TrimPrefix(name, "/rename"))}})
	case "/compact":
		return m.submit(control.Request{Type: "compact", Text: strings.TrimSpace(strings.TrimPrefix(name, "/compact"))})
	case "/help":
		return m.stop("info", control.Help(keyboardHelp))
	case "/new":
		m.resetCompletions()
		return m.submit(control.Request{Type: "new"})
	case "/quit":
		m.dismissFileCompletion()
		return m.submit(control.Request{Type: "quit"})
	}
	return m.stop("error", "Unknown command: "+name+". Type /help.")
}

// resetCompletions drops file and session completion state before a session or settings change.
func (m *model) resetCompletions() {
	m.dismissFileCompletion()
	m.fileIndexCache = nil
}

// submit sends a request to the controller. Rejections keep the composer
// intact and are shown as notices or errors.
func (m model) submit(req control.Request) (tea.Model, tea.Cmd) {
	resp, err := m.ctl.Submit(req)
	if err != nil {
		return m.reject(err)
	}
	m.refresh(true)
	if resp.Status == "closed" {
		return m, tea.Quit
	}
	return m, nil
}

// reject shows a controller error. State rejections are notices; everything else is an error.
func (m model) reject(err error) (tea.Model, tea.Cmd) {
	kind := "error"
	var e *control.Error
	if errors.As(err, &e) {
		switch e.Code {
		case "busy", "cancelling", "queue_full", "setup_required", "pending_images", "not_recallable", "attachment_limit", "not_busy", "compacting":
			kind = "notice"
		}
	}
	return m.stop(kind, err.Error())
}
