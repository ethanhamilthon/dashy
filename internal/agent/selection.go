package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Select sets the provider, model and reasoning effort of this session and makes
// them the default for new sessions. Other open sessions keep theirs.
func (a *Agent) Select(provider, model, effort string) error {
	a.cfg.mu.Lock()
	previous := a.cfg.sel
	a.cfg.sel = selection{provider, model, effort}
	a.cfg.mu.Unlock()
	err := a.update(func(s *Settings) error {
		s.Provider, s.Model, s.Effort = provider, model, effort
		return nil
	})
	if err != nil {
		a.cfg.mu.Lock()
		a.cfg.sel = previous
		a.cfg.mu.Unlock()
		return err
	}
	// Only the selection columns: a turn may be saving the history at the same time.
	_, err = a.db.Exec(`UPDATE sessions SET provider = ?, model = ?, effort = ? WHERE id = ?`, provider, model, effort, a.id)
	return err
}

// ModelChoice is a model offered by one configured provider.
type ModelChoice struct {
	Provider, Model string
	ContextWindow   int64 // Zero means the provider did not report a limit.
}

// AllModels lists the models of every provider concurrently. Failures are reported per provider.
func AllModels(ctx context.Context, providers map[string]Provider) (choices []ModelChoice, errs map[string]error) {
	type result struct {
		name   string
		models []modelInfo
		err    error
	}
	results := make(chan result, len(providers))
	for name, p := range providers {
		go func(name string, p Provider) {
			models, err := newClient(p).modelCatalog(ctx)
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
			choices = append(choices, ModelChoice{Provider: r.name, Model: m.ID, ContextWindow: m.window()})
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
	return a.SaveProvider("", name, provider)
}

// SaveProvider adds the provider name (old is "") or replaces the provider old, which
// may be renamed. An empty key keeps the key of old. Sessions using old follow a rename.
func (a *Agent) SaveProvider(old, name string, provider Provider) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	err := a.withSelection(func(sel *selection) {
		if old != "" && sel.Provider == old {
			sel.Provider = name
		}
	}, func(s *Settings) error {
		previous, editing := s.Providers[old]
		if old != "" && !editing {
			return fmt.Errorf("provider %q not found", old)
		}
		if _, exists := s.Providers[name]; exists && name != old {
			return fmt.Errorf("provider %q already exists", name)
		}
		if editing && provider.APIKey == "" && provider.APIKeyEnv == "" {
			provider.APIKey, provider.APIKeyEnv = previous.APIKey, previous.APIKeyEnv
		}
		delete(s.Providers, old)
		s.Providers[name] = provider
		if old != "" && s.Provider == old {
			s.Provider = name
		}
		return nil
	})
	if err == nil && old != "" && old != name {
		_, err = a.db.Exec(`UPDATE sessions SET provider = ? WHERE provider = ?`, name, old)
	}
	return err
}

// DeleteProvider removes a provider. A selection that used it is cleared.
func (a *Agent) DeleteProvider(name string) error {
	return a.withSelection(func(sel *selection) {
		if sel.Provider == name {
			*sel = selection{}
		}
	}, func(s *Settings) error {
		if _, ok := s.Providers[name]; !ok {
			return fmt.Errorf("provider %q not found", name)
		}
		delete(s.Providers, name)
		if s.Provider == name {
			s.Provider, s.Model, s.Effort = "", "", ""
		}
		return nil
	})
}

// withSelection adjusts the session selection, then saves the change; a failed save
// restores the previous selection.
func (a *Agent) withSelection(adjust func(*selection), change func(*Settings) error) error {
	a.cfg.mu.Lock()
	previous := a.cfg.sel
	adjust(&a.cfg.sel)
	a.cfg.mu.Unlock()
	if err := a.update(change); err != nil {
		a.cfg.mu.Lock()
		a.cfg.sel = previous
		a.cfg.mu.Unlock()
		return err
	}
	return nil
}

// ProviderModels lists the models of a provider that need not be saved yet: a connection test.
func ProviderModels(ctx context.Context, p Provider) ([]string, error) {
	models, err := newClient(p).models(ctx)
	if err != nil && p.key() != "" {
		err = errors.New(strings.ReplaceAll(err.Error(), p.key(), "[redacted]"))
	}
	sort.Strings(models)
	return models, err
}

// ChangeSettings applies change to a copy of the settings, then validates, applies and saves it.
func (a *Agent) ChangeSettings(change func(*Settings)) error {
	return a.update(func(s *Settings) error { change(s); return nil })
}
