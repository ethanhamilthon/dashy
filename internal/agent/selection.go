package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Select sets the provider, model and reasoning effort, and saves them.
func (a *Agent) Select(provider, model, effort string) error {
	return a.update(func(s *Settings) error {
		s.Provider, s.Model, s.Effort = provider, model, effort
		return nil
	})
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
			models, err := newClient(p).models(ctx)
			if err != nil && p.key() != "" {
				err = errors.New(strings.ReplaceAll(err.Error(), p.key(), "[redacted]"))
			}
			results <- result{name, models, err}
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
	return a.update(func(s *Settings) error {
		if _, exists := s.Providers[name]; exists {
			return fmt.Errorf("provider %q already exists", name)
		}
		s.Providers[name] = provider
		return nil
	})
}

// ChangeSettings applies change to a copy of the settings, then validates, applies and saves it.
func (a *Agent) ChangeSettings(change func(*Settings)) error {
	return a.update(func(s *Settings) error { change(s); return nil })
}
