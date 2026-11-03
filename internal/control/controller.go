package control

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"dashy/internal/agent"
)

// Controller coordinates turns for the TUI. One operation (a turn or a
// compaction) is active at a time; inputs arriving meanwhile become steering.
// Events are emitted under the controller lock, so emit must not block and must
// not call back into the controller.
type Controller struct {
	a    *agent.Agent
	emit func(Event)

	mu       sync.Mutex
	active   *operation
	idle     chan struct{} // closed when active ends; nil while idle
	deferred *deferredAction
	journal  []agent.Record // the display timeline: the one copy every view reads
	custom   string         // /rename title
	first    string         // first line of the first user message
	ctx      int64          // context size shown in the status line
	closing  bool
	shut     bool
	closed   chan struct{}
}

// operation is a turn or compaction holding the controller.
type operation struct {
	kind      string // turn or compact
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
	if emit == nil {
		emit = func(Event) {}
	}
	c := &Controller{a: a, emit: emit, journal: a.Records(), closed: make(chan struct{})}
	// Input pushed while idle (a notification, dashy view) starts a turn at once.
	a.Queue().OnPush(func() { go c.view(func() { c.deliverLocked() }) })
	c.custom = a.CustomTitle()
	c.first = a.DefaultTitle()
	c.ctx = a.ContextTokens()
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
		s = Status{Images: c.a.PendingImages()}
		if c.active != nil {
			s.Busy, s.Cancelling = true, c.active.cancelled
			s.Compacting, s.Started = c.active.kind == "compact", c.active.started
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

// Note applies a display event of the interface (a notice, error or info) to the timeline without emitting it.
func (c *Controller) Note(e agent.Event) {
	c.view(func() { c.journal = agent.ApplyEvent(c.journal, e) })
}

// History returns the display journal of the session.
func (c *Controller) History() (records []agent.Record) {
	c.view(func() { records = slices.Clone(c.journal) })
	return records
}

// Sessions lists saved sessions of this project, most recent first.
func (c *Controller) Sessions() (sessions []agent.SessionSummary, err error) {
	c.view(func() { sessions, err = c.sessionsLocked() })
	return sessions, err
}

// Settings returns a copy of the effective settings.
func (c *Controller) Settings() (s agent.Settings) {
	c.view(func() { s = c.a.Settings() })
	return s
}

// AttachmentsDir is where pasted and viewed images are saved.
func (c *Controller) AttachmentsDir() string { return c.a.AttachmentsDir() }

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

// beginLocked makes a new operation of kind (turn or compact) active.
func (c *Controller) beginLocked(kind string) *operation {
	ctx, cancel := context.WithCancel(context.Background())
	c.active = &operation{kind: kind, ctx: ctx, cancel: cancel, started: time.Now()}
	c.idle = make(chan struct{})
	return c.active
}

func (c *Controller) beginTurnLocked(input agent.Input) {
	op := c.beginLocked("turn")
	c.publish(Event{Kind: "started", Operation: "turn", Text: input.Text})
	go func() { c.finishTurn(op, c.a.RunInput(op.ctx, input, func(e agent.Event) { c.observe(op, e) })) }()
}

func (c *Controller) beginCompactLocked(prompt string) {
	op := c.beginLocked("compact")
	c.publish(Event{Kind: "started", Operation: "compact", Text: prompt})
	go func() {
		text, err := c.a.Compact(op.ctx, prompt)
		c.finishCompact(op, text, err)
	}()
}

func (c *Controller) endLocked(op *operation) {
	op.cancel()
	c.active = nil
	close(c.idle)
	c.idle = nil
}

// observe applies one agent event to controller state before reporting it.
func (c *Controller) observe(op *operation, e agent.Event) {
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
			for _, in := range unsent {
				c.a.ReleaseImages(len(in.Images))
			}
		}
		c.endLocked(op)
		final := ""
		if status == "ok" {
			final = c.a.FinalText() // committed history: the answer after the last tool output
		}
		c.publish(Event{Kind: "done", Operation: "turn", Status: status, Error: text, Unsent: unsent, Text: final})
		c.settleLocked(unsent)
	})
}

// finishCompact ends a compaction. Success replaces the display with the compacted history.
func (c *Controller) finishCompact(op *operation, text string, err error) {
	c.view(func() {
		status, message, failure := "ok", text, ""
		switch {
		case op.cancelled || errors.Is(err, context.Canceled):
			status, message = "cancelled", "Compaction cancelled."
		case err != nil:
			status, message, failure = "error", "", err.Error()
		}
		c.endLocked(op)
		if status == "ok" {
			c.ctx = c.a.ContextTokens()
		}
		if status == "error" {
			c.publish(Event{Kind: "error", Text: failure})
		} else {
			c.publish(Event{Kind: "notice", Text: message})
		}
		c.publish(Event{Kind: "done", Operation: "compact", Status: status, Error: failure})
		c.settleLocked(nil)
	})
}

// settleLocked runs after an operation ends: it performs a deferred session change
// or quit, and otherwise delivers queued notifications.
func (c *Controller) settleLocked(unsent []agent.Input) {
	d := c.deferred
	if d == nil || c.closing {
		c.deliverLocked()
		return
	}
	c.deferred = nil
	if d.kind != "quit" && slices.ContainsFunc(unsent, func(in agent.Input) bool { return len(in.Images) > 0 }) {
		c.publish(Event{Kind: "error", Text: "Submit or remove pending images before switching sessions."})
		c.deliverLocked()
		return
	}
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
func (c *Controller) deliverLocked() bool {
	if c.active != nil || c.shut || c.closing {
		return false
	}
	next, ok := c.a.Queue().Pop()
	if !ok {
		return false
	}
	c.beginTurnLocked(next)
	return true
}

// publish records an event in the display journal, stamps the title and emits it.
func (c *Controller) publish(e Event) {
	switch e.Kind {
	case "agent":
		c.journal = agent.ApplyEvent(c.journal, *e.Agent)
		if e.Agent.Kind == "user" && c.first == "" {
			c.first = agent.FirstLine(e.Agent.Text)
		}
	case "notice", "error":
		c.journal = agent.ApplyEvent(c.journal, agent.Event{Kind: e.Kind, Text: e.Text})
	case "session":
		c.journal = slices.Clone(e.Records)
		c.first = c.a.DefaultTitle()
	}
	e.Title = c.titleLocked()
	c.emit(e)
}

func (c *Controller) sessionsLocked() ([]agent.SessionSummary, error) { return c.a.Sessions() }

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
