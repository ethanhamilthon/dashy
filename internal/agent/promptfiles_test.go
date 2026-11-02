package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFileOverridesLocalAndSurvivesReload(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()

	local := filepath.Join(cwd, "dashy.toml")
	flagged := filepath.Join(t.TempDir(), "flag.toml")

	if err := os.WriteFile(local, []byte("system = \"local\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flagged, []byte("system = \"flag\"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if a.System != "local" {
		t.Fatalf("local fallback: %q", a.System)
	}
	a.Close()

	a, err = New(cwd, ConfigFiles{Config: flagged})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.System != "flag" {
		t.Fatalf("flag: %q", a.System)
	}
	if err = a.Reload(); err != nil || a.System != "flag" {
		t.Fatalf("reload: %q %v", a.System, err)
	}
	if _, err = New(cwd, ConfigFiles{Config: "/nonexistent"}); err == nil {
		t.Fatal("missing --config file must fail")
	}
}
