package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFileValidation(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()

	flagged := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(flagged, []byte("# Agents\n"), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{Config: flagged})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if _, err = New(cwd, ConfigFiles{Config: "/nonexistent"}); err == nil {
		t.Fatal("missing --config file must fail")
	}
}
