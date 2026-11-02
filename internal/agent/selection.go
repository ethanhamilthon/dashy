package agent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Select sets the provider, model and reasoning effort, and saves them.
func (a *Agent) Select(provider, model, effort string) error {
	next := a.Settings()
	next.Provider, next.Model, next.Effort = provider, model, effort
	return a.applyAndSave(next)
}

// ModelChoice is a model offered by one configured provider.
type ModelChoice struct{ Provider, Model string }

// AllModels lists the models of every provider concurrently. Failures are reported per provider.
func AllModels(ctx context.Context, providers map[string]Provider) (choices []ModelChoice, errs map[string]error) {
	type result struct {
		name   string
		models []string
		err    error
	}
	results := make(chan result, len(providers))
	for name, p := range providers {
		go func(name string, p Provider) {
			if p.APIKey == "" && p.APIKeyEnv != "" {
				p.APIKey = os.Getenv(p.APIKeyEnv)
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

// AddProvider registers a new provider and saves the settings.
func (a *Agent) AddProvider(name string, provider Provider) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if _, exists := a.settings.Providers[name]; exists {
		return fmt.Errorf("provider %q already exists", name)
	}
	next := a.Settings()
	next.Providers[name] = provider
	return a.applyAndSave(next)
}

// SetReasoningSummary, SetAutoCompact and CycleShowLines change one display or behaviour setting and save it.
func (a *Agent) SetReasoningSummary(enabled bool) error {
	next := a.Settings()
	next.ReasoningSummary = enabled
	return a.applyAndSave(next)
}

func (a *Agent) SetAutoCompact(enabled bool) error {
	next := a.Settings()
	next.AutoCompact = enabled
	return a.applyAndSave(next)
}

func (a *Agent) CycleShowLines() error {
	next := a.Settings()
	next.ShowLines = map[string]string{"short": "medium", "medium": "full"}[next.ShowLines]
	if next.ShowLines == "" {
		next.ShowLines = "short"
	}
	return a.applyAndSave(next)
}
