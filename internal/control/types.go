// Package control is the application controller for the terminal interface.
// It coordinates turns for the TUI, serializes mutations and reports progress
// as ordered events.
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
	Source    string         // started: user, notify, queued or compact
	Agent     *agent.Event   // agent: the unmodified event from the agent
	Text      string         // notice and error text
	Status    string         // done: ok, error or cancelled
	Error     string         // done: failure text, informational for turns
	Message   string         // done (compact): summary or cancellation text
	Unsent    []agent.Input  // done: queued or retried inputs not accepted by the model
	Session   map[string]any // session: description of the active session
	Records   []agent.Record // session: display journal after the change
}

// Error is a structured rejection. Code is stable for interfaces; Message is
// user-facing and never contains credentials.
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
	Name        string `json:"name"`
	Description string `json:"description"`
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

// Help renders the command reference. keyboard holds interface-specific key
// bindings.
func Help(keyboard string) string {
	var help strings.Builder
	for _, cmd := range commandList {
		fmt.Fprintf(&help, "%-9s %s\n", cmd.Name, cmd.Description)
	}
	if keyboard != "" {
		help.WriteString("\n" + keyboard + "\n")
	}
	help.WriteString("\nHost access: the bash tool runs commands with your permissions; not an OS security sandbox.")
	return help.String()
}

// SessionInfo describes one saved session of this project.
type SessionInfo struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Model   string    `json:"model"`
	Updated time.Time `json:"updated"`
}

// ModelInfo is one model offered by a configured provider.
type ModelInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ModelList is the result of listing available models. Errors maps provider
// names to redacted failure messages.
type ModelList struct {
	Models []ModelInfo       `json:"models"`
	Errors map[string]string `json:"errors"`
}

// ProviderInfo describes a configured provider. Credentials are reported as
// presence only; the key itself is never returned.
type ProviderInfo struct {
	Name             string `json:"name"`
	BaseURL          string `json:"base_url"`
	APIKeyEnv        string `json:"api_key_env,omitempty"`
	APIKeyConfigured bool   `json:"api_key_configured"`
}

// Status is a consistent snapshot of the active operation for interfaces.
type Status struct {
	Busy       bool
	Cancelling bool
	Compacting bool
	Turn       string
	Started    time.Time
	Queued     []agent.Input
	Images     int // images in the running turn and in the queue
}
