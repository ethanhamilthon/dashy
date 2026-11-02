package control

import (
	"os"
	"path/filepath"
	"testing"

	"dashy/internal/agent"
)

func TestProjectConfigOnlyForProjectFile(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	globalPath := filepath.Join(home, "dashy.toml")
	projectPath := filepath.Join(cwd, "dashy.toml")
	if err := os.WriteFile(globalPath, []byte("model = 'm'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte("system = 'p'\n"), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := agent.New(cwd, agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := New(a, nil)

	if c.ProjectConfig(globalPath) {
		t.Fatalf("global %s reported as project config", globalPath)
	}
	if !c.ProjectConfig(projectPath) {
		t.Fatalf("project %s not reported as project config", projectPath)
	}
	if c.ProjectConfig("") {
		t.Fatal("empty destination reported as project config")
	}
}

func TestAddProviderReportsGlobalDestinationAsNotProject(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	resp := h.submit(Request{Type: "add_provider", Args: map[string]string{"name": "global", "base_url": "https://example.test/v1", "api_key": "sk-global"}})
	saved, ok := resp.Data.(ProviderSaved)
	if !ok {
		t.Fatalf("unexpected response data %T", resp.Data)
	}
	if saved.Destination != filepath.Join(os.Getenv("DASHY_HOME"), "dashy.toml") {
		t.Fatalf("destination = %q, want the global file", saved.Destination)
	}
	if saved.ProjectConfig {
		t.Fatal("global destination reported as project_config")
	}
}

func TestProjectConfigClassifiesExplicitFlagByPath(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	global := filepath.Join(home, "dashy.toml")
	if err := os.WriteFile(global, []byte("model = 'm'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(cwd, "conf", "alt.toml")
	if err := os.MkdirAll(filepath.Dir(inside), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("system = 'alt'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(cwd, "home-alias.toml")
	if err := os.Symlink(global, alias); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.toml")
	if err := os.WriteFile(outside, []byte("system = 'x'\n"), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := agent.New(cwd, agent.ConfigFiles{Config: inside})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := New(a, nil)
	if !c.ProjectConfig(inside) || !c.ProjectConfig(a.PrimaryConfig()) {
		t.Fatalf("explicit --config inside the project not reported as project config")
	}
	if c.ProjectConfig(alias) {
		t.Fatal("alias of the global file inside the project reported as project config")
	}
	if c.ProjectConfig(global) || c.ProjectConfig(outside) {
		t.Fatal("global or outside file reported as project config")
	}
}
