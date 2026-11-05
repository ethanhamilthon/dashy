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
		case strings.TrimSpace(input.Text) == "":
			err = fail("invalid", "input requires text or images")
		case c.active != nil:
			err = c.queueLocked(input)
		default:
			if err = c.setupError(); err == nil {
				c.beginTurnLocked(input)
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

// Recall returns the newest instruction the agent has not accepted yet.
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

// Reload re-reads settings and DASHY.md; a running turn is not interrupted, it applies once it ends.
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

// Instructions lists the DASHY.md files of the session, in prompt order.
func (c *Controller) Instructions() (paths []string) {
	c.view(func() { paths = c.a.PromptFiles() })
	return paths
}

// Models lists models of all configured providers; failures maps provider names to redacted errors.
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

// SelectModel sets the active provider, model and reasoning effort.
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

// SelectProvider sets the active provider, keeping its model when the selection allows.
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
	c.publish(Event{Kind: "notice", Text: "Selected " + provider + " · " + model + "/" + effort + " (saved to " + c.a.SettingsPath() + ")"})
	return nil
}

// AddProvider configures a new provider. The API key is never reported back.
func (c *Controller) AddProvider(name, baseURL, apiKey string) error {
	var err error
	c.view(func() {
		settings := c.a.Settings()
		if err = c.a.AddProvider(name, agent.Provider{BaseURL: baseURL, APIKey: apiKey}); err != nil {
			err = fail("failed", "%s", agent.Redact(settings, err.Error(), apiKey))
			return
		}
		c.publish(Event{Kind: "notice", Text: "Provider created in " + c.a.SettingsPath() + ". Choose a model with /models."})
	})
	return err
}

// Settings rows that ChangeSetting toggles or cycles.
const (
	SettingReasoning   = "reasoning"
	SettingLines       = "lines"
	SettingShellOutput = "shelloutput"
)

// settingRow describes one /settings row: its label, its values with the index of the
// current one (-1 when the stored value is not one of them) and how a step changes it.
type settingRow struct {
	name    string
	options func(agent.Settings) (options []string, current int)
	step    func(delta int, s *agent.Settings)
}

var settingRows = map[string]settingRow{
	SettingReasoning: {"Reasoning summary",
		func(s agent.Settings) ([]string, int) { return onOff(s.ReasoningSummary) },
		func(delta int, s *agent.Settings) { s.ReasoningSummary = delta < 0 }},
	SettingLines: {"Show lines",
		func(s agent.Settings) ([]string, int) {
			return agent.ShowLinesModes, slices.Index(agent.ShowLinesModes, s.ShowLines)
		},
		func(delta int, s *agent.Settings) {
			i := max(slices.Index(agent.ShowLinesModes, s.ShowLines), 0)
			if delta > 0 {
				i++
			} else {
				i--
			}
			s.ShowLines = agent.ShowLinesModes[min(max(i, 0), len(agent.ShowLinesModes)-1)]
		}},
	SettingShellOutput: {"Bash output",
		func(s agent.Settings) (options []string, current int) {
			current = -1
			for v := agent.ShellOutputMin; v <= agent.ShellOutputMax; v *= 2 {
				if v == s.ShellOutput {
					current = len(options)
				}
				options = append(options, fmt.Sprintf("%dK", v/1024))
			}
			return options, current
		},
		func(delta int, s *agent.Settings) {
			// Doubling steps; from a value off the grid, a step goes to the next grid value in
			// that direction. 0 (no truncation) is only reachable by editing settings.json.
			v := s.ShellOutput
			switch {
			case v == 0 && delta > 0:
				return
			case v == 0:
				v = agent.ShellOutputMax
			case delta > 0:
				n := agent.ShellOutputMin
				for n <= v && n < agent.ShellOutputMax {
					n *= 2
				}
				v = n
			default:
				n := agent.ShellOutputMax
				for n >= v && n > agent.ShellOutputMin {
					n /= 2
				}
				v = n
			}
			s.ShellOutput = v
		}},
}

func onOff(v bool) ([]string, int) { return []string{"on", "off"}, map[bool]int{true: 0, false: 1}[v] }

// SettingOptions lists every value of a setting row and the index of the current one
// (-1 when the stored value is not one of them).
func SettingOptions(setting string, s agent.Settings) (options []string, current int) {
	return settingRows[setting].options(s)
}

// SettingName is the name of a setting row.
func SettingName(setting string) string { return settingRows[setting].name }

// ChangeSetting moves one setting a step back (delta < 0) or forward (delta > 0) and saves it.
// Settings stop at their first and last value. It is allowed while a turn runs: that turn's
// next model request picks the change up.
func (c *Controller) ChangeSetting(setting string, delta int) error {
	row, ok := settingRows[setting]
	if !ok {
		return fail("invalid", "unknown setting: %s", setting)
	}
	var err error
	c.view(func() {
		if e := c.a.ChangeSettings(func(s *agent.Settings) { row.step(delta, s) }); e != nil {
			err = fail("failed", "%s", e)
		}
	})
	return err
}
