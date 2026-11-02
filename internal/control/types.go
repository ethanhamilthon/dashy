// Package control coordinates turns for the terminal interface: turn state,
// cancellation, the steering queue and session switching. Operations are plain
// typed methods; events report what happened, in order.
package control

import (
	"fmt"
	"strings"
	"time"

	"dashy/internal/agent"
)

// Event is one ordered notification from the controller. Consumers receive
// events in emission order and must not call the controller from the callback.
type Event struct {
	Kind      string         // started, agent, notice, error, done or session
	Turn      string         // operation identifier
	Operation string         // started, done, session: turn or compact
	Source    string         // started: user, notify or queued
	Agent     *agent.Event   // agent: the unmodified event from the agent
	Text      string         // started: the input text; notice and error: the text
	Status    string         // done: ok, error or cancelled
	Error     string         // done: failure text
	Message   string         // done (compact): summary or cancellation text
	Unsent    []agent.Input  // done: queued inputs the model did not accept
	Records   []agent.Record // session: display records after the change
	Title     string         // session title after this event (rename, else first user line)
}

// Error is a structured rejection. Code is stable; Message is user-facing and never contains credentials.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CommandInfo describes one slash command.
type CommandInfo struct {
	Name        string
	Description string
}

var commandList = []CommandInfo{
	{"/new", "Start a new chat and shell; re-read configuration"},
	{"/help", "Show commands and keyboard shortcuts"},
	{"/quit", "Save the chat and exit"},
	{"/resume", "Resume a saved session in this project"},
	{"/rename", "Rename this session; empty name restores the default"},
	{"/compact", "Summarize older turns; add a prompt to guide it"},
	{"/settings", "Configure provider, model and display options"},
	{"/login", "Select or create a provider"},
}

// Commands returns the slash commands in completion order.
func Commands() []CommandInfo { return append([]CommandInfo(nil), commandList...) }

// Help renders the command reference followed by keyboard bindings.
func Help(keyboard string) string {
	var help strings.Builder
	for _, cmd := range commandList {
		fmt.Fprintf(&help, "%-9s %s\n", cmd.Name, cmd.Description)
	}
	help.WriteString("\n" + keyboard + "\n")
	help.WriteString("\nHost access: the bash tool runs commands with your permissions; not an OS security sandbox.")
	return help.String()
}

// SessionInfo describes one saved session of this project.
type SessionInfo struct {
	ID      string
	Title   string
	Updated time.Time
}

// ModelInfo is one model offered by a configured provider.
type ModelInfo struct {
	Provider string
	Model    string
}

// ModelList is the result of listing models. Errors maps provider names to redacted failures.
type ModelList struct {
	Models []ModelInfo
	Errors map[string]string
}

// Status is a consistent snapshot of the active operation.
type Status struct {
	Busy       bool
	Cancelling bool
	Compacting bool
	Started    time.Time
	Queued     []agent.Input
	Images     int // new images the next model request will carry, including steering and dashy view
}
