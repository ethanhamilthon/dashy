package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ConfigFiles controls configuration lookup.
type ConfigFiles struct {
	Config   string
	NoGlobal bool
}

// Source describes a configuration source.
type Source struct {
	Name, Path, Origin string
	Sets               []string
}

type configuration struct {
	settings Settings
	system   string
	local    string
	sources  []Source
	primary  string
}

// PrimaryConfig returns the base settings file path.
func (a *Agent) PrimaryConfig() string {
	if a == nil {
		return ""
	}
	if a.primaryConfig != "" {
		return a.primaryConfig
	}
	if a.home != "" {
		return filepath.Join(a.home, "settings.json")
	}
	return ""
}

// ProjectFile reports whether path is inside the project directory.
func (a *Agent) ProjectFile(path string) bool {
	return false
}

func defaultSettings() Settings {
	return Settings{
		CompactKeepTurns: 4,
		MaxContextTokens: 400000,
		MaxCodeLines:     999,
		MaxResultLines:   4,
		MaxThinkingLines: 4,
		ShellTimeout:     120,
		ShellMaxTimeout:  600,
		ShellMaxOutput:   8192,
		ReasoningSummary: "auto",
		Providers:        map[string]Provider{},
	}
}

func loadConfiguration(home, dir string, files ConfigFiles) (configuration, error) {
	if files.Config != "" {
		p := files.Config
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		if _, err := os.Stat(p); err != nil {
			return configuration{}, fmt.Errorf("config %s: %w", p, err)
		}
	}
	s := defaultSettings()
	if home != "" {
		if data, err := os.ReadFile(filepath.Join(home, "settings.json")); err == nil {
			_ = json.Unmarshal(data, &s)
		}
	}
	return configuration{
		settings: s,
		primary:  filepath.Join(home, "settings.json"),
	}, nil
}
