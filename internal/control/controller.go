package control

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"dashy/internal/agent"
)

// Controller coordinates turns for the TUI. One turn is active at a
// time; inputs arriving meanwhile become steering.
// Events are emitted under the controller lock, so emit must not block and must
// not call back into the controller.
type Controller struct {
	a    *agent.Agent
	emit func(Event)

	mu       sync.Mutex
	active   *operation
	idle     chan struct{} // closed when active ends; nil while idle
	deferred *deferredAction
	reload   bool           // /reload waiting for the active operation
	journal  []agent.Record // the display timeline: the one copy every view reads
	events   []string       // notices and errors of this session, shown by /events
	custom   string         // /rename title
	first    string         // first line of the first user message
	ctx      int64          // context size shown in the status line
	closing  bool
	shut     bool
	closed   chan struct{}
}

// operation is the turn holding the controller.
type operation struct {
	ctx       context.Context
	cancel    context.CancelFunc
	started   time.Time
	cancelled bool
}

// deferredAction is a session change or quit waiting for the active operation.
type deferredAction struct {
	kind    string // new, resume or quit
	session string
}

// New returns a controller for a. The agent must outlive the controller.
func New(a *agent.Agent, emit func(Event)) *Controller {
	c := &Controller{a: a, emit: emit, journal: a.Records(), closed: make(chan struct{})}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.custom = a.CustomTitle()
	c.first = a.DefaultTitle()
	c.ctx = a.ContextTokens()
	// Input pushed while idle (a task result, dashy view) starts a turn at once;
	// input queued before the controller existed starts one now.
	a.Queue().OnPush(func() { go c.view(func() { c.deliverLocked() }) })
	c.deliverLocked()
	return c
}

func (c *Controller) view(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

// Closed is closed after a quit has saved the session.
func (c *Controller) Closed() <-chan struct{} { return c.closed }

// Status reports the active operation.
func (c *Controller) Status() (s Status) {
	c.view(func() {
		s = Status{Tasks: c.a.RunningTasks()}
		if c.active != nil {
			s.Busy, s.Cancelling = true, c.active.cancelled
			s.Started = c.active.started
			s.Queued = c.a.Queue().Pending()
		}
	})
	return s
}

// Title is the custom title if one is set, else the first line of the first user message.
func (c *Controller) Title() (title string) {
	c.view(func() { title = c.titleLocked() })
	return title
}

func (c *Controller) titleLocked() string {
	if c.custom != "" {
		return c.custom
	}
	return c.first
}

// Context is the size of the next prompt in tokens, as shown in the status line.
func (c *Controller) Context() (tokens int64) {
	c.view(func() { tokens = c.ctx })
	return tokens
}

// Note records a display event of the interface without emitting it: notices and errors go to Events, the rest to the timeline.
func (c *Controller) Note(e agent.Event) {
	c.view(func() {
		if isStatus(e) {
			c.events = append(c.events, time.Now().Format("15:04:05 ")+e.Text)
		} else {
			c.journal = agent.ApplyEvent(c.journal, e)
		}
	})
}

// Events returns the notices and errors of the session, oldest first.
func (c *Controller) Events() (events []string) {
	c.view(func() { events = slices.Clone(c.events) })
	return events
}

// History returns the display journal of the session.
func (c *Controller) History() (records []agent.Record) {
	c.view(func() { records = slices.Clone(c.journal) })
	return records
}

// Sessions lists saved sessions of this project, most recent first.
func (c *Controller) Sessions() (sessions []agent.SessionSummary, err error) {
	c.view(func() { sessions, err = c.a.Sessions() })
	return sessions, err
}

// Settings returns a copy of the effective settings.
func (c *Controller) Settings() (s agent.Settings) {
	c.view(func() { s = c.a.Settings() })
	return s
}

// AttachmentsDir is where pasted and viewed images are saved.
func (c *Controller) AttachmentsDir() (dir string) {
	c.view(func() { dir = c.a.AttachmentsDir() })
	return dir
}

// Close cancels any active operation and waits for it to finish. It does not save.
func (c *Controller) Close() {
	c.view(func() {
		c.closing = true
		if c.active != nil {
			c.active.cancelled = true
			c.active.cancel()
		}
	})
	c.wait()
}

func (c *Controller) wait() {
	for {
		var idle chan struct{}
		c.view(func() { idle = c.idle })
		if idle == nil {
			return
		}
		<-idle
	}
}

// closeLocked saves the session and marks the controller closed.
func (c *Controller) closeLocked() error {
	if c.shut {
		return nil
	}
	c.closing = true
	if c.a.HasSavedSession() {
		if err := c.a.Save(); err != nil {
			c.closing = false
			return err
		}
	}
	c.shut = true
	close(c.closed)
	return nil
}

// beginLocked makes a new operation active.
func (c *Controller) beginLocked() *operation {
	ctx, cancel := context.WithCancel(context.Background())
	c.active = &operation{ctx: ctx, cancel: cancel, started: time.Now()}
	c.idle = make(chan struct{})
	return c.active
}

func (c *Controller) beginTurnLocked(input agent.Input) {
	op := c.beginLocked()
	c.publish(Event{Kind: "started", Text: input.Text})
	go func() { c.finishTurn(op, c.a.RunInput(op.ctx, input, func(e agent.Event) { c.observe(e) })) }()
}

func (c *Controller) endLocked(op *operation) {
	op.cancel()
	c.active = nil
	close(c.idle)
	c.idle = nil
}

// observe applies one agent event to controller state before reporting it.
func (c *Controller) observe(e agent.Event) {
	c.view(func() {
		if e.Meta != nil && e.Meta.ContextInput != nil {
			c.ctx = *e.Meta.ContextInput
		}
		c.publish(Event{Kind: "agent", Agent: &e})
	})
}

// finishTurn ends a turn exactly once. Queued instructions either start the next
// turn (after a clean finish) or are returned as unsent for the caller to keep.
func (c *Controller) finishTurn(op *operation, err error) {
	c.view(func() {
		status, text := "ok", ""
		switch {
		case op.cancelled || errors.Is(err, context.Canceled):
			status = "cancelled"
		case err != nil:
			status, text = "error", err.Error()
		}
		if status == "cancelled" {
			c.publish(Event{Kind: "notice", Text: "Turn cancelled."})
		}
		c.ctx = c.a.ContextTokens()
		// Queued input stays in the session queue and the next turn takes it in order. Composer
		// input is handed back when the turn did not finish cleanly or the session is changing.
		var unsent []agent.Input
		if status != "ok" || c.deferred != nil || c.closing {
			unsent = c.a.Queue().RemoveComposer()
		}
		c.endLocked(op)
		final := ""
		if status == "ok" {
			final = c.a.FinalText() // committed history: the answer after the last tool output
		}
		c.publish(Event{Kind: "done", Status: status, Error: text, Unsent: unsent, Text: final})
		c.settleLocked()
	})
}

// settleLocked runs after an operation ends: it performs a deferred session change
// or quit, and otherwise delivers queued notices.
func (c *Controller) settleLocked() {
	if c.reload {
		c.reload = false
		c.reloadLocked()
	}
	d := c.deferred
	if d == nil || c.closing {
		c.deliverLocked()
		return
	}
	c.deferred = nil
	if d.kind == "quit" {
		if err := c.closeLocked(); err != nil {
			c.publish(Event{Kind: "error", Text: err.Error()})
		}
		return
	}
	if err := c.switchLocked(d.kind, d.session); err != nil {
		c.publish(Event{Kind: "error", Text: err.Error()})
	}
	c.deliverLocked()
}

// switchLocked changes the session. Callers guarantee that no operation runs.
func (c *Controller) switchLocked(kind, id string) error {
	var err error
	if kind == "resume" {
		err = c.a.Resume(id)
	} else {
		err = c.a.NewSession()
	}
	if err != nil {
		return err
	}
	c.custom = c.a.CustomTitle()
	c.ctx = c.a.ContextTokens()
	c.publish(Event{Kind: "session", Records: c.a.Records()})
	return nil
}

// deliverLocked starts a turn with the oldest queued input when idle.
func (c *Controller) deliverLocked() {
	if c.active != nil || c.shut || c.closing {
		return
	}
	if next, ok := c.a.Queue().Pop(); ok {
		c.beginTurnLocked(next)
	}
}

// publish records an event in the display journal, stamps the title and emits it.
func (c *Controller) publish(e Event) {
	if e.Kind == "agent" && isStatus(*e.Agent) {
		e = Event{Kind: e.Agent.Kind, Text: e.Agent.Text}
	}
	switch e.Kind {
	case "agent":
		c.journal = agent.ApplyEvent(c.journal, *e.Agent)
		if e.Agent.Kind == "user" && c.first == "" {
			c.first = agent.FirstLine(e.Agent.Text)
		}
	case "notice", "error":
		c.events = append(c.events, time.Now().Format("15:04:05 ")+e.Text)
	case "session":
		c.journal = slices.Clone(e.Records)
		c.events = nil
		c.first = c.a.DefaultTitle()
	}
	e.Title = c.titleLocked()
	c.emit(e)
}

// isStatus tells notices and errors outside the conversation (retries, failures) from task results, view images and tool output.
func isStatus(e agent.Event) bool {
	return (e.Kind == "notice" || e.Kind == "error") && !e.Final && !agent.IsNotice(e.Text)
}

// setupError explains what is missing before a message can start a turn.
func (c *Controller) setupError() error {
	if hint := c.a.Settings().SetupHint(); hint != "" {
		return fail("setup_required", "%s", hint)
	}
	return nil
}

func (c *Controller) requireIdle(msg string) error {
	if c.active != nil {
		return fail("busy", "%s", msg)
	}
	return nil
}
