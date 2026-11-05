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
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Fixed limits of the bash tool and the timeline (show_lines).
const (
	ShellTimeout      = 120  // seconds, default for a bash call
	ShellMaxTimeout   = 600  // seconds
	ShellOutput       = 8192 // bytes kept per bash call by default (shell_output setting)
	normalCodeLines   = 3    // show_lines=normal: first lines of a tool call, no output
	normalReasonLines = 3
	fullOutputLines   = 6 // show_lines=full: whole tool call, first lines of the output
	fullReasonLines   = 6
)

// The shell_output range /settings steps through; stored settings may hold any value >= 0.
const (
	ShellOutputMin = 2048
	ShellOutputMax = 65536
)

// ShowLinesModes are the show_lines values in /settings order.
var ShowLinesModes = []string{"minimal", "normal", "full"}

// Efforts are the reasoning efforts offered by /models; "" uses the provider default.
var Efforts = []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Provider is an OpenAI-compatible endpoint. APIKey and APIKeyEnv are exclusive.
type Provider struct {
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// key is the API key of p: the stored key or the value of its environment variable.
func (p Provider) key() string {
	if p.APIKey == "" && p.APIKeyEnv != "" {
		return os.Getenv(p.APIKeyEnv)
	}
	return p.APIKey
}

// Redact replaces every configured API key (and any extra secret) in text with [redacted].
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

// Settings are stored as one JSON row in the database.
type Settings struct {
	Provider         string              `json:"provider"`
	Model            string              `json:"model"`
	Effort           string              `json:"effort"`
	ReasoningSummary bool                `json:"reasoning_summary"`
	ShowLines        string              `json:"show_lines"`
	ShellOutput      int                 `json:"shell_output"` // bytes of output kept per bash call, 0 = all
	Providers        map[string]Provider `json:"providers"`
}

// DefaultSettings is used when no settings file exists.
func DefaultSettings() Settings {
	return Settings{ReasoningSummary: true, ShowLines: "normal", ShellOutput: ShellOutput, Providers: map[string]Provider{}}
}

// SetupHint explains what must be configured before a model request; "" when nothing is missing.
func (s Settings) SetupHint() string {
	switch {
	case s.Provider != "" && s.Model != "":
		return ""
	case len(s.Providers) == 0:
		return "No provider configured. Use /connect to add a provider, then choose a model with /models."
	}
	return "No model selected. Choose a model with /models (use /connect to add more providers)."
}

// MaxLines is how many lines of a kind ("code", "output" or "reasoning") the timeline shows;
// 0 hides the block and -1 shows all of it.
//
//	minimal: only user and assistant messages
//	normal:  messages, the first 3 lines of a tool call, no output
//	full:    messages, the whole tool call, the first 6 lines of the output
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

// LoadSettings reads the settings stored in home; a fresh database means defaults.
func LoadSettings(home string) (Settings, error) {
	db, err := openDB(home)
	if err != nil {
		return Settings{}, err
	}
	defer db.Close()
	s, _, err := loadSettings(db)
	return s, err
}

// SaveSettings validates and stores settings in home.
func SaveSettings(home string, s Settings) error {
	db, err := openDB(home)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = storeSettings(db, s)
	return err
}

// loadSettings reads and validates the stored settings and their version (0 when none are stored).
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

// settingsVersion is the version of the stored settings; it grows on every save.
func settingsVersion(db *sql.DB) (version int64, err error) {
	err = db.QueryRow(`SELECT version FROM settings WHERE id = 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

// storeSettings validates and saves settings, returning their new version.
func storeSettings(db *sql.DB, s Settings) (version int64, err error) {
	if err := validateSettings(s); err != nil {
		return 0, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return 0, err
	}
	err = db.QueryRow(`INSERT INTO settings (id, data, version) VALUES (1, ?, 1)
		ON CONFLICT (id) DO UPDATE SET data = excluded.data, version = settings.version + 1
		RETURNING version`, string(data)).Scan(&version)
	return version, err
}

// DashyHome is DASHY_HOME: default ~/.dashy, a relative value resolves against cwd.
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

// DBPath is the database holding settings and sessions.
func (a *Agent) DBPath() string { return DBPath(a.home) }

// AttachmentsDir is where pasted and viewed images are saved.
func (a *Agent) AttachmentsDir() string { return filepath.Join(a.home, "attachments") }

// settingsState is the live settings of an agent. The turn goroutine, the TUI and
// reloads of changes made by other dashy processes share it, so every access goes through mu.
type settingsState struct {
	mu      sync.Mutex
	s       Settings
	client  openai.Client
	version int64 // of the stored settings when s was loaded or saved
	warned  int64 // last version whose reload failed, reported once
}

// set makes s effective and rebuilds the API client for its provider. Callers hold mu or own c.
func (c *settingsState) set(s Settings, version int64) {
	if s.Providers == nil {
		s.Providers = map[string]Provider{}
	}
	c.s, c.version = s, version
	c.client = newClient(s.Providers[s.Provider])
}

// readSettings loads the stored settings with the per-run -p overrides applied and validated.
func (a *Agent) readSettings() (Settings, int64, error) {
	s, version, err := loadSettings(a.db)
	if err != nil {
		return Settings{}, version, err
	}
	for field, value := range map[*string]string{&s.Provider: a.opts.Provider, &s.Model: a.opts.Model, &s.Effort: a.opts.Effort} {
		if value != "" {
			*field = value
		}
	}
	return s, version, validateSettings(s)
}

// reloadLocked re-reads the settings when another process saved a new version. Settings
// that fail to load keep the previous ones and return a warning, once per version.
func (a *Agent) reloadLocked() (warning string) {
	c := a.cfg
	version, err := settingsVersion(a.db)
	if err != nil || version == c.version {
		return ""
	}
	s, version, err := a.readSettings()
	if err != nil {
		if version == c.warned {
			return ""
		}
		c.warned = version
		return "Stored settings changed but could not be loaded; keeping the previous settings: " + Redact(c.s, err.Error())
	}
	c.set(s, version)
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

// snapshot is the settings and client for one model request.
func (a *Agent) snapshot() (Settings, openai.Client) {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked(), a.cfg.client
}

func (a *Agent) settingsLocked() Settings {
	s := a.cfg.s
	s.Providers = maps.Clone(s.Providers)
	return s
}

// Settings returns a copy of the effective settings.
func (a *Agent) Settings() Settings {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	return a.settingsLocked()
}

// update changes the latest settings (other processes' changes included), then validates, saves and
// applies them. A failed change or save leaves the previous settings in place.
func (a *Agent) update(change func(*Settings) error) error {
	a.cfg.mu.Lock()
	defer a.cfg.mu.Unlock()
	a.reloadLocked()
	next := a.settingsLocked()
	if err := change(&next); err != nil {
		return err
	}
	version, err := storeSettings(a.db, next)
	if err != nil {
		return err
	}
	a.cfg.set(next, version)
	return nil
}

func newClient(p Provider) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(strings.TrimRight(p.BaseURL, "/")+"/"),
		option.WithAPIKey(p.key()),
		option.WithMaxRetries(0),
		option.WithRequestTimeout(10*time.Minute),
	)
}
