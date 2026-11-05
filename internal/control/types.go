package control

import (
	"fmt"
	"time"

	"dashy/internal/agent"
)

// Event is one ordered notification from the controller. Consumers receive
// events in emission order and must not call the controller from the callback.
type Event struct {
	Kind    string // started, agent, notice, error, done or session
	Agent   *agent.Event
	Text    string         // started: the input text; notice and error: the text; done (ok): final assistant text
	Status  string         // done: ok, error or cancelled
	Error   string         // done: failure text
	Unsent  []agent.Input  // done: queued inputs the model did not accept
	Records []agent.Record // session: display records after the change
	Title   string         // session title after this event (rename, else first user line)
}

// Error is a rejection. Code tells the interface how to show it; Message is user-facing and never contains credentials.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(code, format string, args ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

type Status struct {
	Busy       bool
	Cancelling bool
	Started    time.Time
	Queued     []agent.Input
	Tasks      int
}
