package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Selection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
}

func (a *Agent) selection() *Selection {
	return &Selection{a.settings.Provider, a.settings.Model, a.settings.Effort}
}

func (a *Agent) SessionInfo() map[string]any { return a.SessionInfoFor(a.Records()) }

// SessionInfoFor describes the session using a display snapshot, so callers that
// must not read a.records while a turn runs can pass their own copy.
func (a *Agent) SessionInfoFor(records []Record) map[string]any {
	var size any
	for _, r := range records {
		if r.Meta != nil && r.Meta.RequestID != "" {
			size = nil
			if r.Meta.ContextInput != nil {
				size = *r.Meta.ContextInput
			}
		}
	}
	return map[string]any{
		"id":       strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl"),
		"cwd":      a.Shell.Dir,
		"model":    a.settings.Model,
		"provider": a.settings.Provider,
		"effort":   a.settings.Effort,
		"ctx_size": size,
	}
}

// SelectionConfig is the settings file.
func (a *Agent) SelectionConfig() string {
	return a.PrimaryConfig()
}

// Select updates the provider, model, and effort in settings.
func (a *Agent) Select(provider, model, effort string) error {
	prev := a.settings
	next := prev
	next.Provider, next.Model, next.Effort = provider, model, effort
	if err := validateSettings(next); err != nil {
		return err
	}
	a.settings = next
	if a.HasSavedSession() {
		if err := a.Save(); err != nil {
			a.settings = prev
			return err
		}
	}
	a.applySettings(next)
	return nil
}

// ModelChoice is a model offered by one configured provider.
type ModelChoice struct{ Provider, Model string }

// AllModels lists models from every configured provider concurrently.
func AllModels(ctx context.Context, providers map[string]Provider, environment ...map[string]string) (choices []ModelChoice, errs map[string]error) {
	type result struct {
		name   string
		models []string
		err    error
	}
	var env map[string]string
	if len(environment) > 0 {
		env = environment[0]
	}
	results := make(chan result, len(providers))
	for name, p := range providers {
		go func(name string, p Provider) {
			if p.APIKey == "" && p.APIKeyEnv != "" {
				if env != nil {
					p.APIKey = env[p.APIKeyEnv]
				} else {
					p.APIKey = os.Getenv(p.APIKeyEnv)
				}
			}
			client := newClient(p)
			page := client.Models.ListAutoPaging(ctx)
			var models []string
			for page.Next() {
				models = append(models, page.Current().ID)
			}
			results <- result{name, models, page.Err()}
		}(name, p)
	}
	errs = map[string]error{}
	for range providers {
		r := <-results
		if r.err != nil {
			errs[r.name] = r.err
			continue
		}
		for _, m := range r.models {
			choices = append(choices, ModelChoice{r.name, m})
		}
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i].Provider != choices[j].Provider {
			return choices[i].Provider < choices[j].Provider
		}
		return choices[i].Model < choices[j].Model
	})
	return choices, errs
}

// AddProvider registers a provider in the session.
func (a *Agent) AddProvider(name string, provider Provider) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if _, exists := a.settings.Providers[name]; exists {
		return fmt.Errorf("provider %q already exists", name)
	}
	next := a.Settings()
	if next.Providers == nil {
		next.Providers = map[string]Provider{}
	}
	next.Providers[name] = provider
	if err := validateSettings(next); err != nil {
		return err
	}
	a.applySettings(next)
	return nil
}

func (a *Agent) bashParameters() map[string]any {
	timeout, maxTimeout, maxOutput := a.settings.ShellTimeout, a.settings.ShellMaxTimeout, a.settings.ShellMaxOutput
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"timeout": map[string]any{
				"type":        []string{"integer", "null"},
				"description": fmt.Sprintf("seconds, default %d, maximum %d", timeout, maxTimeout),
			},
			"max_output": map[string]any{
				"type":        []string{"integer", "null"},
				"description": fmt.Sprintf("output bytes to keep, default %d (first half + last half); 0 = no truncation", maxOutput),
			},
		},
		"required":             []string{"command", "timeout", "max_output"},
		"additionalProperties": false,
	}
}

// shellEnv is exported into the shell before every call.
// Spec: exported to the agent's bash tool calls: DASHY_SESSION_ID, DASHY_ENDPOINT
// Never export provider/model/effort/ctx/cwd/state/event variables.
func (a *Agent) shellEnv() map[string]string {
	env := make(map[string]string, 2)
	if a.socketPath != "" {
		env["DASHY_ENDPOINT"] = a.socketPath
	}
	env["DASHY_SESSION_ID"] = a.sessionID()
	return env
}
