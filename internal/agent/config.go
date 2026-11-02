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
	Mode     string
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
	var sources []Source
	var local string
	if !files.NoGlobal && home != "" {
		globalPath := filepath.Clean(filepath.Join(home, "AGENTS.md"))
		if st, err := os.Stat(globalPath); err == nil && !st.IsDir() {
			sources = append(sources, Source{Name: "global", Path: globalPath, Origin: "global"})
		}
	}
	if files.Config != "" {
		p := files.Config
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		p = filepath.Clean(p)
		if st, err := os.Stat(p); err != nil {
			return configuration{}, fmt.Errorf("config %s: %w", p, err)
		} else if st.IsDir() {
			return configuration{}, fmt.Errorf("config %s: is a directory", p)
		}
		local = p
		sources = append(sources, Source{Name: filepath.Base(p), Path: p, Origin: "flag"})
	} else if dir != "" {
		p := filepath.Clean(filepath.Join(dir, "AGENTS.md"))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			local = p
			sources = append(sources, Source{Name: "local", Path: p, Origin: "project"})
		}
	}
	primary := filepath.Join(home, "settings.json")
	s, err := LoadSettings(primary)
	if err != nil {
		return configuration{}, err
	}
	return configuration{
		settings: s,
		local:    local,
		sources:  sources,
		primary:  primary,
	}, nil
}
