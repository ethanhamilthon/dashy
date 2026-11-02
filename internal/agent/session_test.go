package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFileAndNewSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	dir := t.TempDir()

	cfg := "system = 'Custom system instructions.'\n"
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.System != "Custom system instructions." {
		t.Fatalf("unexpected system: %q", a.System)
	}

	a.message("user", "old chat")
	if err = a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if len(a.History) != 0 || a.System != "Custom system instructions." {
		t.Fatal("new session must clear history and reload config")
	}

	next, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if len(next.History) != 0 || next.System != "Custom system instructions." {
		t.Fatal("existing config must not be overwritten")
	}
}

func TestReloadFailurePreservesSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.message("user", "keep history")
	a.records = []Record{{Kind: "user", Text: "keep history"}}
	previousSystem := a.System

	if err = os.WriteFile(filepath.Join(a.home, "dashy.toml"), []byte("invalid toml [["), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.NewSession(); err == nil {
		t.Fatal("expected reload error")
	}
	if len(a.History) != 1 || len(a.Records()) != 1 || a.System != previousSystem {
		t.Fatal("reload mutated session")
	}
}

func TestSaveFailurePreservesSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.message("user", "keep")
	a.records = []Record{{Kind: "user", Text: "keep"}}

	if err = os.Mkdir(a.HistoryPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err = a.NewSession(); err == nil {
		t.Fatal("expected save error")
	}
	if len(a.History) != 1 || len(a.Records()) != 1 {
		t.Fatal("save failure discarded history")
	}
}

func TestProjectLocalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	cwd := t.TempDir()

	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(home, "dashy.toml"), "system = 'global system'\nmodel = 'global-model'\n")
	write(filepath.Join(cwd, "dashy.toml"), "system = 'local system'\nmodel = 'local-model'\n")

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.System != "local system" || a.Settings().Model != "local-model" || a.LocalConfig != cwd {
		t.Fatalf("local config not applied: system=%q model=%q local=%q", a.System, a.Settings().Model, a.LocalConfig)
	}

	write(filepath.Join(cwd, "dashy.toml"), "system = 'changed'\nmodel = 'local-model'\n")
	if err = a.Reload(); err != nil || a.System != "changed" {
		t.Fatalf("reload failed: %q %v", a.System, err)
	}
}

func TestLocalConfigIgnoredWhenItIsHome(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", cwd)
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte("system = 'test'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.LocalConfig != "" {
		t.Fatalf("home file treated as project: LocalConfig = %q", a.LocalConfig)
	}
	if len(a.Sources) != 1 || a.Sources[0].Origin != "home" {
		t.Fatalf("expected exactly one home source, got %+v", a.Sources)
	}
}
