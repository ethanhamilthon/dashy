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
	idle     chan struct{}
	deferred *deferredAction
	reload   bool
	journal  []agent.Record
	events   []string
	custom   string
	first    string
	ctx      int64
	closing  bool
	shut     bool
	closed   chan struct{}
}

type operation struct {
	ctx       context.Context
	cancel    context.CancelFunc
	started   time.Time
	cancelled bool
}

type deferredAction struct {
	kind    string
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
	a.Queue().OnPush(func() { go c.view(func() { c.deliverLocked() }) })
	c.deliverLocked()
	return c
}

func (c *Controller) view(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

func (c *Controller) Closed() <-chan struct{} { return c.closed }

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

func (c *Controller) Context() (tokens int64) {
	c.view(func() { tokens = c.ctx })
	return tokens
}

func (c *Controller) Note(e agent.Event) {
	c.view(func() {
		if isStatus(e) {
			c.events = append(c.events, time.Now().Format("15:04:05 ")+e.Text)
		} else {
			c.journal = agent.ApplyEvent(c.journal, e)
		}
	})
}

func (c *Controller) Events() (events []string) {
	c.view(func() { events = slices.Clone(c.events) })
	return events
}

func (c *Controller) History() (records []agent.Record) {
	c.view(func() { records = slices.Clone(c.journal) })
	return records
}

func (c *Controller) Sessions() (sessions []agent.SessionSummary, err error) {
	c.view(func() { sessions, err = c.a.Sessions() })
	return sessions, err
}

func (c *Controller) Settings() (s agent.Settings) {
	c.view(func() { s = c.a.Settings() })
	return s
}

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

func (c *Controller) observe(e agent.Event) {
	c.view(func() {
		if e.Meta != nil && e.Meta.ContextInput != nil {
			c.ctx = *e.Meta.ContextInput
		}
		c.publish(Event{Kind: "agent", Agent: &e})
	})
}

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
		var unsent []agent.Input
		if status != "ok" || c.deferred != nil || c.closing {
			unsent = c.a.Queue().RemoveComposer()
		}
		c.endLocked(op)
		final := ""
		if status == "ok" {
			final = c.a.FinalText()
		}
		c.publish(Event{Kind: "done", Status: status, Error: text, Unsent: unsent, Text: final})
		c.settleLocked()
	})
}

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

func (c *Controller) deliverLocked() {
	if c.active != nil || c.shut || c.closing {
		return
	}
	if next, ok := c.a.Queue().Pop(); ok {
		c.beginTurnLocked(next)
	}
}

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

func isStatus(e agent.Event) bool {
	return (e.Kind == "notice" || e.Kind == "error") && !e.Final && !agent.IsNotice(e.Text)
}

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
