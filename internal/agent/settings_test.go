package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateSettings(t *testing.T) {
	t.Setenv("TEST_KEY_ENV", "secret-value")

	valid := Settings{
		Provider:         "p",
		Model:            "m",
		Effort:           "low",
		ReasoningSummary: true,
		AutoCompact:      true,
		ShowLines:        "short",
		Providers: map[string]Provider{
			"p": {BaseURL: "https://example.com/v1", APIKey: "key"},
		},
	}
	if err := validateSettings(valid); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}

	for _, effort := range append([]string{""}, Efforts...) {
		s := valid
		s.Effort = effort
		if err := validateSettings(s); err != nil {
			t.Fatalf("valid effort %q rejected: %v", effort, err)
		}
	}

	for _, lines := range []string{"", "short", "medium", "full"} {
		s := valid
		s.ShowLines = lines
		if err := validateSettings(s); err != nil {
			t.Fatalf("valid show_lines %q rejected: %v", lines, err)
		}
	}

	// Unselected provider with missing env var is tolerated; selected provider checks env var
	envProv := valid
	envProv.Providers["unselected"] = Provider{BaseURL: "https://example.com", APIKeyEnv: "NON_EXISTENT_VAR"}
	if err := validateSettings(envProv); err != nil {
		t.Fatalf("unselected provider with missing env var rejected: %v", err)
	}

	selectedEnv := valid
	selectedEnv.Providers["p"] = Provider{BaseURL: "https://example.com", APIKeyEnv: "TEST_KEY_ENV"}
	if err := validateSettings(selectedEnv); err != nil {
		t.Fatalf("selected provider with valid env var rejected: %v", err)
	}

	// Invalid cases
	invalidCases := []struct {
		name string
		s    Settings
	}{
		{"missing selected provider", Settings{Provider: "missing"}},
		{"model without provider", Settings{Model: "gpt-4o"}},
		{"bad effort", Settings{Provider: "p", Effort: "ultra", Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}}},
		{"bad show_lines", Settings{Provider: "p", ShowLines: "massive", Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}}},
		{"empty base url", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: ""}}}},
		{"non-http url", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "file:///tmp"}}}},
		{"ftp url", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "ftp://example.com"}}}},
		{"url without host", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "https://"}}}},
		{"empty provider name key", Settings{Providers: map[string]Provider{" ": {BaseURL: "https://example.com"}}}},
		{"conflicting api key and env", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "https://example.com", APIKey: "k", APIKeyEnv: "E"}}}},
		{"selected provider with unset env var", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "https://example.com", APIKeyEnv: "NON_EXISTENT_ENV"}}}},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSettings(tc.s); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestLoadSettings(t *testing.T) {
	t.Run("missing file returns defaults", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		s, err := LoadSettings(path)
		if err != nil {
			t.Fatalf("unexpected error on missing file: %v", err)
		}
		if s.ReasoningSummary != true || s.AutoCompact != true || s.ShowLines != "short" {
			t.Fatalf("unexpected defaults: %+v", s)
		}
		if s.Provider != "" || s.Model != "" || len(s.Providers) != 0 {
			t.Fatalf("expected empty provider/model/providers: %+v", s)
		}
	})

	t.Run("valid settings loaded", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		data := `{
			"provider": "custom",
			"model": "my-model",
			"effort": "high",
			"reasoning_summary": false,
			"auto_compact": false,
			"show_lines": "medium",
			"providers": {
				"custom": {
					"base_url": "https://custom.example.com/v1",
					"api_key": "custom-key"
				}
			}
		}`
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSettings(path)
		if err != nil {
			t.Fatalf("load failed: %v", err)
		}
		if s.Provider != "custom" || s.Model != "my-model" || s.Effort != "high" {
			t.Fatalf("mismatched provider/model/effort: %+v", s)
		}
		if s.ReasoningSummary != false || s.AutoCompact != false || s.ShowLines != "medium" {
			t.Fatalf("mismatched toggles: %+v", s)
		}
		p, ok := s.Providers["custom"]
		if !ok || p.BaseURL != "https://custom.example.com/v1" || p.APIKey != "custom-key" {
			t.Fatalf("mismatched provider: %+v", p)
		}
	})

	t.Run("unknown keys ignored on read", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		data := `{
			"unknown_feature": "something",
			"version": 99,
			"provider": "p",
			"model": "m",
			"providers": {"p": {"base_url": "https://example.com"}}
		}`
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSettings(path)
		if err != nil {
			t.Fatalf("load failed on unknown keys: %v", err)
		}
		if s.Provider != "p" || s.Model != "m" {
			t.Fatalf("unexpected loaded settings: %+v", s)
		}
		if s.ReasoningSummary != true || s.AutoCompact != true || s.ShowLines != "short" {
			t.Fatalf("defaults not filled when omitted: %+v", s)
		}
	})

	t.Run("invalid json returns error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(path, []byte("not-json{"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettings(path); err == nil {
			t.Fatal("expected error on corrupt JSON")
		}
	})

	t.Run("empty file returns error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(path, []byte(""), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettings(path); err == nil {
			t.Fatal("expected error on empty file")
		}
	})

	t.Run("invalid settings in file returns error", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "settings.json")
		data := `{"provider": "missing"}`
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettings(path); err == nil {
			t.Fatal("expected validation error on invalid settings in file")
		}
	})
}

func TestSaveSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub", "dashy")
	path := filepath.Join(dir, "settings.json")

	s := Settings{
		Provider:         "test",
		Model:            "model-1",
		Effort:           "medium",
		ReasoningSummary: true,
		AutoCompact:      true,
		ShowLines:        "full",
		Providers: map[string]Provider{
			"test": {BaseURL: "https://api.test.com/v1", APIKey: "sk-12345"},
		},
	}

	if err := SaveSettings(path, s); err != nil {
		t.Fatalf("SaveSettings failed: %v", err)
	}

	// Verify directory mode 0700 and file mode 0600
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("directory perm = %o, want 0700", dirInfo.Mode().Perm())
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Fatalf("file perm = %o, want 0600", fileInfo.Mode().Perm())
	}

	// Verify roundtrip
	loaded, err := LoadSettings(path)
	if err != nil {
		t.Fatalf("LoadSettings failed: %v", err)
	}
	if loaded.Provider != s.Provider || loaded.Model != s.Model || loaded.Effort != s.Effort ||
		loaded.ReasoningSummary != s.ReasoningSummary || loaded.AutoCompact != s.AutoCompact || loaded.ShowLines != s.ShowLines {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", loaded, s)
	}
	if loaded.Providers["test"] != s.Providers["test"] {
		t.Fatalf("provider roundtrip mismatch: got %+v, want %+v", loaded.Providers["test"], s.Providers["test"])
	}

	// Refuses invalid settings
	invalid := s
	invalid.ShowLines = "bad"
	if err := SaveSettings(path, invalid); err == nil {
		t.Fatal("SaveSettings should reject invalid settings")
	}
}

func TestSettingsSaveAndApplyOnAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if err := a.AddProvider("prov", Provider{BaseURL: "https://example.com/v1"}); err != nil {
		t.Fatal(err)
	}

	// Verify provider was saved to settings.json
	settingsFile := filepath.Join(home, "settings.json")
	saved, err := LoadSettings(settingsFile)
	if err != nil {
		t.Fatalf("failed to load saved settings: %v", err)
	}
	if _, ok := saved.Providers["prov"]; !ok {
		t.Fatal("new provider not saved to settings.json")
	}

	// Select model
	if err := a.Select("prov", "m1", "high"); err != nil {
		t.Fatal(err)
	}
	saved, err = LoadSettings(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Provider != "prov" || saved.Model != "m1" || saved.Effort != "high" {
		t.Fatalf("select not saved: %+v", saved)
	}

	// Toggle reasoning summary
	if err := a.SetReasoningSummary(false); err != nil {
		t.Fatal(err)
	}
	saved, err = LoadSettings(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ReasoningSummary != false {
		t.Fatalf("reasoning_summary toggle not saved: %+v", saved)
	}

	// Toggle auto compact
	if err := a.SetAutoCompact(false); err != nil {
		t.Fatal(err)
	}
	saved, err = LoadSettings(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if saved.AutoCompact != false {
		t.Fatalf("auto_compact toggle not saved: %+v", saved)
	}

	// Cycle show_lines: short -> medium -> full -> short
	if err := a.CycleShowLines(); err != nil {
		t.Fatal(err)
	}
	if a.Settings().ShowLines != "medium" {
		t.Fatalf("expected medium, got %s", a.Settings().ShowLines)
	}
	if err := a.CycleShowLines(); err != nil {
		t.Fatal(err)
	}
	if a.Settings().ShowLines != "full" {
		t.Fatalf("expected full, got %s", a.Settings().ShowLines)
	}
	if err := a.CycleShowLines(); err != nil {
		t.Fatal(err)
	}
	if a.Settings().ShowLines != "short" {
		t.Fatalf("expected short, got %s", a.Settings().ShowLines)
	}
}

func TestRelativeHome(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", "custom/.dashy")
	dir, err := configDir(cwd)
	if err != nil || dir != filepath.Join(cwd, "custom", ".dashy") {
		t.Fatalf("%s %v", dir, err)
	}
}

func TestSettingsSnapshotDoesNotMutateAgent(t *testing.T) {
	a := &Agent{}
	a.applySettings(Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}})
	snapshot := a.Settings()
	snapshot.Model = "changed"
	snapshot.Providers["p"] = Provider{BaseURL: "changed"}
	if a.Settings().Model != "m" || a.Settings().Providers["p"].BaseURL != "https://example.com" {
		t.Fatal("mutable settings exposed")
	}
}
