package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func configureTestAgent(t *testing.T, a *Agent, s Settings) {
	t.Helper()
	if s.ShellTimeout <= 0 {
		s.ShellTimeout = 120
	}
	if s.ShellMaxTimeout <= 0 {
		s.ShellMaxTimeout = 600
	}
	if s.ShellMaxOutput < 0 {
		s.ShellMaxOutput = 8192
	}
	a.applySettings(s)
	if a.home == "" {
		return
	}
	cf := rawConfig{
		Provider:  &s.Provider,
		Model:     &s.Model,
		Effort:    &s.Effort,
		Providers: map[string]rawProvider{},
	}
	for k, p := range s.Providers {
		b, k1, k2 := p.BaseURL, p.APIKey, p.APIKeyEnv
		cf.Providers[k] = rawProvider{BaseURL: &b, APIKey: &k1, APIKeyEnv: &k2}
	}
	data, err := toml.Marshal(cf)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(a.home, "dashy.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSessionSelectionDefaultsAndRestoration(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	original := a.Settings()
	if err = a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err = a.Select(original.Provider, "new-model", "max"); err != nil {
		t.Fatal(err)
	}
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	selected := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err = a.Resume(first); err != nil {
		t.Fatal(err)
	}
	if a.Settings().Model != original.Model || a.Settings().Effort != original.Effort {
		t.Fatal("old session adopted new defaults")
	}
	if err = a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if a.Settings().Model != "new-model" || a.Settings().Effort != "max" {
		t.Fatal("new session did not use new defaults")
	}
	if err = a.Resume(selected); err != nil {
		t.Fatal(err)
	}
	if a.Settings().Model != "new-model" {
		t.Fatal("session model not restored")
	}
	if err = a.Select(original.Provider, "bad", "unsupported"); err == nil {
		t.Fatal("invalid effort accepted")
	}
	if a.Settings().Model != "new-model" {
		t.Fatal("failed selection mutated settings")
	}
}

func TestSessionGlobalAndContextMatchesRecords(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	n := int64(36679)
	a.records = []Record{{Kind: "assistant", Meta: &Metadata{RequestID: "r-1", ContextInput: &n}}}
	result, err := a.Shell.Execute(context.Background(), `echo "$DASHY_CTX_SIZE $DASHY_SESSION_ID"; sh -c 'echo "$DASHY_ENDPOINT"'`, time.Second)
	id := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err != nil || result.Error || !strings.Contains(result.Output, "36679 "+id) || !strings.Contains(result.Output, filepath.Join(a.socketDir, "socket")) {
		t.Fatalf("metadata: %+v %v", result, err)
	}
	a.records = append(a.records, Record{Kind: "assistant", Meta: &Metadata{RequestID: "r-2"}})
	result, err = a.Shell.Execute(context.Background(), `echo "[$DASHY_CTX_SIZE]"`, time.Second)
	if err != nil || result.Error || result.Output != "[]\n" {
		t.Fatalf("unknown context must be empty: %+v %v", result, err)
	}
}

func TestModelListUsesSelectedProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer selected" {
			t.Error("wrong endpoint or credentials")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"z"},{"id":"a"}]}`)
	}))
	defer server.Close()
	a := &Agent{}
	a.applySettings(Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL, APIKey: "selected"}}})
	models, errs := AllModels(context.Background(), a.Settings().Providers)
	if len(errs) != 0 || len(models) != 2 || models[0].Model != "a" || models[1].Model != "z" {
		t.Fatalf("%v %v", models, errs)
	}
}

func TestLegacyHeaderPinsCurrentDefaultsAndCorruptSelectionPreservesState(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.HistoryPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	var header map[string]any
	if err = json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	delete(header, "selection")
	lines[0] = string(raw(header))
	if err = os.WriteFile(a.HistoryPath, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.readSession(); err != nil {
		t.Fatal(err)
	}
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(a.HistoryPath)
	if err != nil || !strings.Contains(string(data), `"selection"`) {
		t.Fatal("legacy defaults not pinned")
	}
	previous := a.Settings()
	header["selection"] = Selection{Provider: "missing", Model: "bad"}
	lines[0] = string(raw(header))
	if err = os.WriteFile(a.HistoryPath, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.readSession(); err == nil {
		t.Fatal("unavailable provider accepted")
	}
	if a.Settings().Provider != previous.Provider {
		t.Fatal("failed read mutated selection")
	}
}

func TestReloadPreservesSessionSelectionAndReloadsSettings(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	s := a.Settings()
	s.Providers = map[string]Provider{
		"first":  {BaseURL: "https://first.example", APIKey: "old"},
		"second": {BaseURL: "https://second.example"},
	}
	s.Provider, s.Model, s.Effort = "first", "session-model", "high"
	configureTestAgent(t, a, s)

	// Update on-disk config with different global defaults
	text := `provider = "second"
model = "global-model"
effort = "low"
[ui]
max_code_lines = 17
[providers.first]
base_url = "https://updated.example"
api_key = "new"
[providers.second]
base_url = "https://second.example"
`
	if err = os.WriteFile(filepath.Join(a.home, "dashy.toml"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Reload(); err != nil {
		t.Fatal(err)
	}
	got := a.Settings()
	if got.Provider != "first" || got.Model != "session-model" || got.Effort != "high" {
		t.Fatalf("session selection changed: %+v", got)
	}
	if got.Providers["first"].BaseURL != "https://updated.example" || got.MaxCodeLines != 17 {
		t.Fatalf("other settings not reloaded: %+v", got)
	}

	// Delete provider that is currently selected in session -> reload must fail
	textBad := `provider = "second"
model = "global-model"
[providers.second]
base_url = "https://second.example"
`
	if err = os.WriteFile(filepath.Join(a.home, "dashy.toml"), []byte(textBad), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Reload(); err == nil {
		t.Fatal("reload accepted a missing session provider")
	}
	if a.Settings().Provider != "first" || a.Settings().Model != "session-model" {
		t.Fatal("failed reload changed selection")
	}
}

func TestAllModelsResolvesAPIKeyEnvFromEnvironment(t *testing.T) {
	authenticated := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer env-secret-123" {
			authenticated = true
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"model-from-env"}]}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	defer server.Close()

	providers := map[string]Provider{
		"env_prov": {
			BaseURL:   server.URL,
			APIKeyEnv: "CUSTOM_SECRET_ENV",
		},
	}
	env := map[string]string{
		"CUSTOM_SECRET_ENV": "env-secret-123",
	}

	// 1. With effective environment passed as 3rd arg: succeeds
	choices, errs := AllModels(context.Background(), providers, env)
	if len(errs) != 0 || len(choices) != 1 || choices[0].Model != "model-from-env" || !authenticated {
		t.Fatalf("expected successful listing with env, got choices=%v, errs=%v", choices, errs)
	}

	// 2. Without environment (2-arg call): fails auth since variable is not in os.Environ
	choices, errs = AllModels(context.Background(), providers)
	if len(errs) == 0 {
		t.Fatalf("expected error without env, got choices=%v", choices)
	}
}

func TestAddProviderRejectsExisting(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	// Create initial config with provider "existing"
	initialConfig := `provider = "existing"
model = "m1"
[providers.existing]
base_url = "https://existing.example"
api_key = "key1"
`
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(initialConfig), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// AddProvider with "existing" must be rejected
	err = a.AddProvider("existing", Provider{BaseURL: "https://new.example", APIKey: "key2"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got: %v", err)
	}

	// Explicit model selection on existing provider must still work
	if err = a.Select("existing", "new-model-id", "high"); err != nil {
		t.Fatalf("Select on existing provider should work, got: %v", err)
	}
	if a.Settings().Model != "new-model-id" || a.Settings().Effort != "high" {
		t.Fatalf("settings not updated: %+v", a.Settings())
	}
}

// Without a project dashy.toml, $DASHY_HOME/dashy.toml is both the global layer
// that is read and the file that /model writes.
func TestGlobalDashyTomlIsReadAndWrittenWithoutProjectFile(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	global := filepath.Join(home, "dashy.toml")
	text := "provider = 'p'\nmodel = 'm1'\n\n[providers.p]\nbase_url = 'https://example.com/v1'\napi_key = 'k'\n"
	if err := os.WriteFile(global, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Settings().Model != "m1" || a.PrimaryConfig() != global {
		t.Fatalf("global dashy.toml not loaded: model=%q primary=%q", a.Settings().Model, a.PrimaryConfig())
	}
	if err := a.Select("p", "m2", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.settings.Model != "m2" || cfg.primary != global {
		t.Fatalf("/model did not write %s: model=%q primary=%q", global, cfg.settings.Model, cfg.primary)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Fatalf("selection wrote a config.toml: %v", err)
	}
}
