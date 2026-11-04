package control

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dashy/internal/agent"
)

// Input sends user input. An idle session starts a turn; a busy one queues it as steering.
func (c *Controller) Input(input agent.Input) error {
	var err error
	c.view(func() {
		switch {
		case c.shut:
			err = fail("closed", "the session is closed")
		case strings.TrimSpace(input.Text) == "" && len(input.Images) == 0:
			err = fail("invalid", "input requires text or images")
		case c.active != nil:
			err = c.queueLocked(input)
		default:
			if err = c.setupError(); err == nil {
				if err = c.admit(input); err == nil {
					c.noteImages(input)
					c.beginTurnLocked(input)
				}
			}
		}
	})
	return err
}

func (c *Controller) queueLocked(input agent.Input) error {
	if c.active.kind == "compact" {
		return fail("compacting", "Compaction is running; wait before sending.")
	}
	if c.closing || c.deferred != nil {
		return fail("cancelling", "Session is changing; wait for the change to complete.")
	}
	if err := c.admit(input); err != nil {
		return err
	}
	if !c.a.Queue().Push(input) {
		c.a.ReleaseImages(len(input.Images))
		return fail("queue_full", "Steering queue is full; your message remains in the input.")
	}
	c.noteImages(input)
	return nil
}

// admit reserves the images of input in the allowance of one model request.
func (c *Controller) admit(input agent.Input) error {
	if len(input.Images) > 0 && !c.a.ReserveImages(len(input.Images)) {
		return fail("attachment_limit", "At most %d new images per request. Remove or submit images first.", agent.MaxPendingImages)
	}
	return nil
}

func (c *Controller) noteImages(input agent.Input) {
	if n := len(input.Images); n > 0 {
		c.publish(Event{Kind: "notice", Text: map[bool]string{true: "Image", false: "Images"}[n == 1] + " added to next model request."})
	}
}

// Recall returns the newest instruction the agent has not accepted yet.
func (c *Controller) Recall() (*agent.Input, error) {
	var input agent.Input
	var ok bool
	c.view(func() {
		if c.active != nil {
			if input, ok = c.a.Queue().Recall(); ok {
				c.a.ReleaseImages(len(input.Images))
			}
		}
	})
	if !ok {
		return nil, fail("not_recallable", "That instruction was already accepted and cannot be recalled.")
	}
	return &input, nil
}

// Stop cancels the active operation. The cancellation completes later as a done event.
func (c *Controller) Stop() {
	c.view(func() {
		if c.active != nil {
			c.active.cancelled = true
			c.active.cancel()
		}
	})
}

// Quit closes the session now, or after the active operation has ended.
// closed reports whether the session closed immediately.
func (c *Controller) Quit() (closed bool, err error) {
	c.view(func() {
		if c.active != nil {
			c.deferred = &deferredAction{kind: "quit"}
			c.active.cancelled = true
			c.active.cancel()
			return
		}
		err = c.closeLocked()
		closed = err == nil
	})
	return closed, err
}

// NewSession switches to a new session once the active operation ends.
func (c *Controller) NewSession() error { return c.switchRequest("new", "") }

// Resume switches to a saved session of this project once the active operation ends.
func (c *Controller) Resume(sessionID string) error { return c.switchRequest("resume", sessionID) }

func (c *Controller) switchRequest(kind, id string) error {
	var err error
	c.view(func() {
		if kind == "resume" {
			var sessions []agent.SessionSummary
			if sessions, err = c.sessionsLocked(); err != nil {
				err = fail("failed", "%s", err)
				return
			}
			if !slices.ContainsFunc(sessions, func(s agent.SessionSummary) bool { return s.ID == id }) {
				err = fail("not_found", "session not found in this project: %s", id)
				return
			}
		}
		if c.active != nil {
			for _, in := range c.a.Queue().Pending() {
				if len(in.Images) > 0 {
					err = fail("pending_images", "Submit or remove pending images before switching sessions.")
					return
				}
			}
			c.deferred = &deferredAction{kind: kind, session: id}
			c.active.cancelled = true
			c.active.cancel()
			return
		}
		if err = c.switchLocked(kind, id); err != nil {
			err = fail("failed", "%s", err)
		}
	})
	return err
}

// Reload re-reads settings and AGENTS.md; a running turn is not interrupted, it applies once it ends.
func (c *Controller) Reload() {
	c.view(func() {
		if c.active != nil {
			c.reload = true
			c.publish(Event{Kind: "notice", Text: "Instructions will reload after the current turn."})
			return
		}
		c.reloadLocked()
	})
}

func (c *Controller) reloadLocked() {
	if err := c.a.Reload(); err != nil {
		c.publish(Event{Kind: "error", Text: err.Error()})
		return
	}
	c.publish(Event{Kind: "notice", Text: "Settings and AGENTS.md reloaded; the prompt is rebuilt before the next request."})
}

// Rename sets the title of the current session; an empty name restores the first user message.
func (c *Controller) Rename(title string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before renaming."); err != nil {
			return
		}
		if err = c.a.Rename(title); err != nil {
			err = fail("failed", "%s", err)
			return
		}
		c.custom = c.a.CustomTitle()
		text := "Session title reset to the first message."
		if c.custom != "" {
			text = "Session renamed: " + c.custom
		}
		c.publish(Event{Kind: "notice", Text: text})
	})
	return err
}

// Compact summarizes older turns, guided by optional prompt text.
func (c *Controller) Compact(prompt string) error {
	var err error
	c.view(func() {
		switch {
		case c.active != nil && c.active.kind == "compact":
			err = fail("busy", "Compaction is already running.")
		case c.active != nil:
			err = fail("busy", "Wait for the active turn before compacting.")
		default:
			if err = c.setupError(); err == nil {
				c.beginCompactLocked(strings.TrimSpace(prompt))
			}
		}
	})
	return err
}

// Instructions lists the AGENTS.md files of the session, in prompt order.
func (c *Controller) Instructions() (paths []string) {
	c.view(func() { paths = c.a.PromptFiles() })
	return paths
}

// Models lists models of all configured providers; failures maps provider names to redacted errors.
func (c *Controller) Models(ctx context.Context) (models []agent.ModelChoice, failures map[string]string, err error) {
	var settings agent.Settings
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err == nil && c.shut {
			err = fail("closed", "the session is closed")
		}
		settings = c.a.Settings()
	})
	if err != nil {
		return nil, nil, err
	}
	models, errs := agent.AllModels(ctx, settings.Providers)
	if ctx.Err() != nil {
		return nil, nil, fail("cancelled", "the model listing was cancelled")
	}
	failures = map[string]string{}
	for name, err := range errs {
		failures[name] = agent.Redact(settings, err.Error())
	}
	return models, failures, nil
}

// SelectModel sets the active provider, model and reasoning effort.
func (c *Controller) SelectModel(provider, model, effort string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		if provider == "" || model == "" {
			err = fail("invalid", "a model and its provider are required")
			return
		}
		err = c.selectLocked(provider, model, effort)
	})
	return err
}

// SelectProvider sets the active provider, keeping its model when the selection allows.
func (c *Controller) SelectProvider(name string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		s := c.a.Settings()
		err = c.selectLocked(name, s.Model, s.Effort)
	})
	return err
}

func (c *Controller) selectLocked(provider, model, effort string) error {
	if _, ok := c.a.Settings().Providers[provider]; !ok {
		return fail("not_found", "provider not found: %s", provider)
	}
	if err := c.a.Select(provider, model, effort); err != nil {
		return fail("failed", "%s", agent.Redact(c.a.Settings(), err.Error()))
	}
	c.publish(Event{Kind: "notice", Text: "Selected " + provider + " · " + model + "/" + effort + " (saved to " + c.a.SettingsPath() + ")"})
	return nil
}

// AddProvider configures a new provider. The API key is never reported back.
func (c *Controller) AddProvider(name, baseURL, apiKey string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		settings := c.a.Settings()
		if err = c.a.AddProvider(name, agent.Provider{BaseURL: baseURL, APIKey: apiKey}); err != nil {
			err = fail("failed", "%s", agent.Redact(settings, err.Error(), apiKey))
			return
		}
		c.publish(Event{Kind: "notice", Text: "Provider created in " + c.a.SettingsPath() + ". Choose a model with /settings."})
	})
	return err
}

// Settings rows that ChangeSetting toggles or cycles.
const (
	SettingReasoning   = "reasoning"
	SettingAutoCompact = "autocompact"
	SettingThreshold   = "threshold"
	SettingLines       = "lines"
)

// SettingLabel shows one setting row, as in /settings and in the notice after a change.
func SettingLabel(setting string, s agent.Settings) string {
	switch setting {
	case SettingReasoning:
		return "Reasoning summary: " + OnOff(s.ReasoningSummary)
	case SettingAutoCompact:
		return "Auto compact: " + OnOff(s.AutoCompact)
	case SettingThreshold:
		return fmt.Sprintf("Compact at: %dK tokens", s.CompactThreshold/1000)
	}
	return "Show lines: " + s.ShowLines
}

// ChangeSetting toggles or cycles one setting and saves it. It is rejected while an
// operation runs, so no turn ever sees settings change under it.
func (c *Controller) ChangeSetting(setting string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		if e := c.a.ChangeSettings(func(s *agent.Settings) { cycle(setting, s) }); e != nil {
			err = fail("failed", "%s", e)
			return
		}
		c.publish(Event{Kind: "notice", Text: SettingLabel(setting, c.a.Settings())})
	})
	return err
}

func cycle(setting string, s *agent.Settings) {
	switch setting {
	case SettingReasoning:
		s.ReasoningSummary = !s.ReasoningSummary
	case SettingAutoCompact:
		s.AutoCompact = !s.AutoCompact
	case SettingThreshold:
		i := slices.IndexFunc(agent.CompactThresholds, func(t int64) bool { return t > s.CompactThreshold })
		s.CompactThreshold = agent.CompactThresholds[max(i, 0)]
	case SettingLines:
		s.ShowLines = map[string]string{"short": "medium", "medium": "full", "full": "short"}[s.ShowLines]
	}
}

// OnOff is how settings show a boolean.
func OnOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
