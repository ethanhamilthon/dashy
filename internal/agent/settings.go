package agent

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Constants that used to be configurable.
const (
	CompactThreshold  = 400000 // estimated tokens that trigger automatic compaction
	CompactKeepTurns  = 4
	ShellTimeout      = 120  // seconds, default for a bash call
	ShellMaxTimeout   = 600  // seconds
	ShellOutput       = 8192 // bytes kept per bash call by default
	shortCodeLines    = 10   // show_lines=short
	shortResultLines  = 4
	mediumCodeLines   = 40 // show_lines=medium
	mediumResultLines = 16
)

// Efforts are the reasoning efforts offered by /settings; "" uses the provider default.
var Efforts = []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Provider is an OpenAI-compatible endpoint. APIKey and APIKeyEnv are exclusive.
type Provider struct {
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// Settings is the content of ~/.dashy/settings.json.
type Settings struct {
	Provider         string              `json:"provider"`
	Model            string              `json:"model"`
	Effort           string              `json:"effort"`
	ReasoningSummary bool                `json:"reasoning_summary"`
	AutoCompact      bool                `json:"auto_compact"`
	ShowLines        string              `json:"show_lines"`
	Providers        map[string]Provider `json:"providers"`
}

// DefaultSettings is used when no settings file exists.
func DefaultSettings() Settings {
	return Settings{ReasoningSummary: true, AutoCompact: true, ShowLines: "short", Providers: map[string]Provider{}}
}

// MaxLines is how many lines of a kind ("code", "output" or "reasoning") the timeline shows; -1 means all.
func (s Settings) MaxLines(kind string) int {
	switch {
	case s.ShowLines == "full":
		return -1
	case s.ShowLines == "medium" && kind == "code":
		return mediumCodeLines
	case s.ShowLines == "medium":
		return mediumResultLines
	case kind == "code":
		return shortCodeLines
	}
	return shortResultLines
}

func validateSettings(s Settings) error {
	if s.ShowLines != "short" && s.ShowLines != "medium" && s.ShowLines != "full" {
		return fmt.Errorf("show_lines must be short, medium or full")
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

// LoadSettings reads and validates a settings file; a missing file means defaults.
func LoadSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	s := DefaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("settings %s: %w", path, err)
	}
	if s.Providers == nil {
		s.Providers = map[string]Provider{}
	}
	if err := validateSettings(s); err != nil {
		return Settings{}, fmt.Errorf("settings %s: %w", path, err)
	}
	return s, nil
}

// writeSettings validates and atomically writes settings (directory 0700, file 0600).
func writeSettings(path string, s Settings) error {
	if err := validateSettings(s); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(data, '\n'))
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

// SettingsPath is the settings file in DASHY_HOME.
func (a *Agent) SettingsPath() string { return filepath.Join(a.home, "settings.json") }

// AttachmentsDir is where pasted and viewed images are saved.
func (a *Agent) AttachmentsDir() string { return filepath.Join(a.home, "attachments") }

// Settings returns a copy of the effective settings.
func (a *Agent) Settings() Settings {
	s := a.settings
	s.Providers = maps.Clone(a.settings.Providers)
	return s
}

// applySettings makes s effective and rebuilds the API client for the selected provider.
func (a *Agent) applySettings(s Settings) {
	if s.Providers == nil {
		s.Providers = map[string]Provider{}
	}
	a.settings = s
	p := s.Providers[s.Provider]
	if p.APIKey == "" && p.APIKeyEnv != "" {
		p.APIKey = os.Getenv(p.APIKeyEnv)
	}
	a.client = newClient(p)
}

// applyAndSave validates, applies and persists next. A failed save restores the previous settings.
func (a *Agent) applyAndSave(next Settings) error {
	if err := validateSettings(next); err != nil {
		return err
	}
	prev := a.settings
	a.applySettings(next)
	if err := writeSettings(a.SettingsPath(), next); err != nil {
		a.applySettings(prev)
		return err
	}
	return nil
}

func newClient(p Provider) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(strings.TrimRight(p.BaseURL, "/")+"/"),
		option.WithAPIKey(p.APIKey),
		option.WithMaxRetries(0),
		option.WithRequestTimeout(10*time.Minute),
	)
}
