package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
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

// SelectionConfig is the file /model writes: the highest layer that sets model, or the
// primary file when no layer does. Origins are cached until the next reload.
func (a *Agent) SelectionConfig() string {
	for i := len(a.Sources) - 1; i >= 0; i-- {
		if slices.Contains(a.Sources[i].Sets, "model") {
			return a.Sources[i].Path
		}
	}
	return a.PrimaryConfig()
}

// transact runs one configuration write under an exclusive lock on the file behind
// target. patch writes the file, the merged configuration is reloaded, and commit
// applies it to the session. A failing step restores the file.
func (a *Agent) transact(target string, patch func(path string) error, commit func(configuration) error) error {
	path := resolvePath(target)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	backup, existed := readFileIfExists(path)
	if err := patch(path); err != nil {
		return err
	}
	cfg, err := loadConfiguration(a.home, a.Shell.Dir, a.files)
	if err == nil {
		err = commit(cfg)
	}
	if err != nil {
		rollbackFile(path, backup, existed)
	}
	return err
}

// Select saves provider, model and effort to SelectionConfig and makes them the session
// selection. Other settings and the session's other state are not touched.
func (a *Agent) Select(provider, model, effort string) error {
	prev := a.settings
	target := a.SelectionConfig()
	want := Selection{provider, model, effort}
	err := a.transact(target, func(path string) error {
		return patchRootKeys(path, map[string]string{"provider": provider, "model": model, "effort": effort})
	}, func(cfg configuration) error {
		if got := (Selection{cfg.settings.Provider, cfg.settings.Model, cfg.settings.Effort}); got != want {
			return shadowError(cfg.sources, target)
		}
		next := prev
		next.Provider, next.Model, next.Effort = provider, model, effort
		if err := validateSettings(next); err != nil {
			return err
		}
		a.settings = next // Save records the new selection
		if a.HasSavedSession() {
			if err := a.Save(); err != nil {
				a.settings = prev
				return err
			}
		}
		a.applySettings(next)
		return nil
	})
	if err != nil {
		return err
	}
	if prev.Provider != provider || prev.Model != model || prev.Effort != effort {
		a.Fire("model_change", "DASHY_PREV_PROVIDER", prev.Provider, "DASHY_PREV_MODEL", prev.Model, "DASHY_PREV_EFFORT", prev.Effort)
	}
	return nil
}

// shadowError explains why a saved selection would not take effect: a higher layer
// sets the same keys, so the merged defaults would differ from the selection.
func shadowError(sources []Source, target string) error {
	for i := len(sources) - 1; i >= 0; i-- {
		if src := sources[i]; src.Path != target && len(src.Sets) > 0 {
			return fmt.Errorf("selection not saved: %s also sets provider, model or effort and overrides %s; change that file or use its values", src.Path, target)
		}
	}
	return fmt.Errorf("selection not saved: %s does not take effect", target)
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
				p.APIKey = resolveEnv(p.APIKeyEnv, env)
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

// AddProvider writes a provider to the primary configuration. Only the provider
// registry changes in the session; the selection and other settings stay as they are.
func (a *Agent) AddProvider(name string, provider Provider) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("provider name is required")
	}
	if _, exists := a.settings.Providers[name]; exists {
		return fmt.Errorf("provider %q already exists", name)
	}
	return a.transact(a.PrimaryConfig(), func(path string) error {
		return patchProvider(path, name, provider)
	}, func(cfg configuration) error {
		next := a.Settings()
		if next.Providers == nil {
			next.Providers = map[string]Provider{}
		}
		next.Providers[name] = cfg.settings.Providers[name]
		if err := validateSettings(next); err != nil {
			return err
		}
		a.applySettings(next)
		return nil
	})
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
func (a *Agent) shellEnv() map[string]string {
	env := make(map[string]string, len(a.settings.Environment)+6)
	for k, v := range a.settings.Environment {
		env[k] = v
	}
	info := a.SessionInfo()
	endpoint, ctx := "", ""
	if a.socketDir != "" {
		endpoint = filepath.Join(a.socketDir, "socket")
	}
	if info["ctx_size"] != nil {
		ctx = fmt.Sprint(info["ctx_size"])
	}
	env["DASHY_ENDPOINT"] = endpoint
	env["DASHY_SESSION_ID"] = fmt.Sprint(info["id"])
	env["DASHY_MODEL"] = a.settings.Model
	env["DASHY_PROVIDER"] = a.settings.Provider
	env["DASHY_EFFORT"] = a.settings.Effort
	env["DASHY_CTX_SIZE"] = ctx
	return env
}
