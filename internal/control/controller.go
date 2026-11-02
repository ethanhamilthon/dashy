package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"dashy/internal/agent"
	"dashy/internal/shell"
)

// maxPendingImages bounds images waiting in the queue plus the running turn.
const maxPendingImages = 8

// Controller coordinates turns for the TUI. One operation (a turn or a
// compaction) is active at a time; inputs arriving meanwhile become steering.
// Events are emitted under the controller lock, so emit must not block and must
// not call back into the controller.
type Controller struct {
	a    *agent.Agent
	emit func(Event)

	mu       sync.Mutex
	count    int
	active   *operation
	idle     chan struct{} // closed when active ends; nil while idle
	deferred *deferredAction
	journal  []agent.Record // display journal, the same records the agent persists
	closing  bool
	shut     bool
	reason   string
	closed   chan struct{}
}

// operation is a turn or compaction holding the controller.
type operation struct {
	id        string
	kind      string // turn or compact
	ctx       context.Context
	cancel    context.CancelFunc
	started   time.Time
	steering  *agent.Steering
	images    []shell.Image // submitted images, restored when the turn is withdrawn
	cancelled bool
	retry     bool
	retryText string
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
	return &Controller{a: a, emit: emit, journal: a.Records(), closed: make(chan struct{})}
}

// view runs fn under the lock, so state is never read mid-change.
func (c *Controller) view(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

// Closed is closed after a quit or shutdown has saved the session.
func (c *Controller) Closed() <-chan struct{} { return c.closed }

// Reason reports why the controller closed, or "" while it is open.
func (c *Controller) Reason() (reason string) {
	c.view(func() { reason = c.reason })
	return reason
}

// Status reports the active operation.
func (c *Controller) Status() (s Status) {
	c.view(func() {
		s = Status{Images: c.pendingImages()}
		if c.active != nil {
			s.Busy, s.Cancelling = true, c.active.cancelled
			s.Compacting, s.Turn, s.Started = c.active.kind == "compact", c.active.id, c.active.started
			if c.active.steering != nil {
				s.Queued = c.active.steering.Pending()
			}
		}
	})
	return s
}

// History returns the display journal of the session.
func (c *Controller) History() (records []agent.Record) {
	c.view(func() { records = slices.Clone(c.journal) })
	return records
}

// Sessions lists saved sessions of this project, most recent first.
func (c *Controller) Sessions() (sessions []SessionInfo, err error) {
	c.view(func() { sessions, err = c.sessionsLocked() })
	return sessions, err
}

// Settings returns a copy of the effective settings.
func (c *Controller) Settings() (s agent.Settings) {
	c.view(func() { s = c.a.Settings() })
	return s
}

// AttachmentsDir returns the attachments directory.
func (c *Controller) AttachmentsDir() string {
	return c.a.AttachmentsDir()
}

// HistoryPath is the file that stores the active session.
func (c *Controller) HistoryPath() (path string) {
	c.view(func() { path = c.a.HistoryPath })
	return path
}

// PrimaryConfig is the configuration file that receives settings changes.
func (c *Controller) PrimaryConfig() (path string) {
	c.view(func() { path = c.a.PrimaryConfig() })
	return path
}

// Deliver starts a turn for pending external notifications when idle.
func (c *Controller) Deliver() (started bool) {
	c.view(func() { started = c.deliverLocked() })
	return started
}

// Run delivers external notifications until ctx ends. Notifications that arrive
// while a turn runs are accepted by that turn at its next safe boundary.
func (c *Controller) Run(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.Deliver()
		}
	}
}

// Wait blocks until no operation is active or ctx ends.
func (c *Controller) Wait(ctx context.Context) error {
	for {
		var idle chan struct{}
		c.view(func() { idle = c.idle })
		if idle == nil {
			return nil
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Shutdown saves the session and closes the controller.
func (c *Controller) Shutdown(reason string) (err error) {
	c.view(func() {
		if c.active != nil {
			err = fail("busy", "Wait for the active turn before closing.")
			return
		}
		err = c.closeLocked(reason)
	})
	return err
}

// Close cancels any active operation and waits for it to finish. It does not save.
func (c *Controller) Close() {
	c.view(func() {
		c.closing = true
		if c.active != nil {
			c.cancelActiveLocked()
		}
	})
	_ = c.Wait(context.Background())
}

func (c *Controller) closeLocked(reason string) error {
	if c.shut {
		return nil
	}
	c.closing = true
	c.a.RememberDisplay(c.journal)
	if c.a.HasSavedSession() {
		if err := c.a.Save(); err != nil {
			c.closing = false
			return err
		}
	}
	c.shut, c.reason = true, reason
	close(c.closed)
	return nil
}

func (c *Controller) beginTurnLocked(input agent.Input, source string) string {
	c.count++
	turnID := fmt.Sprintf("turn-%d", c.count)
	ctx, cancel := context.WithCancel(context.Background())
	images := append([]shell.Image(nil), input.Images...)
	op := &operation{
		id:       turnID,
		kind:     "turn",
		ctx:      ctx,
		cancel:   cancel,
		started:  time.Now(),
		steering: &agent.Steering{},
		images:   images,
	}
	c.active = op
	c.idle = make(chan struct{})
	c.publish(Event{Kind: "started", Turn: turnID, Operation: "turn", Source: source})
	go c.runTurn(op, input)
	return turnID
}

func (c *Controller) beginCompactLocked(prompt string) {
	c.count++
	turnID := fmt.Sprintf("compact-%d", c.count)
	ctx, cancel := context.WithCancel(context.Background())
	op := &operation{id: turnID, kind: "compact", ctx: ctx, cancel: cancel, started: time.Now()}
	c.active = op
	c.idle = make(chan struct{})
	c.publish(Event{Kind: "started", Turn: turnID, Operation: "compact", Source: "compact"})
	go c.runCompact(op, prompt)
}

func (c *Controller) endLocked(op *operation) {
	op.cancel()
	c.active = nil
	close(c.idle)
	c.idle = nil
}

func (c *Controller) cancelActiveLocked() {
	c.active.cancelled = true
	c.active.cancel()
}

func (c *Controller) runTurn(op *operation, input agent.Input) {
	err := c.a.RunInput(op.ctx, input, func(e agent.Event) { c.observe(op, e) }, op.steering)
	c.finishTurn(op, err)
}

// observe applies one agent event to controller state before reporting it.
func (c *Controller) observe(op *operation, e agent.Event) {
	c.view(func() {
		if e.Kind == "withdraw" {
			op.retry, op.retryText = true, e.Text
		}
		c.publish(Event{Kind: "agent", Turn: op.id, Agent: &e})
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
		pending := op.steering.TakeInputs()
		promote := status == "ok" && len(pending) > 0 && c.deferred == nil && !c.closing
		var unsent []agent.Input
		var next agent.Input
		var rest []agent.Input
		if promote {
			next, rest = pending[0], pending[1:]
		} else {
			if op.retry {
				unsent = append(unsent, agent.Input{Text: op.retryText, Images: op.images})
			}
			unsent = append(unsent, pending...)
		}
		c.endLocked(op)
		c.publish(Event{Kind: "done", Turn: op.id, Operation: "turn", Status: status, Error: text, Unsent: unsent})
		if promote {
			c.beginTurnLocked(next, "queued")
			for _, in := range rest {
				c.active.steering.PushInput(in)
			}
			return
		}
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
			records := c.a.Records()
			c.publish(Event{Kind: "session", Records: records, Session: c.a.SessionInfoFor(records), Operation: "compact"})
		}
		if status == "error" {
			c.publish(Event{Kind: "error", Text: failure})
		}
		c.publish(Event{Kind: "done", Turn: op.id, Operation: "compact", Status: status, Error: failure, Message: message})
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
		if err := c.closeLocked("quit"); err != nil {
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
	c.a.RememberDisplay(c.journal)
	var err error
	if kind == "resume" {
		err = c.a.Resume(id)
	} else {
		err = c.a.NewSession()
	}
	if err != nil {
		return err
	}
	records := c.a.Records()
	c.publish(Event{Kind: "session", Records: records, Session: c.a.SessionInfoFor(records)})
	return nil
}

// deliverLocked turns pending notifications into a turn when idle.
func (c *Controller) deliverLocked() bool {
	if c.active != nil || c.shut || c.closing {
		return false
	}
	messages := c.a.Notifications()
	if len(messages) == 0 {
		return false
	}
	c.beginTurnLocked(agent.Input{Text: messages[0]}, "notify")
	for _, text := range messages[1:] {
		c.active.steering.Push(text)
	}
	return true
}

// publish records an event in the display journal and emits it.
func (c *Controller) publish(e Event) {
	switch e.Kind {
	case "agent":
		c.journal = agent.ApplyEvent(c.journal, *e.Agent)
	case "notice", "error":
		c.journal = agent.ApplyEvent(c.journal, agent.Event{Kind: e.Kind, Text: e.Text})
	case "session":
		c.journal = slices.Clone(e.Records)
	}
	c.emit(e)
}

func (c *Controller) sessionsLocked() ([]SessionInfo, error) {
	summaries, err := c.a.Sessions()
	if err != nil {
		return nil, err
	}
	sessions := make([]SessionInfo, 0, len(summaries))
	for _, s := range summaries {
		sessions = append(sessions, SessionInfo{ID: s.ID, Title: s.Title, Model: s.Model, Updated: s.Mod})
	}
	return sessions, nil
}

// setupError explains what is missing before a message can start a turn.
func (c *Controller) setupError() error {
	s := c.a.Settings()
	if s.Provider != "" && s.Model != "" {
		return nil
	}
	hint := "No model selected. Choose a model with /settings (use /login to add more providers)."
	if len(s.Providers) == 0 {
		hint = "No provider configured. Use /login to add a provider, then choose a model with /settings."
	}
	return fail("setup_required", "%s", hint)
}

func (c *Controller) pendingImages() int {
	n := 0
	if c.active != nil {
		n += len(c.active.images)
		if c.active.steering != nil {
			for _, in := range c.active.steering.Pending() {
				n += len(in.Images)
			}
		}
	}
	return n
}

// redact removes credentials from text returned to a caller.
func redact(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

// providerSecrets lists every configured credential so error text can be scrubbed.
func providerSecrets(s agent.Settings, extra ...string) []string {
	secrets := append([]string(nil), extra...)
	for _, p := range s.Providers {
		secrets = append(secrets, p.APIKey, os.Getenv(p.APIKeyEnv))
	}
	return secrets
}

// ProjectConfig reports whether dest is a project-level file, where credentials should not be committed.
func (c *Controller) ProjectConfig(dest string) (project bool) {
	return false
}

// Sources lists the configuration files in effect, lowest priority first.
func (c *Controller) Sources() (sources []agent.Source) {
	c.view(func() { sources = slices.Clone(c.a.Sources) })
	return sources
}

func (c *Controller) startLocked(kind string, images []shell.Image) *operation {
	c.count++
	id := fmt.Sprintf("%s-%d", kind, c.count)
	ctx, cancel := context.WithCancel(context.Background())
	op := &operation{id: id, kind: kind, ctx: ctx, cancel: cancel, started: time.Now(), steering: &agent.Steering{}, images: images}
	c.active = op
	c.idle = make(chan struct{})
	return op
}
