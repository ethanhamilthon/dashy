package agent

import (
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
	return DefaultSettings()
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
	primary := filepath.Join(home, "settings.json")
	s, err := LoadSettings(primary)
	if err != nil {
		return configuration{}, err
	}
	return configuration{
		settings: s,
		primary:  primary,
	}, nil
}
