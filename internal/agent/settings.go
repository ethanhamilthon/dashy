package agent

import (
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

type Provider struct {
	BaseURL   string
	APIKey    string
	APIKeyEnv string
}

type Settings struct {
	AttachmentDir    string
	MaxContextTokens int64
	CompactKeepTurns int
	MaxAgentSteps    int
	MaxFileBytes     int
	MaxCodeLines     int
	MaxResultLines   int
	MaxThinkingLines int
	ShellTimeout     int
	ShellMaxTimeout  int
	ShellMaxOutput   int
	Provider         string
	Model            string
	ReasoningSummary string
	Effort           string
	Providers        map[string]Provider
}

// Efforts are the reasoning efforts offered by /model; empty uses the provider default.
var Efforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

func validateSettings(s Settings) error {
	if s.MaxCodeLines < 0 || s.MaxResultLines < 0 || s.MaxThinkingLines < 0 {
		return fmt.Errorf("max_*_lines must be non-negative")
	}
	if s.MaxContextTokens < 0 || s.CompactKeepTurns < 0 || s.MaxAgentSteps < 0 || s.MaxFileBytes < 0 {
		return fmt.Errorf("max_context_tokens, compact_keep_turns, max_agent_steps and max_file_bytes must be non-negative")
	}
	if s.ShellTimeout <= 0 || s.ShellMaxTimeout <= 0 {
		return fmt.Errorf("shell timeout and max_timeout must be positive")
	}
	const maxDurationSeconds = int64(math.MaxInt64 / time.Second)
	if int64(s.ShellTimeout) > maxDurationSeconds || int64(s.ShellMaxTimeout) > maxDurationSeconds {
		return fmt.Errorf("shell timeout and max_timeout must not exceed %d seconds", maxDurationSeconds)
	}
	if s.ShellTimeout > s.ShellMaxTimeout {
		return fmt.Errorf("shell timeout (%d) cannot exceed max_timeout (%d)", s.ShellTimeout, s.ShellMaxTimeout)
	}
	if s.ShellMaxOutput < 0 {
		return fmt.Errorf("shell max_output must be non-negative")
	}
	if s.Provider != "" {
		p, ok := s.Providers[s.Provider]
		if !ok {
			return fmt.Errorf("selected provider %q is not configured", s.Provider)
		}
		if p.APIKey != "" && p.APIKeyEnv != "" {
			return fmt.Errorf("provider %q has conflicting api_key and api_key_env", s.Provider)
		}
		if p.APIKeyEnv != "" {
			val := os.Getenv(p.APIKeyEnv)
			if strings.TrimSpace(val) == "" {
				return fmt.Errorf("provider %q: environment variable %q is not set or empty", s.Provider, p.APIKeyEnv)
			}
		}
	}
	for name, p := range s.Providers {
		if p.APIKey != "" && p.APIKeyEnv != "" {
			return fmt.Errorf("provider %q has conflicting api_key and api_key_env", name)
		}
		if p.BaseURL != "" {
			u, err := url.Parse(p.BaseURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("provider %q needs an HTTP(S) base_url", name)
			}
		}
	}
	if s.ReasoningSummary != "" && s.ReasoningSummary != "auto" && s.ReasoningSummary != "off" {
		return fmt.Errorf("reasoning_summary must be auto or off")
	}
	if s.Effort != "" && !slices.Contains(Efforts, s.Effort) {
		return fmt.Errorf("unsupported effort %q", s.Effort)
	}
	return nil
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

// ApplySettings updates the settings for testing and runtime use.
func (a *Agent) ApplySettings(s Settings) {
	a.applySettings(s)
}

func (a *Agent) applySettings(s Settings) {
	if s.ShellTimeout <= 0 {
		s.ShellTimeout = 120
	}
	if s.ShellMaxTimeout <= 0 {
		s.ShellMaxTimeout = 600
	}
	if s.ShellMaxOutput < 0 {
		s.ShellMaxOutput = 8192
	}
	if s.CompactKeepTurns <= 0 {
		s.CompactKeepTurns = 4
	}
	if s.MaxContextTokens <= 0 {
		s.MaxContextTokens = 400000
	}
	a.settings = s
	p := s.Providers[s.Provider]
	if p.APIKey == "" && p.APIKeyEnv != "" {
		p.APIKey = os.Getenv(p.APIKeyEnv)
	}
	a.client = newClient(p)
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
