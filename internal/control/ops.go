package control

import (
	"context"
	"slices"
	"strings"

	"dashy/internal/agent"
)

func (c *Controller) Input(input agent.Input) error {
	var err error
	c.view(func() {
		switch {
		case c.shut:
			err = fail("closed", "the session is closed")
		case strings.TrimSpace(input.Text) == "":
			err = fail("invalid", "input requires text or images")
		case c.active != nil:
			err = c.queueLocked(input)
		default:
			if err = c.setupError(); err == nil {
				if err = c.queueLocked(input); err == nil {
					c.deliverLocked()
				}
			}
		}
	})
	return err
}

func (c *Controller) queueLocked(input agent.Input) error {
	if c.closing || c.deferred != nil {
		return fail("cancelling", "Session is changing; wait for the change to complete.")
	}
	if !c.a.Queue().Push(input) {
		return fail("queue_full", "Steering queue is full; your message remains in the input.")
	}
	return nil
}

func (c *Controller) Recall() (*agent.Input, error) {
	var input agent.Input
	var ok bool
	c.view(func() {
		if c.active != nil {
			input, ok = c.a.Queue().Recall()
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

func (c *Controller) NewSession() error { return c.switchRequest("new", "") }

func (c *Controller) Resume(sessionID string) error { return c.switchRequest("resume", sessionID) }

func (c *Controller) switchRequest(kind, id string) error {
	var err error
	c.view(func() {
		if kind == "resume" {
			var sessions []agent.SessionSummary
			if sessions, err = c.a.Sessions(); err != nil {
				err = fail("failed", "%s", err)
				return
			}
			if !slices.ContainsFunc(sessions, func(s agent.SessionSummary) bool { return s.ID == id }) {
				err = fail("not_found", "session not found in this project: %s", id)
				return
			}
		}
		if c.active != nil {
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
	c.publish(Event{Kind: "notice", Text: "Settings and DASHY.md reloaded; the prompt is rebuilt before the next request."})
}

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

func (c *Controller) Instructions() (paths []string) {
	c.view(func() { paths = c.a.PromptFiles() })
	return paths
}

func (c *Controller) Models(ctx context.Context) (models []agent.ModelChoice, failures map[string]string, err error) {
	var settings agent.Settings
	c.view(func() {
		if c.shut {
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

func (c *Controller) SelectModel(provider, model, effort string) error {
	var err error
	c.view(func() {
		if provider == "" || model == "" {
			err = fail("invalid", "a model and its provider are required")
			return
		}
		err = c.selectLocked(provider, model, effort)
	})
	return err
}

func (c *Controller) SelectProvider(name string) error {
	var err error
	c.view(func() {
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
	c.publish(Event{Kind: "notice", Text: "Selected " + provider + " · " + model + "/" + effort + "; saved to " + c.a.DBPath() + "."})
	return nil
}

func (c *Controller) SaveProvider(old, name, baseURL, apiKey string) error {
	var err error
	c.view(func() {
		settings := c.a.Settings()
		if err = c.a.SaveProvider(old, name, agent.Provider{BaseURL: strings.TrimSpace(baseURL), APIKey: strings.TrimSpace(apiKey)}); err != nil {
			err = fail("failed", "%s", agent.Redact(settings, err.Error(), apiKey))
			return
		}
		verb := "created"
		if old != "" {
			verb = "saved"
		}
		c.publish(Event{Kind: "notice", Text: "Provider " + verb + ": " + strings.TrimSpace(name) + "."})
	})
	return err
}

func (c *Controller) DeleteProvider(name string) error {
	var err error
	c.view(func() {
		if err = c.a.DeleteProvider(name); err != nil {
			err = fail("failed", "%s", err.Error())
			return
		}
		c.publish(Event{Kind: "notice", Text: "Provider deleted: " + name + "."})
	})
	return err
}

func (c *Controller) ProviderModels(ctx context.Context, old, baseURL, apiKey string) ([]string, error) {
	var p agent.Provider
	var settings agent.Settings
	c.view(func() {
		settings = c.a.Settings()
		p = settings.Providers[old]
	})
	p.BaseURL = strings.TrimSpace(baseURL)
	if key := strings.TrimSpace(apiKey); key != "" {
		p.APIKey, p.APIKeyEnv = key, ""
	}
	models, err := agent.ProviderModels(ctx, p)
	if err != nil {
		return nil, fail("failed", "%s", agent.Redact(settings, err.Error(), apiKey))
	}
	return models, nil
}

func (c *Controller) Tasks() (tasks []agent.Task, err error) {
	c.view(func() { tasks, err = c.a.Tasks() })
	return tasks, err
}

func (c *Controller) StopTask(id string) error {
	var err error
	c.view(func() {
		if err = c.a.StopTask(id); err != nil {
			err = fail("invalid", "%s", err.Error())
		}
	})
	return err
}

func (c *Controller) ChangeSettings(change func(*agent.Settings)) error {
	var err error
	c.view(func() {
		if e := c.a.ChangeSettings(change); e != nil {
			err = fail("failed", "%s", e)
		}
	})
	return err
}
