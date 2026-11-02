package control

import (
	"context"
	"net/url"
	"slices"
	"sort"
	"strings"

	"dashy/internal/agent"
)

// Input sends user input. An idle session starts a turn; a busy one receives
// it as steering.
func (c *Controller) Input(input agent.Input) error {
	var err error
	c.view(func() {
		if c.shut {
			err = fail("closed", "the session is closed")
			return
		}
		if strings.TrimSpace(input.Text) == "" && len(input.Images) == 0 {
			err = fail("invalid", "input requires text or images")
			return
		}
		if c.active != nil {
			err = c.queueLocked(input)
			return
		}
		if err = c.setupError(); err != nil {
			return
		}
		if err = c.admit(input); err != nil {
			return
		}
		c.noteImages(input)
		c.beginTurnLocked(input, "user")
	})
	return err
}

// Steer adds input as steering for the active turn. It returns an error if no
// turn is active.
func (c *Controller) Steer(input agent.Input) error {
	var err error
	c.view(func() {
		if c.shut {
			err = fail("closed", "the session is closed")
			return
		}
		if strings.TrimSpace(input.Text) == "" && len(input.Images) == 0 {
			err = fail("invalid", "input requires text or images")
			return
		}
		if c.active == nil {
			err = fail("not_busy", "no turn is running; send input to start one")
			return
		}
		err = c.queueLocked(input)
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
	if !c.active.steering.PushInput(input) {
		return fail("queue_full", "Steering queue is full; your message remains in the input.")
	}
	c.noteImages(input)
	return nil
}

// Recall returns the newest instruction the agent has not accepted yet.
func (c *Controller) Recall() (*agent.Input, error) {
	var input agent.Input
	var ok bool
	c.view(func() {
		if c.active == nil || c.active.steering == nil {
			return
		}
		input, ok = c.active.steering.RecallInput()
	})
	if !ok {
		return nil, fail("not_recallable", "That instruction was already accepted and cannot be recalled.")
	}
	return &input, nil
}

// Stop stops the active operation.
func (c *Controller) Stop() {
	c.view(func() {
		if c.active != nil {
			c.cancelActiveLocked()
		}
	})
}

// Quit closes the session now, or after the active operation has ended.
// closed reports whether the session closed immediately.
func (c *Controller) Quit() (bool, error) {
	var closed bool
	var err error
	c.view(func() {
		if c.active != nil {
			c.deferred = &deferredAction{kind: "quit"}
			c.cancelActiveLocked()
			closed = false
			return
		}
		if err = c.closeLocked("quit"); err == nil {
			closed = true
		}
	})
	return closed, err
}

// NewSession switches to a new session.
func (c *Controller) NewSession() error {
	var err error
	c.view(func() {
		err = c.switchRequest("new", "")
	})
	return err
}

// Resume switches to the saved session with the given ID.
func (c *Controller) Resume(sessionID string) error {
	var err error
	c.view(func() {
		err = c.switchRequest("resume", sessionID)
	})
	return err
}

func (c *Controller) switchRequest(kind, id string) error {
	if kind == "resume" {
		if id == "" {
			return fail("invalid", "resume requires session_id")
		}
		sessions, err := c.sessionsLocked()
		if err != nil {
			return fail("failed", "%s", err)
		}
		if !slices.ContainsFunc(sessions, func(s SessionInfo) bool { return s.ID == id }) {
			return fail("not_found", "session not found in this project: %s", id)
		}
	}
	if c.pendingImages() > 0 && c.active != nil {
		for _, in := range c.active.steering.Pending() {
			if len(in.Images) > 0 {
				return fail("pending_images", "Submit or remove pending images before switching sessions.")
			}
		}
	}
	if c.active != nil {
		c.deferred = &deferredAction{kind: kind, session: id}
		c.cancelActiveLocked()
		return nil
	}
	if err := c.switchLocked(kind, id); err != nil {
		return fail("failed", "%s", err)
	}
	return nil
}

// Rename sets the title of the current session.
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
		text := "Session title reset to the first message."
		if name := strings.Join(strings.Fields(title), " "); name != "" {
			text = "Session renamed: " + name
		}
		c.publish(Event{Kind: "notice", Text: text})
	})
	return err
}

// Compact summarizes older turns, guided by optional prompt text.
func (c *Controller) Compact(prompt string) error {
	var err error
	c.view(func() {
		if c.active != nil {
			hint := "Wait for the active turn before compacting."
			if c.active.kind == "compact" {
				hint = "Compaction is already running."
			}
			err = fail("busy", "%s", hint)
			return
		}
		if err = c.setupError(); err != nil {
			return
		}
		c.beginCompactLocked(prompt)
	})
	return err
}

func (c *Controller) runCompact(op *operation, prompt string) {
	text, err := c.a.Compact(op.ctx, strings.TrimSpace(prompt))
	c.finishCompact(op, text, err)
}

// Models fetches model lists from all configured providers.
func (c *Controller) Models(ctx context.Context) (ModelList, error) {
	var settings agent.Settings
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err == nil && c.shut {
			err = fail("closed", "the session is closed")
		}
		settings = c.a.Settings()
	})
	if err != nil {
		return ModelList{}, err
	}
	choices, failures := agent.AllModels(ctx, settings.Providers)
	if ctx.Err() != nil {
		return ModelList{}, fail("cancelled", "the model listing was cancelled")
	}
	list := ModelList{Models: []ModelInfo{}, Errors: map[string]string{}}
	for _, choice := range choices {
		list.Models = append(list.Models, ModelInfo{Provider: choice.Provider, Model: choice.Model})
	}
	for name, err := range failures {
		list.Errors[name] = redact(err.Error(), providerSecrets(settings)...)
	}
	return list, nil
}

// Providers returns descriptions of configured providers.
func (c *Controller) Providers() ([]ProviderInfo, error) {
	var list []ProviderInfo
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		settings := c.a.Settings()
		list = []ProviderInfo{}
		for name, p := range settings.Providers {
			list = append(list, ProviderInfo{
				Name:             name,
				BaseURL:          sanitizeURL(p.BaseURL),
				APIKeyEnv:        p.APIKeyEnv,
				APIKeyConfigured: p.APIKey != "" || p.APIKeyEnv != "",
			})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	})
	return list, err
}

// SelectModel sets the active provider, model, and reasoning effort.
func (c *Controller) SelectModel(provider, model, effort string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		if provider == "" || model == "" {
			err = fail("invalid", "select_model requires provider and model")
			return
		}
		err = c.selectLocked(provider, model, effort)
	})
	return err
}

// SelectProvider sets the active provider.
func (c *Controller) SelectProvider(name string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		settings := c.a.Settings()
		err = c.selectLocked(name, settings.Model, settings.Effort)
	})
	return err
}

func (c *Controller) selectLocked(provider, model, effort string) error {
	if _, ok := c.a.Settings().Providers[provider]; !ok {
		return fail("not_found", "provider not found: %s", provider)
	}
	if err := c.a.Select(provider, model, effort); err != nil {
		return fail("failed", "%s", redact(err.Error(), providerSecrets(c.a.Settings())...))
	}
	text := "Selected " + provider + " · " + model + "/" + effort
	if dest := c.a.SelectionConfig(); dest != "" {
		text += " (saved to " + dest + ")"
	}
	c.publish(Event{Kind: "notice", Text: text})
	return nil
}

// AddProvider configures a new provider.
func (c *Controller) AddProvider(name, baseURL, apiKey string) error {
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err != nil {
			return
		}
		trimmed := strings.TrimSpace(name)
		secrets := providerSecrets(c.a.Settings(), apiKey)
		if err = c.a.AddProvider(trimmed, agent.Provider{BaseURL: baseURL, APIKey: apiKey}); err != nil {
			err = fail("failed", "%s", redact(err.Error(), secrets...))
			return
		}
		dest := c.a.PrimaryConfig()
		text := "Provider created. Choose a model with /settings."
		if dest != "" {
			text = "Provider created in " + dest + ". Choose a model with /settings."
		}
		c.publish(Event{Kind: "notice", Text: text})
	})
	return err
}

func (c *Controller) requireIdle(msg string) error {
	if c.active != nil {
		return fail("busy", "%s", msg)
	}
	return nil
}

func (c *Controller) admit(input agent.Input) error {
	if len(input.Images) > 0 && c.pendingImages()+len(input.Images) > maxPendingImages {
		return fail("attachment_limit", "At most %d images may be pending. Remove or submit images first.", maxPendingImages)
	}
	return nil
}

func (c *Controller) noteImages(input agent.Input) {
	if len(input.Images) == 0 {
		return
	}
	text := "Image added to next model request."
	if len(input.Images) > 1 {
		text = "Images added to next model request."
	}
	c.publish(Event{Kind: "notice", Text: text})
}

func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}
