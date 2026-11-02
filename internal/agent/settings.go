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

// Constants for behavior previously configurable.
const (
	CompactThreshold       = 400000 // tokens
	CompactKeepTurns       = 4
	DefaultShellTimeout    = 120   // seconds
	DefaultShellMaxTimeout = 600   // seconds
	DefaultShellMaxOutput  = 8192  // bytes
	DefaultMaxFileBytes    = 65536 // 64 KiB
	DashyBlockTimeout      = 30 * time.Second
	DashyBlockMaxOutput    = 65536

	ShortCodeLines    = 10
	ShortResultLines  = 4
	MediumCodeLines   = 40
	MediumResultLines = 16
)

type Provider struct {
	BaseURL   string `json:"base_url,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

type Settings struct {
	Provider         string              `json:"provider"`
	Model            string              `json:"model"`
	Effort           string              `json:"effort"`
	ReasoningSummary bool                `json:"reasoning_summary"`
	AutoCompact      bool                `json:"auto_compact"`
	ShowLines        string              `json:"show_lines"`
	Providers        map[string]Provider `json:"providers,omitempty"`
}

// Efforts are the reasoning efforts offered by /settings; empty uses the provider default.
var Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func DefaultSettings() Settings {
	return Settings{
		ReasoningSummary: true,
		AutoCompact:      true,
		ShowLines:        "short",
		Providers:        map[string]Provider{},
	}
}

func (s Settings) MaxLines(kind string) int {
	if s.ShowLines == "full" {
		return -1
	}
	if s.ShowLines == "medium" {
		if kind == "code" {
			return MediumCodeLines
		}
		return MediumResultLines
	}
	if kind == "code" {
		return ShortCodeLines
	}
	return ShortResultLines
}

func (s Settings) MaxCodeLines() int     { return s.MaxLines("code") }
func (s Settings) MaxResultLines() int   { return s.MaxLines("output") }
func (s Settings) MaxThinkingLines() int { return s.MaxLines("reasoning") }

func validateSettings(s Settings) error {
	if s.ShowLines != "" && s.ShowLines != "short" && s.ShowLines != "medium" && s.ShowLines != "full" {
		return fmt.Errorf("show_lines must be short, medium, or full")
	}
	if s.Effort != "" && !slices.Contains(Efforts, s.Effort) {
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
			return fmt.Errorf("provider %q has conflicting api_key and api_key_env", name)
		}
	}
	if s.Provider != "" {
		p, ok := s.Providers[s.Provider]
		if !ok {
			return fmt.Errorf("selected provider %q is not configured", s.Provider)
		}
		if p.APIKeyEnv != "" && strings.TrimSpace(os.Getenv(p.APIKeyEnv)) == "" {
			return fmt.Errorf("provider %q: environment variable %q is not set or empty", s.Provider, p.APIKeyEnv)
		}
	} else if s.Model != "" {
		return fmt.Errorf("model %q selected but no provider configured", s.Model)
	}
	return nil
}

// LoadSettings reads and validates settings from path. Missing file returns DefaultSettings.
func LoadSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultSettings(), nil
		}
		return Settings{}, err
	}
	s := DefaultSettings()
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("settings %s: %w", path, err)
	}
	if s.ShowLines == "" {
		s.ShowLines = "short"
	}
	if s.Providers == nil {
		s.Providers = map[string]Provider{}
	}
	if err := validateSettings(s); err != nil {
		return Settings{}, fmt.Errorf("settings %s: %w", path, err)
	}
	return s, nil
}

// SaveSettings validates and atomically writes settings to path with 0600 mode and 0700 directory.
func SaveSettings(path string, s Settings) error {
	if err := validateSettings(s); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0700)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := os.Chmod(tmpPath, 0600); err != nil {
		tmpFile.Close()
		return err
	}
	if _, err := tmpFile.Write(append(data, '\n')); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func configDir(cwd string) (string, error) {
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

// Settings returns the selected configuration without exposing mutable provider state.
func (a *Agent) Settings() Settings {
	s := a.settings
	s.Providers = maps.Clone(a.settings.Providers)
	return s
}

// SaveSettings writes the agent's current settings to its primary config file.
func (a *Agent) SaveSettings() error {
	path := a.PrimaryConfig()
	if path == "" {
		return nil
	}
	return SaveSettings(path, a.settings)
}

// ApplySettings updates the settings for testing and runtime use.
func (a *Agent) ApplySettings(s Settings) {
	a.applySettings(s)
}

func (a *Agent) applySettings(s Settings) {
	if s.ShowLines == "" {
		s.ShowLines = "short"
	}
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

func (a *Agent) applyAndSave(next Settings) error {
	if err := validateSettings(next); err != nil {
		return err
	}
	prev := a.settings
	a.applySettings(next)
	if err := a.SaveSettings(); err != nil {
		a.applySettings(prev)
		return err
	}
	return nil
}

// AttachmentsDir returns the attachments directory in DASHY_HOME.
func (a *Agent) AttachmentsDir() string {
	if a == nil || a.home == "" {
		return ""
	}
	return filepath.Join(a.home, "attachments")
}

func newClient(p Provider) openai.Client {
	return openai.NewClient(
		option.WithBaseURL(strings.TrimRight(p.BaseURL, "/")+"/"),
		option.WithAPIKey(p.APIKey),
		option.WithOrganization(""),
		option.WithProject(""),
		option.WithMaxRetries(0),
		option.WithRequestTimeout(10*time.Minute),
	)
}

// DashyHome returns the effective dashy configuration directory for cwd.
func DashyHome(cwd string) (string, error) {
	return configDir(cwd)
}
