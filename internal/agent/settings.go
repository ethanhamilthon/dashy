package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const (
	ShellTimeout      = 120
	ShellMaxTimeout   = 600
	ShellOutput       = 8192
	normalCodeLines   = 3
	normalReasonLines = 3
	fullOutputLines   = 6
	fullReasonLines   = 6
)

// The shell_output range /settings steps through; stored settings may hold any value >= 0.
const (
	ShellOutputMin = 2048
	ShellOutputMax = 65536
)

var ShowLinesModes = []string{"minimal", "normal", "full"}

var Efforts = []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

type Provider struct {
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

func (p Provider) key() string {
	if p.APIKey == "" && p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return p.APIKey
}

func Redact(s Settings, text string, extra ...string) string {
	secrets := extra
	for _, p := range s.Providers {
		secrets = append(secrets, p.APIKey, p.key())
	}
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	return text
}

type Settings struct {
	Provider         string              `json:"provider"`
	Model            string              `json:"model"`
	Effort           string              `json:"effort"`
	ReasoningSummary bool                `json:"reasoning_summary"`
	ShowLines        string              `json:"show_lines"`
	ShellOutput      int                 `json:"shell_output"`
	Providers        map[string]Provider `json:"providers"`
}

func DefaultSettings() Settings {
	return Settings{ReasoningSummary: true, ShowLines: "normal", ShellOutput: ShellOutput, Providers: map[string]Provider{}}
}

func (s Settings) SetupHint() string {
	switch {
	case s.Provider != "" && s.Model != "":
		return ""
	case len(s.Providers) == 0:
		return "No provider configured. Use /connect to add a provider, then choose a model with /models."
	}
	return "No model selected. Choose a model with /models (use /connect to add more providers)."
}

func (s Settings) MaxLines(kind string) int {
	switch s.ShowLines {
	case "minimal":
		return 0
	case "normal":
		switch kind {
		case "code":
			return normalCodeLines
		case "reasoning":
			return normalReasonLines
		}
		return 0
	}
	switch kind {
	case "code":
		return -1
	case "reasoning":
		return fullReasonLines
	}
	return fullOutputLines
}

func validateSettings(s Settings) error {
	if !slices.Contains(ShowLinesModes, s.ShowLines) {
		return fmt.Errorf("show_lines must be minimal, normal or full")
	}
	if s.ShellOutput < 0 {
		return fmt.Errorf("shell_output must be >= 0 bytes (0 = no truncation)")
	}
	if !slices.Contains(Efforts, s.Effort) {
		return fmt.Errorf("unsupported effort %q", s.Effort)
	}
	for name, p := range s.Providers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("provider name cannot be empty")
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("provider %q needs an HTTP(S) base_url", name)
		}
		if p.APIKey != "" && p.APIKeyEnv != "" {
			return fmt.Errorf("provider %q has both api_key and api_key_env", name)
		}
	}
	if s.Provider == "" {
		if s.Model != "" {
			return fmt.Errorf("model %q selected but no provider configured", s.Model)
		}
		return nil
	}
	p, ok := s.Providers[s.Provider]
	if !ok {
		return fmt.Errorf("selected provider %q is not configured", s.Provider)
	}
	if p.APIKeyEnv != "" && strings.TrimSpace(os.Getenv(p.APIKeyEnv)) == "" {
		return fmt.Errorf("provider %q: environment variable %q is not set or empty", s.Provider, p.APIKeyEnv)
	}
	return nil
}

func LoadSettings(home string) (Settings, error) {
	db, err := openDB(home)
	if err != nil {
		return Settings{}, err
	}
	defer db.Close()
	s, _, err := loadSettings(db)
	return s, err
}

func SaveSettings(home string, s Settings) error {
	db, err := openDB(home)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := validateSettings(s); err != nil {
		return err
	}
	version, err := settingsVersion(db)
	if err == nil {
		_, err = storeSettingsAt(db, s, version)
	}
	return err
}

func loadSettings(db *sql.DB) (Settings, int64, error) {
	var data []byte
	var version int64
	s := DefaultSettings()
	err := db.QueryRow(`SELECT data, version FROM settings WHERE id = 1`).Scan(&data, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return s, 0, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &s)
	}
	if err == nil {
		err = validateSettings(s)
	}
	if err != nil {
		return Settings{}, version, fmt.Errorf("settings: %w", err)
	}
	return s, version, nil
}

func settingsVersion(db *sql.DB) (version int64, err error) {
	err = db.QueryRow(`SELECT version FROM settings WHERE id = 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

// errSettingsConflict means another process saved the settings since they were read.
var errSettingsConflict = errors.New("settings changed concurrently; try again")

func storeSettingsAt(db *sql.DB, s Settings, expected int64) (version int64, err error) {
	data, err := json.Marshal(s)
	if err != nil {
		return 0, err
	}
	if expected == 0 {
		err = db.QueryRow(`INSERT INTO settings (id, data, version) VALUES (1, ?, 1)
			ON CONFLICT (id) DO NOTHING RETURNING version`, string(data)).Scan(&version)
	} else {
		err = db.QueryRow(`UPDATE settings SET data = ?, version = version + 1
			WHERE id = 1 AND version = ? RETURNING version`, string(data), expected).Scan(&version)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errSettingsConflict
	}
	return version, err
}

func DashyHome(cwd string) (string, error) {
	dir := os.Getenv("DASHY_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".dashy")
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(cwd, dir)
	}
	return filepath.Abs(dir)
}

func (a *Agent) DBPath() string { return DBPath(a.home) }

func (a *Agent) AttachmentsDir() string { return filepath.Join(a.home, "attachments") }

// settingsState is the live settings of an agent. The turn goroutine, the TUI and
// reloads of changes made by other dashy processes share it, so every access goes through mu.
type settingsState struct {
	mu      sync.Mutex
	base    Settings
	sel     selection
	s       Settings
	client  *client
	version int64
	warned  int64
}

type selection struct{ Provider, Model, Effort string }

func (a *Agent) effectiveLocked(base Settings) (Settings, error) {
	s := base
	s.Providers = maps.Clone(base.Providers)
	if s.Providers == nil {
		s.Providers = map[string]Provider{}
	}
	if sel := a.cfg.sel; sel.Model != "" {
		s.Provider, s.Model, s.Effort = sel.Provider, sel.Model, sel.Effort
	}
	for field, value := range map[*string]string{&s.Provider: a.opts.Provider, &s.Model: a.opts.Model, &s.Effort: a.opts.Effort} {
		if value != "" {
			*field = value
		}
	}
	return s, validateSettings(s)
}

func (a *Agent) applyLocked(base Settings, version int64) error {
	s, err := a.effectiveLocked(base)
	if err != nil {
		return err
	}
	c := a.cfg
	c.base, c.s, c.version = base, s, version
	c.client = newClient(s.Providers[s.Provider])
	return nil
}

func (a *Agent) fixSelection(sel selection) {
	c := a.cfg
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sel = sel
	if sel.Model == "" || a.applyLocked(c.base, c.version) != nil {
		c.sel = selection{}
		_ = a.applyLocked(c.base, c.version)
	}
	if c.s.Model != "" {
		c.sel = selection{c.s.Provider, c.s.Model, c.s.Effort}
	}
}

// reloadLocked re-reads the settings when another process saved a new version. Settings
// that fail to load keep the previous ones and return a warning, once per version.
func (a *Agent) reloadLocked() (warning string) {
	c := a.cfg
	version, err := settingsVersion(a.db)
	if err != nil || version == c.version {
		return ""
	}
	base, version, err := loadSettings(a.db)
	if err == nil {
		err = a.applyLocked(base, version)
	}
	if err != nil {
		if version == c.warned {
			return ""
		}
		c.warned = version
		return "Stored settings changed but could not be loaded; keeping the previous settings: " + Redact(c.s, err.Error())
	}
	return ""
}

// refreshSettings picks up settings saved by other processes since the last request. emit may be nil.
func (a *Agent) refreshSettings(emit func(Event)) {
	a.cfg.mu.Lock()
	warning := a.reloadLocked()
	a.cfg.mu.Unlock()
	if warning != "" && emit != nil {
		emit(Event{Kind: "notice", Text: warning})
	}
}

func (a *Agent) snapshot() (Settings, *client) {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked(), a.cfg.client
}

func (a *Agent) settingsLocked() Settings {
	s := a.cfg.s
	s.Providers = maps.Clone(s.Providers)
	return s
}

func (a *Agent) Settings() Settings {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked()
}

// update changes the latest stored settings (other processes' changes included), then
// validates, saves and applies them. A failed change or save leaves the previous settings in place.
// The save succeeds only if nobody saved in between; otherwise the change is applied again
// to the newer settings, so concurrent edits of different fields all survive.
func (a *Agent) update(change func(*Settings) error) error {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	for range 20 {
		a.reloadLocked()
		base, version, err := loadSettings(a.db)
		if err != nil {
			// Unreadable stored settings are replaced by the change applied to the last good ones.
			base = a.cfg.base
		}
		next := base
		next.Providers = maps.Clone(next.Providers)
		if next.Providers == nil {
			next.Providers = map[string]Provider{}
		}
		if err := change(&next); err != nil {
			return err
		}
		if err := validateSettings(next); err != nil {
			return err
		}
		if _, err := a.effectiveLocked(next); err != nil {
			return err
		}
		saved, err := storeSettingsAt(a.db, next, version)
		if errors.Is(err, errSettingsConflict) {
			continue
		}
		if err != nil {
			return err
		}
		return a.applyLocked(next, saved)
	}
	return errSettingsConflict
}
