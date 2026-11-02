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
)

func configureTestAgent(t *testing.T, a *Agent, s Settings) {
	t.Helper()
	if s.ShowLines == "" {
		s.ShowLines = "short"
	}
	a.applySettings(s)
	if a.home != "" {
		_ = SaveSettings(filepath.Join(a.home, "settings.json"), s)
	}
}

func TestSessionSelectionDefaultsAndRestoration(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{
		Provider: "default-prov",
		Model:    "default-model",
		Providers: map[string]Provider{
			"default-prov": {BaseURL: "https://example.com/v1"},
		},
	})
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	first := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	original := a.Settings()
	if err = a.NewSession(); err != nil {
		t.Fatal(err)
	}
	a.ApplySettings(original)
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
	result, err := a.Shell.Execute(context.Background(), `echo "$DASHY_SESSION_ID"; sh -c 'echo "$DASHY_ENDPOINT"'`, time.Second)
	id := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err != nil || result.Error || !strings.Contains(result.Output, id) || !strings.Contains(result.Output, filepath.Join(a.socketDir, "socket")) {
		t.Fatalf("metadata: %+v %v", result, err)
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

	choices, errs := AllModels(context.Background(), providers, env)
	if len(errs) != 0 || len(choices) != 1 || choices[0].Model != "model-from-env" || !authenticated {
		t.Fatalf("expected successful listing with env, got choices=%v, errs=%v", choices, errs)
	}

	choices, errs = AllModels(context.Background(), providers)
	if len(errs) == 0 {
		t.Fatalf("expected error without env, got choices=%v", choices)
	}
}

func TestAddProviderRejectsExisting(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.ApplySettings(Settings{
		Provider: "existing",
		Model:    "m1",
		Providers: map[string]Provider{
			"existing": {BaseURL: "https://existing.example", APIKey: "key1"},
		},
	})

	err = a.AddProvider("existing", Provider{BaseURL: "https://new.example", APIKey: "key2"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected 'already exists' error, got: %v", err)
	}

	if err = a.Select("existing", "new-model-id", "high"); err != nil {
		t.Fatalf("Select on existing provider should work, got: %v", err)
	}
	if a.Settings().Model != "new-model-id" || a.Settings().Effort != "high" {
		t.Fatalf("settings not updated: %+v", a.Settings())
	}
}
