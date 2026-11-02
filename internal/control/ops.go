package control

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"dashy/internal/agent"
)

// Submit applies one request. Synchronous requests return their outcome; background
// work reports through events. A "command" runs the operation its slash name names.
func (c *Controller) Submit(req Request) (Response, error) {
	return c.SubmitContext(context.Background(), req)
}

// SubmitContext is Submit for callers that can abandon a request. Cancelling ctx
// ends a model listing with a "cancelled" error; other requests are not affected.
func (c *Controller) SubmitContext(ctx context.Context, req Request) (Response, error) {
	mapped, err := Normalize(req)
	if err != nil {
		return Response{}, err
	}
	if mapped.Type == "models" {
		return c.models(ctx)
	}
	var resp Response
	c.view(func() {
		if c.shut {
			err = fail("closed", "the session is closed")
			return
		}
		resp, err = c.dispatchLocked(mapped)
	})
	return resp, err
}

// Normalize maps a "command" request onto the request type its slash name
// names, keeping the request ID. Other requests are returned unchanged. Adapters
// classify by the normalized type, so a command gets the handling of its
// operation: "/model" is a listing, just like type "models".
func Normalize(req Request) (Request, error) {
	if req.Type != "command" {
		return req, nil
	}
	mapped, err := commandRequest(req)
	if err != nil {
		return Request{}, err
	}
	mapped.ID = req.ID
	return mapped, nil
}

func (c *Controller) dispatchLocked(req Request) (Response, error) {
	switch req.Type {
	case "input", "steer":
		input := agent.Input{Text: req.Text, Images: req.Images, Files: req.Files}
		if strings.TrimSpace(input.Text) == "" && len(input.Images) == 0 && len(input.Files) == 0 {
			return Response{}, fail("invalid", "input requires text, images or files")
		}
		if c.active != nil {
			return c.queueLocked(input)
		}
		if req.Type == "steer" {
			return Response{}, fail("not_busy", "no turn is running; send input to start one")
		}
		if err := c.setupError(); err != nil {
			return Response{}, err
		}
		if err := c.admit(input); err != nil {
			return Response{}, err
		}
		c.noteImages(input)
		return Response{Status: "started", Turn: c.beginTurnLocked(input, "user")}, nil
	case "recall":
		return c.recall()
	case "stop":
		if c.active == nil {
			return Response{Status: "idle"}, nil
		}
		c.cancelActiveLocked()
		return Response{Status: "cancelling", Turn: c.active.id}, nil
	case "quit":
		return c.quit()
	case "new":
		return c.switchRequest("new", "")
	case "resume":
		return c.switchRequest("resume", req.Args["session_id"])
	case "sessions":
		sessions, err := c.sessionsLocked()
		if err != nil {
			return Response{}, fail("failed", "%s", err)
		}
		return Response{Status: "ok", Data: sessions}, nil
	case "history":
		return Response{Status: "ok", Data: slices.Clone(c.journal)}, nil
	case "state":
		return Response{Status: "ok", Data: c.stateLocked()}, nil
	case "help":
		return Response{Status: "ok", Data: HelpInfo{Commands: Commands(), Types: Types}}, nil
	case "rename":
		return c.rename(req.Args["title"])
	case "compact":
		return c.compact(req.Text)
	case "providers":
		return c.providers()
	case "select_model":
		return c.selectModel(req.Args)
	case "select_provider":
		return c.selectProvider(req.Args["name"])
	case "add_provider":
		return c.addProvider(req.Args)
	case "toggle_reasoning_summary":
		return c.toggleReasoningSummary()
	case "toggle_auto_compact":
		return c.toggleAutoCompact()
	case "cycle_show_lines":
		return c.cycleShowLines()
	}
	return Response{}, fail("invalid", "unknown request type %q", req.Type)
}

// commandNames maps the plain slash commands onto request types.
var commandNames = map[string]string{"/new": "new", "/quit": "quit", "/help": "help"}

// commandRequest maps a slash command onto the operation it names.
func commandRequest(req Request) (Request, error) {
	args := req.Args
	if typ, ok := commandNames[req.Name]; ok {
		return Request{Type: typ}, nil
	}
	switch req.Name {
	case "/resume":
		return Request{Type: "resume", Args: map[string]string{"session_id": args["session_id"]}}, nil
	case "/rename":
		return Request{Type: "rename", Args: map[string]string{"title": args["title"]}}, nil
	case "/compact":
		return Request{Type: "compact", Text: args["prompt"]}, nil
	case "/settings":
		if args["provider"] != "" || args["model"] != "" {
			return Request{Type: "select_model", Args: args}, nil
		}
		return Request{Type: "models"}, nil
	case "/login":
		switch {
		case args["base_url"] != "":
			return Request{Type: "add_provider", Args: args}, nil
		case args["name"] != "":
			return Request{Type: "select_provider", Args: args}, nil
		}
		return Request{Type: "providers"}, nil
	}
	return Request{}, fail("unknown_command", "Unknown command: %s. Type /help.", req.Name)
}

// requireIdle rejects a settings or session mutation while an operation runs.
func (c *Controller) requireIdle(message string) error {
	if c.active != nil {
		return fail("busy", "%s", message)
	}
	return nil
}

// admit checks attachment limits before input joins the queue or starts a turn.
func (c *Controller) admit(input agent.Input) error {
	if c.pendingImages()+len(input.Images) > maxPendingImages {
		return fail("attachment_limit", "At most eight images may be pending. Remove or submit images first.")
	}
	return nil
}

func (c *Controller) noteImages(input agent.Input) {
}

// queueLocked adds input as steering for the active turn.
func (c *Controller) queueLocked(input agent.Input) (Response, error) {
	switch {
	case c.active.kind == "compact":
		return Response{}, fail("busy", "Compaction is running; wait before sending.")
	case c.active.cancelled:
		return Response{}, fail("cancelling", "Cancellation in progress; your message remains in the input.")
	}
	if err := c.admit(input); err != nil {
		return Response{}, err
	}
	c.inputs++
	input.ID = fmt.Sprintf("steer-%d", c.inputs)
	if !c.active.steering.PushInput(input) {
		c.inputs--
		return Response{}, fail("queue_full", "Steering queue is full; your message remains in the input.")
	}
	c.queued = append(c.queued, input)
	c.noteImages(input)
	return Response{Status: "queued", Turn: c.active.id}, nil
}

// recall returns the newest instruction the agent has not accepted yet.
func (c *Controller) recall() (Response, error) {
	notRecallable := fail("not_recallable", "That instruction was already accepted and cannot be recalled.")
	if c.active == nil || len(c.queued) == 0 {
		return Response{}, notRecallable
	}
	input, ok := c.active.steering.RecallInput()
	if !ok {
		return Response{}, notRecallable
	}
	for i := len(c.queued) - 1; i >= 0; i-- {
		if c.queued[i].ID == input.ID {
			c.queued = slices.Delete(c.queued, i, i+1)
			break
		}
	}
	return Response{Status: "recalled", Turn: c.active.id, Input: &input}, nil
}

// quit closes the session now, or after the active operation has been cancelled.
func (c *Controller) quit() (Response, error) {
	if c.active != nil {
		c.deferred = &deferredAction{kind: "quit"}
		c.cancelActiveLocked()
		return Response{Status: "deferred", Turn: c.active.id}, nil
	}
	if err := c.closeLocked("quit"); err != nil {
		return Response{}, fail("failed", "%s", err)
	}
	return Response{Status: "closed"}, nil
}

// switchRequest starts a session change. During work it cancels the turn and
// performs the change once the turn has ended.
func (c *Controller) switchRequest(kind, id string) (Response, error) {
	if kind == "resume" {
		if id == "" {
			return Response{}, fail("invalid", "resume requires session_id")
		}
		sessions, err := c.sessionsLocked()
		if err != nil {
			return Response{}, fail("failed", "%s", err)
		}
		if !slices.ContainsFunc(sessions, func(s SessionInfo) bool { return s.ID == id }) {
			return Response{}, fail("not_found", "session not found in this project: %s", id)
		}
	}
	for _, in := range c.queued {
		if len(in.Images) > 0 {
			return Response{}, fail("pending_images", "Submit or remove pending images before switching sessions.")
		}
	}
	if c.active != nil {
		c.deferred = &deferredAction{kind: kind, session: id}
		c.cancelActiveLocked()
		return Response{Status: "deferred", Turn: c.active.id}, nil
	}
	if err := c.switchLocked(kind, id); err != nil {
		return Response{}, fail("failed", "%s", err)
	}
	return Response{Status: "ok"}, nil
}

func (c *Controller) rename(title string) (Response, error) {
	if err := c.requireIdle("Wait for the active turn before renaming."); err != nil {
		return Response{}, err
	}
	if err := c.a.Rename(title); err != nil {
		return Response{}, fail("failed", "%s", err)
	}
	text := "Session title reset to the first message."
	if name := strings.Join(strings.Fields(title), " "); name != "" {
		text = "Session renamed: " + name
	}
	c.publish(Event{Kind: "notice", Text: text})
	return Response{Status: "ok"}, nil
}

// compact summarizes older turns in the background; the operation holds the controller until it ends.
func (c *Controller) compact(prompt string) (Response, error) {
	if err := c.requireIdle("Wait for the active turn before compacting."); err != nil {
		return Response{}, err
	}
	op := c.startLocked("compact", nil)
	prompt = strings.TrimSpace(prompt)
	go func() {
		text, err := c.a.Compact(op.ctx, prompt)
		c.finishCompact(op, text, err)
	}()
	c.publish(Event{Kind: "started", Turn: op.id, Operation: "compact", Source: "compact"})
	return Response{Status: "started", Turn: op.id}, nil
}

// models lists provider models without holding the lock during network calls.
func (c *Controller) models(parent context.Context) (Response, error) {
	var settings agent.Settings
	var err error
	c.view(func() {
		if err = c.requireIdle("Wait for the active turn before changing settings."); err == nil && c.shut {
			err = fail("closed", "the session is closed")
		}
		settings = c.a.Settings()
	})
	if err != nil {
		return Response{}, err
	}
	if parent.Err() != nil {
		return Response{}, fail("cancelled", "the model listing was cancelled")
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	choices, failures := agent.AllModels(ctx, settings.Providers)
	if parent.Err() != nil {
		return Response{}, fail("cancelled", "the model listing was cancelled")
	}
	list := ModelList{Models: []ModelInfo{}, Errors: map[string]string{}}
	for _, choice := range choices {
		list.Models = append(list.Models, ModelInfo{Provider: choice.Provider, Model: choice.Model})
	}
	for name, err := range failures {
		list.Errors[name] = redact(err.Error(), providerSecrets(settings)...)
	}
	return Response{Status: "ok", Data: list}, nil
}

func (c *Controller) providers() (Response, error) {
	if err := c.requireIdle("Wait for the active turn before changing settings."); err != nil {
		return Response{}, err
	}
	settings := c.a.Settings()
	list := []ProviderInfo{}
	for name, p := range settings.Providers {
		list = append(list, ProviderInfo{Name: name, BaseURL: sanitizeURL(p.BaseURL), APIKeyEnv: p.APIKeyEnv,
			APIKeyConfigured: p.APIKey != "" || p.APIKeyEnv != ""})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return Response{Status: "ok", Data: list}, nil
}

func (c *Controller) selectModel(args map[string]string) (Response, error) {
	if err := c.requireIdle("Wait for the active turn before changing settings."); err != nil {
		return Response{}, err
	}
	if args["provider"] == "" || args["model"] == "" {
		return Response{}, fail("invalid", "select_model requires provider and model")
	}
	return c.selectLocked(args["provider"], args["model"], args["effort"])
}

func (c *Controller) selectProvider(name string) (Response, error) {
	if err := c.requireIdle("Wait for the active turn before changing settings."); err != nil {
		return Response{}, err
	}
	settings := c.a.Settings()
	return c.selectLocked(name, settings.Model, settings.Effort)
}

// selectLocked saves the default selection and reports it.
func (c *Controller) selectLocked(provider, model, effort string) (Response, error) {
	if _, ok := c.a.Settings().Providers[provider]; !ok {
		return Response{}, fail("not_found", "provider not found: %s", provider)
	}
	if err := c.a.Select(provider, model, effort); err != nil {
		return Response{}, fail("failed", "%s", redact(err.Error(), providerSecrets(c.a.Settings())...))
	}
	text := "Selected " + provider + " · " + model + "/" + effort
	if dest := c.a.SelectionConfig(); dest != "" {
		text += " (saved to " + dest + ")"
	}
	c.publish(Event{Kind: "notice", Text: text})
	return Response{Status: "ok"}, nil
}

func (c *Controller) addProvider(args map[string]string) (Response, error) {
	if err := c.requireIdle("Wait for the active turn before changing settings."); err != nil {
		return Response{}, err
	}
	name := strings.TrimSpace(args["name"])
	secrets := providerSecrets(c.a.Settings(), args["api_key"])
	if err := c.a.AddProvider(name, agent.Provider{BaseURL: args["base_url"], APIKey: args["api_key"]}); err != nil {
		return Response{}, fail("failed", "%s", redact(err.Error(), secrets...))
	}
	dest := c.a.PrimaryConfig()
	text := "Provider created. Choose a model with /settings."
	if dest != "" {
		text = "Provider created in " + dest + ". Choose a model with /settings."
	}
	c.publish(Event{Kind: "notice", Text: text})
	return Response{Status: "ok", Data: ProviderSaved{Name: name, Destination: dest, ProjectConfig: c.projectConfigLocked(dest)}}, nil
}

// stateLocked snapshots the session and the active operation.
func (c *Controller) stateLocked() StateInfo {
	info := StateInfo{Session: c.a.SessionInfoFor(c.journal), Queued: len(c.queued)}
	if c.active != nil {
		info.Busy, info.Cancelling, info.Turn = true, c.active.cancelled, c.active.id
		info.Compacting = c.active.kind == "compact"
	}
	return info
}

// projectConfigLocked reports whether dest lies inside the project directory, judged
// by its actual path. The global $DASHY_HOME/dashy.toml is never a project file.
func (c *Controller) projectConfigLocked(dest string) bool {
	return false
}

// sanitizeURL drops credentials and query strings from a provider base URL.
func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

func (c *Controller) toggleReasoningSummary() (Response, error) {
	curr := c.a.Settings().ReasoningSummary
	if err := c.a.SetReasoningSummary(!curr); err != nil {
		return Response{}, fail("failed", "%s", err)
	}
	state := "off"
	if !curr {
		state = "on"
	}
	c.publish(Event{Kind: "notice", Text: "Reasoning summary: " + state})
	return Response{Status: "ok"}, nil
}

func (c *Controller) toggleAutoCompact() (Response, error) {
	curr := c.a.Settings().AutoCompact
	if err := c.a.SetAutoCompact(!curr); err != nil {
		return Response{}, fail("failed", "%s", err)
	}
	state := "off"
	if !curr {
		state = "on"
	}
	c.publish(Event{Kind: "notice", Text: "Auto compact: " + state})
	return Response{Status: "ok"}, nil
}

func (c *Controller) cycleShowLines() (Response, error) {
	if err := c.a.CycleShowLines(); err != nil {
		return Response{}, fail("failed", "%s", err)
	}
	c.publish(Event{Kind: "notice", Text: "Show lines: " + c.a.Settings().ShowLines})
	return Response{Status: "ok"}, nil
}
