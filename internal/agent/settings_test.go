package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rawSettingsHome is a DASHY_HOME whose database stores data as settings, unvalidated.
func rawSettingsHome(t *testing.T, data string) string {
	t.Helper()
	home := t.TempDir()
	db, err := openDB(home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO settings (id, data, version) VALUES (1, ?, 1)`, data); err != nil {
		t.Fatal(err)
	}
	return home
}

// A fresh database means defaults; invalid stored settings are rejected with the reason in the error.
func TestLoadSettingsDefaultsAndValidation(t *testing.T) {
	s, err := LoadSettings(t.TempDir())
	if err != nil || !s.ReasoningSummary || s.ShowLines != "normal" {
		t.Fatalf("defaults: %+v %v", s, err)
	}
	t.Setenv("DASHY_TEST_UNSET_KEY", "")
	bad := map[string]string{
		"show_lines":       `{"show_lines":"huge","reasoning_summary":true}`,
		"base_url":         `{"provider":"p","model":"m","show_lines":"normal","providers":{"p":{"base_url":"ftp://x"}}}`,
		"key and env":      `{"provider":"p","model":"m","show_lines":"normal","providers":{"p":{"base_url":"https://x","api_key":"a","api_key_env":"B"}}}`,
		"effort":           `{"provider":"p","model":"m","show_lines":"normal","effort":"ultra","providers":{"p":{"base_url":"https://x"}}}`,
		"unset key env":    `{"provider":"p","model":"m","show_lines":"normal","providers":{"p":{"base_url":"https://x","api_key_env":"DASHY_TEST_UNSET_KEY"}}}`,
		"unknown provider": `{"provider":"nope","model":"m","show_lines":"short"}`,
	}
	for name, body := range bad {
		if _, err := LoadSettings(rawSettingsHome(t, body)); err == nil || !strings.Contains(err.Error(), "settings") {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
}

// The database is private to the user, and saved settings read back unchanged.
func TestSaveSettingsIsPrivateAndRoundTrips(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	s := DefaultSettings()
	s.Provider, s.Model, s.Effort = "p", "m", "high"
	s.Providers["p"] = Provider{BaseURL: "https://api.example.com/v1"}
	if err := SaveSettings(home, s); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{home: 0700, DBPath(home): 0600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: mode %v %v", path, info, err)
		}
	}
	back, err := LoadSettings(home)
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
	if err := a.ChangeSettings(func(s *Settings) { s.ReasoningSummary, s.ShowLines = false, "full" }); err != nil || a.Settings().ShowLines != "full" {
		t.Fatalf("change: %v", err)
	}
	if a.ChangeSettings(func(s *Settings) { s.ShowLines = "huge" }) == nil || a.Settings().ShowLines != "full" {
		t.Fatal("invalid settings accepted")
	}
	back, err := LoadSettings(a.home)
	if err != nil || back.Model != "m2" || back.Effort != "high" || back.ReasoningSummary || back.Providers["q"].BaseURL == "" {
		t.Fatalf("saved settings: %+v %v", back, err)
	}
}

// The bash output limit comes from settings only: default 8192, validated, and applied to calls.
func TestShellOutputSetting(t *testing.T) {
	if s, err := LoadSettings(t.TempDir()); err != nil || s.ShellOutput != ShellOutput {
		t.Fatalf("default: %+v, %v", s, err)
	}
	if _, err := LoadSettings(rawSettingsHome(t, `{"shell_output":-1,"providers":{}}`)); err == nil {
		t.Fatal("negative shell_output accepted")
	}
	if strings.Contains(string(raw(bashParameters())), "max_output") {
		t.Fatal("bash schema still has max_output")
	}
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if err := a.ChangeSettings(func(s *Settings) { s.ShellOutput = 100 }); err != nil {
		t.Fatal(err)
	}
	res := a.runTool(context.Background(), "bash", `{"command":"head -c 1000 /dev/zero | tr '\\0' x","timeout":null,"max_output":null}`, func(Event) {})
	if !strings.Contains(res.Output, "output truncated") || len(res.Output) > 400 {
		t.Fatalf("limit not applied: %q", res.Output)
	}
}

// A change saved by another process while this one edits is kept: the edit is applied again on top.
func TestConcurrentSettingsEditsKeepBoth(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	other := true
	err := a.ChangeSettings(func(s *Settings) {
		if other {
			other = false
			stored, err := LoadSettings(a.home)
			if err != nil {
				t.Fatal(err)
			}
			stored.ShellOutput = 16384
			if err := SaveSettings(a.home, stored); err != nil {
				t.Fatal(err)
			}
		}
		s.ReasoningSummary = false
	})
	if err != nil {
		t.Fatal(err)
	}
	back, err := LoadSettings(a.home)
	if err != nil || back.ShellOutput != 16384 || back.ReasoningSummary {
		t.Fatalf("lost update: %+v %v", back, err)
	}
	if s := a.Settings(); s.ShellOutput != 16384 || s.ReasoningSummary {
		t.Fatalf("applied: %+v", s)
	}
}

// Editing keeps the key when none is given and a rename carries the selection;
// deleting the selected provider clears the selection.
func TestEditRenameAndDeleteProvider(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{})
	if err := a.AddProvider("q", Provider{BaseURL: "https://q.example", APIKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveProvider("q", "q2", Provider{BaseURL: "https://q2.example"}); err != nil {
		t.Fatal(err)
	}
	if p := a.Settings().Providers["q2"]; p.BaseURL != "https://q2.example" || p.APIKey != "secret" || len(a.Settings().Providers) != 2 {
		t.Fatalf("edited: %+v", a.Settings().Providers)
	}
	if a.SaveProvider("q2", "p", Provider{BaseURL: "https://x"}) == nil || a.SaveProvider("missing", "z", Provider{BaseURL: "https://x"}) == nil {
		t.Fatal("rename onto an existing or missing provider accepted")
	}
	base := a.Settings().Providers["p"].BaseURL
	if err := a.SaveProvider("p", "main", Provider{BaseURL: base}); err != nil {
		t.Fatal(err)
	}
	if s := a.Settings(); s.Provider != "main" || s.Model != "m" {
		t.Fatalf("rename lost the selection: %+v", s)
	}
	if err := a.DeleteProvider("main"); err != nil {
		t.Fatal(err)
	}
	s := a.Settings()
	if _, ok := s.Providers["main"]; ok || s.Provider != "" || s.Model != "" || s.SetupHint() == "" {
		t.Fatalf("after delete: %+v", s)
	}
	back, err := LoadSettings(a.home)
	if err != nil || back.Provider != "" || len(back.Providers) != 1 {
		t.Fatalf("saved: %+v %v", back, err)
	}
}
