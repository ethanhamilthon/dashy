package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddProviderDoesNotReloadExistingCredentials(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	path := filepath.Join(cwd, "dashy.toml")
	before := "provider='p'\nmodel='m'\n[providers.p]\nbase_url='https://example.test/v1'\napi_key='original'\n"
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(before, "original", "edited")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.AddProvider("q", Provider{BaseURL: "https://other.test/v1"}); err != nil {
		t.Fatal(err)
	}
	if got := a.Settings().Providers["p"].APIKey; got != "original" {
		t.Fatalf("adding q reloaded p's credentials: %q", got)
	}
}

func TestSelectValidatesCurrentEnvironmentWithoutReload(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	t.Setenv("AUDIT_NEW_PROVIDER_KEY", "")
	path := filepath.Join(cwd, "dashy.toml")
	before := "provider='p'\nmodel='m'\n[providers.p]\nbase_url='https://example.test/v1'\n[providers.q]\nbase_url='https://other.test/v1'\napi_key_env='AUDIT_NEW_PROVIDER_KEY'\n"
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	edited := before + "[env]\nAUDIT_NEW_PROVIDER_KEY='not-yet-reloaded'\n"
	if err := os.WriteFile(path, []byte(edited), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Select("q", "other", ""); err == nil {
		t.Fatal("selection used credentials from an unrelated disk edit")
	}
	if a.Settings().Provider != "p" {
		t.Fatal("failed selection changed active provider")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != edited {
		t.Fatalf("failed selection did not restore config: %s, %v", data, err)
	}
}
