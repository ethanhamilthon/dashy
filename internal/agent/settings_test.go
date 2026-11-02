package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A missing settings file means defaults; invalid files are rejected with the reason in the error.
func TestLoadSettingsDefaultsAndValidation(t *testing.T) {
	s, err := LoadSettings(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || !s.ReasoningSummary || !s.AutoCompact || s.ShowLines != "short" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	t.Setenv("DASHY_TEST_UNSET_KEY", "")
	bad := map[string]string{
		"show_lines":       `{"show_lines":"huge","reasoning_summary":true,"auto_compact":true}`,
		"base_url":         `{"provider":"p","model":"m","show_lines":"short","providers":{"p":{"base_url":"ftp://x"}}}`,
		"key and env":      `{"provider":"p","model":"m","show_lines":"short","providers":{"p":{"base_url":"https://x","api_key":"a","api_key_env":"B"}}}`,
		"effort":           `{"provider":"p","model":"m","show_lines":"short","effort":"ultra","providers":{"p":{"base_url":"https://x"}}}`,
		"unset key env":    `{"provider":"p","model":"m","show_lines":"short","providers":{"p":{"base_url":"https://x","api_key_env":"DASHY_TEST_UNSET_KEY"}}}`,
		"unknown provider": `{"provider":"nope","model":"m","show_lines":"short"}`,
	}
	for name, body := range bad {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSettings(path); err == nil || !strings.Contains(err.Error(), "settings") {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

// Settings are written atomically with private permissions and read back unchanged.
func TestSaveSettingsIsPrivateAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "home", "settings.json")
	s := DefaultSettings()
	s.Provider, s.Model, s.Effort = "p", "m", "high"
	s.Providers["p"] = Provider{BaseURL: "https://api.example.com/v1"}
	if err := writeSettings(path, s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("file mode %v %v", info, err)
	}
	dir, _ := os.Stat(filepath.Dir(path))
	if dir.Mode().Perm() != 0700 {
		t.Fatalf("directory mode %v", dir.Mode().Perm())
	}
	back, err := LoadSettings(path)
	if err != nil || back.Model != "m" || back.Effort != "high" || back.Providers["p"].BaseURL != "https://api.example.com/v1" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

// Choices made in the app are applied at once and saved; invalid changes change nothing.
func TestSelectionsAreAppliedAndSaved(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	if err := a.Select("p", "m2", "high"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddProvider("q", Provider{BaseURL: "https://q.example"}); err != nil {
		t.Fatal(err)
	}
	if a.AddProvider("q", Provider{BaseURL: "https://q.example"}) == nil || a.AddProvider(" ", Provider{BaseURL: "https://x"}) == nil {
		t.Fatal("duplicate or empty provider name accepted")
	}
	if a.Select("missing", "m", "") == nil || a.Settings().Model != "m2" {
		t.Fatal("selection of an unknown provider accepted")
	}
	if err := a.SetAutoCompact(false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"medium", "full", "short"} {
		if err := a.CycleShowLines(); err != nil || a.Settings().ShowLines != want {
			t.Fatalf("show lines %q, want %q (%v)", a.Settings().ShowLines, want, err)
		}
	}
	back, err := LoadSettings(a.SettingsPath())
	if err != nil || back.Model != "m2" || back.Effort != "high" || back.AutoCompact || back.Providers["q"].BaseURL == "" {
		t.Fatalf("saved settings: %+v %v", back, err)
	}
}
