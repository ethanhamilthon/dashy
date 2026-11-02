package agent

import (
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestConfigPrecedenceAndNoExtend(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	globalCfg := `
model = "global-model"
system = "global system"
[ui]
max_code_lines = 100
[env]
G_VAR = "g_val"
SHARED = "from_global"
[providers.p1]
base_url = "https://global.p1"
api_key = "k1"
[hooks.h1]
events = ["turn_end"]
command = "echo h1"
`
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(globalCfg), 0600); err != nil {
		t.Fatal(err)
	}

	projectCfg := `
model = "project-model"
[env]
P_VAR = "p_val"
SHARED = "from_project"
[providers.p1]
base_url = "https://project.p1"
[hooks.h2]
events = ["idle"]
command = "echo h2"
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(projectCfg), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Project dashy.toml merges over global
	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	s := cfg.settings
	if s.Model != "project-model" || cfg.system != "global system" || s.MaxCodeLines != 100 {
		t.Fatalf("scalar merge error: model=%q sys=%q lines=%d", s.Model, cfg.system, s.MaxCodeLines)
	}
	if s.Environment["G_VAR"] != "g_val" || s.Environment["P_VAR"] != "p_val" || s.Environment["SHARED"] != "from_project" {
		t.Fatalf("env merge error: %+v", s.Environment)
	}
	if s.Providers["p1"].BaseURL != "https://project.p1" || s.Providers["p1"].APIKey != "k1" {
		t.Fatalf("provider deep merge error: %+v", s.Providers["p1"])
	}
	if len(cfg.hooks) != 2 {
		t.Fatalf("hooks merge error: %+v", cfg.hooks)
	}

	// 2. --no-extend skips global base
	cfgNoExt, err := loadConfiguration(home, cwd, ConfigFiles{NoExtend: true})
	if err != nil {
		t.Fatalf("no-extend load failed: %v", err)
	}
	if cfgNoExt.settings.Environment["G_VAR"] != "" || cfgNoExt.settings.Providers["p1"].APIKey != "" {
		t.Fatalf("no-extend inherited global settings: %+v", cfgNoExt.settings)
	}
	if cfgNoExt.system != "You are Dashy, a coding agent" {
		t.Fatalf("no-extend must keep builtin defaults: %q", cfgNoExt.system)
	}
}

func TestConfigFalseZeroEmptyScalarsReplace(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	globalCfg := `
model = "global-model"
compact_keep_turns = 10
max_agent_steps = 50
system = "initial system"
[ui]
max_code_lines = 100
`
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(globalCfg), 0600); err != nil {
		t.Fatal(err)
	}

	projectCfg := `
compact_keep_turns = 0
max_agent_steps = 0
system = ""
[ui]
max_code_lines = 0
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(projectCfg), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	s := cfg.settings
	if s.CompactKeepTurns != 0 || s.MaxAgentSteps != 0 || s.MaxCodeLines != 0 || cfg.system != "" {
		t.Fatalf("explicit 0/empty did not replace: turns=%d steps=%d lines=%d sys=%q",
			s.CompactKeepTurns, s.MaxAgentSteps, s.MaxCodeLines, cfg.system)
	}
}

func TestConfigArraysReplace(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	globalCfg := `
[ui]
status_left = ["echo left1", "echo left2"]
status_right = ["echo right1"]
[hooks.test]
events = ["turn_start", "turn_end"]
command = "echo 1"
`
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(globalCfg), 0600); err != nil {
		t.Fatal(err)
	}

	projectCfg := `
[ui]
status_left = []
[hooks.test]
events = ["idle"]
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(projectCfg), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	s := cfg.settings
	if len(s.StatusLeft) != 0 {
		t.Fatalf("status_left was not replaced by empty array: %+v", s.StatusLeft)
	}
	if len(s.StatusRight) != 1 || s.StatusRight[0] != "echo right1" {
		t.Fatalf("status_right was not inherited: %+v", s.StatusRight)
	}
	if len(cfg.hooks) != 1 || len(cfg.hooks[0].Events) != 1 || cfg.hooks[0].Events[0] != "idle" {
		t.Fatalf("hook events array was not replaced: %+v", cfg.hooks[0])
	}
}

func TestDisabledHookMayOmitCommandAndEvents(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	globalCfg := `
[hooks.h1]
events = ["turn_end"]
command = "make test"
`
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(globalCfg), 0600); err != nil {
		t.Fatal(err)
	}

	projectCfg := `
[hooks.h1]
enabled = false
[hooks.h2]
enabled = false
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(projectCfg), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatalf("disabled hooks should not fail validation: %v", err)
	}
	if len(cfg.hooks) != 0 {
		t.Fatalf("expected 0 active hooks, got %d", len(cfg.hooks))
	}
}

func TestHookTimeoutBoundaries(t *testing.T) {
	for _, tc := range []struct {
		timeout int
		ok      bool
	}{
		{-1, false},
		{0, true}, // unset: the default applies
		{1, true},
		{600, true},
		{601, false},
	} {
		home, cwd := t.TempDir(), t.TempDir()
		t.Setenv("DASHY_HOME", home)
		text := fmt.Sprintf("[hooks.h1]\nevents = ['turn_end']\ncommand = 'true'\ntimeout = %d\n", tc.timeout)
		if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfiguration(home, cwd, ConfigFiles{})
		if tc.ok && err != nil {
			t.Fatalf("timeout %d rejected: %v", tc.timeout, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("timeout %d accepted", tc.timeout)
		}
	}
}

func TestSelectedProviderSecretResolutionAndConflict(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	t.Setenv("MY_SECRET", "super-secret-key")

	// 1. Selected provider with api_key_env resolves from effective environment
	cfgText := `
provider = "p1"
model = "m1"
[env]
MY_INDIRECT = "indirect-key"
[providers.p1]
base_url = "https://example.com"
api_key_env = "MY_INDIRECT"
[providers.p2_unused]
base_url = "https://unused.com"
api_key_env = "NONEXISTENT_VAR"
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(cfgText), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatalf("failed to resolve valid selected provider: %v", err)
	}
	if cfg.settings.Providers["p1"].APIKeyEnv != "MY_INDIRECT" {
		t.Fatalf("APIKeyEnv not preserved: %+v", cfg.settings.Providers["p1"])
	}

	// 2. Selected provider missing env var must fail
	cfgTextBad := `
provider = "p1"
model = "m1"
[providers.p1]
base_url = "https://example.com"
api_key_env = "MISSING_ENV_VAR"
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(cfgTextBad), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(home, cwd, ConfigFiles{}); err == nil {
		t.Fatal("expected error for missing selected provider env var")
	}

	// 3. Provider with both api_key and api_key_env conflicts
	cfgTextConflict := `
provider = "p1"
model = "m1"
[providers.p1]
base_url = "https://example.com"
api_key = "direct-key"
api_key_env = "MY_SECRET"
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(cfgTextConflict), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(home, cwd, ConfigFiles{}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected conflict error: %v", err)
	}

	// 4. Reserved DASHY_* env key rejected
	cfgTextReserved := `
[env]
DASHY_CUSTOM = "bad"
`
	if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(cfgTextReserved), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfiguration(home, cwd, ConfigFiles{}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved env key error: %v", err)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	for _, tc := range []struct {
		text    string
		wantKey string
	}{
		{"unknown_field = 'val'\n", "unknown_field"},
		{"[ui]\nunknown_ui = 1\n", "unknown_ui"},
		{"[shell]\nunknown_shell = true\n", "unknown_shell"},
		{"[hooks.h1]\nevents = ['idle']\ncommand = 'true'\nunknown_hook = 5\n", "unknown_hook"},
		{"[hooks.h2]\nevent = 'idle'\ncommand = 'true'\n", "event"},
		{"[ui]\nstatus-left = ['echo left']\n", "status-left"},
		{"[ui]\nstatus-right = ['echo right']\n", "status-right"},
	} {
		if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(tc.text), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfiguration(home, cwd, ConfigFiles{})
		if err == nil {
			t.Fatalf("unknown field accepted: %s", tc.text)
		}
		if !strings.Contains(err.Error(), tc.wantKey) {
			t.Fatalf("expected error to name unknown key %q, got: %v", tc.wantKey, err)
		}
	}
}

func TestMissingExplicitConfigFails(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	missing := filepath.Join(t.TempDir(), "nonexistent.toml")
	if _, err := loadConfiguration(home, cwd, ConfigFiles{Config: missing}); err == nil {
		t.Fatal("missing explicit config must fail without fallback")
	}
}

func TestSameFileAliasLoadedOnlyOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	globalPath := filepath.Join(home, "dashy.toml")
	if err := os.WriteFile(globalPath, []byte("model = 'm1'\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// cwd is $DASHY_HOME, so ./dashy.toml is the global file itself.
	cfg, err := loadConfiguration(home, home, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.sources) != 1 || cfg.sources[0].Origin != "home" {
		t.Fatalf("expected 1 home source for same file alias, got %+v", cfg.sources)
	}
	if cfg.local != "" || cfg.primary != globalPath {
		t.Fatalf("global file treated as project: local=%q primary=%q", cfg.local, cfg.primary)
	}

	// A symlink to the global file in another project is the same file too.
	cwd := t.TempDir()
	if err := os.Symlink(globalPath, filepath.Join(cwd, "dashy.toml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg, err = loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.sources) != 1 || cfg.sources[0].Origin != "home" || cfg.local != "" {
		t.Fatalf("symlink alias of global file treated as project: sources=%+v local=%q", cfg.sources, cfg.local)
	}
}

func TestLosslessSparseWritesAndRollback(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	// Primary dashy.toml has comments and custom ui
	initialProject := `# Custom Project Config
system = "custom system instructions"

[ui]
# Preserve this comment
max_code_lines = 42

[providers.other]
base_url = "https://other.example"
api_key = "other-key"
`
	projectPath := filepath.Join(cwd, "dashy.toml")
	if err := os.WriteFile(projectPath, []byte(initialProject), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Select updates provider, model, effort sparsely
	if err = a.AddProvider("p_new", Provider{BaseURL: "https://new.example", APIKey: "k_new"}); err != nil {
		t.Fatal(err)
	}
	if err = a.Select("p_new", "my-model", "high"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Check unrelated values, formatting, and comments were preserved
	if !strings.Contains(content, "# Custom Project Config") ||
		!strings.Contains(content, "# Preserve this comment") ||
		!strings.Contains(content, "max_code_lines = 42") ||
		!strings.Contains(content, "custom system instructions") ||
		!strings.Contains(content, "other") {
		t.Fatalf("unrelated values or comments lost: %s", content)
	}

	// Check new values sparsely added
	if !strings.Contains(content, "p_new") ||
		!strings.Contains(content, "my-model") ||
		!strings.Contains(content, "high") {
		t.Fatalf("sparse keys not added: %s", content)
	}

	// Rollback on write failure: invalid effort fails and preserves file
	backupContent := content
	if err = a.Select("p_new", "my-model", "unsupported-effort"); err == nil {
		t.Fatal("expected error on unsupported effort")
	}
	afterFail, _ := os.ReadFile(projectPath)
	if string(afterFail) != backupContent {
		t.Fatalf("rollback did not restore file: %s", afterFail)
	}
}

func TestPatchWithMultilineApparentHeadersAndAssignments(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	multilineSystem := `[fake_section]
model = "fake-model"
provider = "fake-provider"
[providers.fake]
api_key = "fake_key"
`
	configContent := fmt.Sprintf(`system = %s
provider = "orig"
model = "orig-m"

[providers.orig]
base_url = "https://orig.example"
api_key = "secret_key"
`, strconv.Quote(multilineSystem))

	configPath := filepath.Join(home, "dashy.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// 1. Select updates root keys without corrupting multiline system containing fake headers
	if err = a.Select("orig", "updated-model", "high"); err != nil {
		t.Fatalf("Select failed: %v", err)
	}

	rawBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = toml.Unmarshal(rawBytes, &decoded); err != nil {
		t.Fatalf("unmarshal patched config: %v", err)
	}
	if decoded["system"] != multilineSystem {
		t.Fatalf("multiline system was corrupted by patchRootKeys!\ngot:\n%v\nwant:\n%v", decoded["system"], multilineSystem)
	}
	if decoded["model"] != "updated-model" || decoded["effort"] != "high" || decoded["provider"] != "orig" {
		t.Fatalf("root keys not properly updated: %v", decoded)
	}

	// 2. AddProvider adds new provider without corrupting multiline system containing fake headers
	if err = a.AddProvider("p_added", Provider{BaseURL: "https://added.example", APIKey: "added_k"}); err != nil {
		t.Fatalf("AddProvider failed: %v", err)
	}

	rawBytes, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	decoded = nil
	if err = toml.Unmarshal(rawBytes, &decoded); err != nil {
		t.Fatalf("unmarshal patched config after AddProvider: %v", err)
	}
	if decoded["system"] != multilineSystem {
		t.Fatalf("multiline system was corrupted by patchProvider!\ngot:\n%v\nwant:\n%v", decoded["system"], multilineSystem)
	}
	provs, ok := decoded["providers"].(map[string]any)
	if !ok || provs["p_added"] == nil || provs["orig"] == nil {
		t.Fatalf("providers missing after AddProvider: %+v", decoded)
	}
}

func TestConfigMaxFileBytesLayers(t *testing.T) {
	for _, tc := range []struct {
		name, global, project string
		want                  int
		invalid               bool
	}{
		{"builtin default", "", "", 65536, false},
		{"global overrides default", "max_file_bytes = 1024\n", "", 1024, false},
		{"project overrides global", "max_file_bytes = 1024\n", "max_file_bytes = 2048\n", 2048, false},
		{"explicit zero overrides inherited", "max_file_bytes = 1024\n", "max_file_bytes = 0\n", 0, false},
		{"negative rejected", "", "max_file_bytes = -1\n", 0, true},
		{"negative global rejected", "max_file_bytes = -5\n", "", 0, true},
		{"unknown key rejected", "", "max_file_byte = 1\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			if tc.global != "" {
				if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(tc.global), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.project != "" {
				if err := os.WriteFile(filepath.Join(cwd, "dashy.toml"), []byte(tc.project), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
			if tc.invalid {
				if err == nil {
					t.Fatalf("expected error, got max_file_bytes=%d", cfg.settings.MaxFileBytes)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.settings.MaxFileBytes != tc.want {
				t.Fatalf("max_file_bytes = %d, want %d", cfg.settings.MaxFileBytes, tc.want)
			}
		})
	}
}

// A leftover config.toml from earlier releases is simply ignored: it is neither
// parsed nor reported, even when its contents would be invalid.
func TestLeftoverConfigTomlIsIgnored(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = 'leftover'\nprovider = [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatalf("leftover config.toml must be ignored: %v", err)
	}
	if cfg.settings.Model != "" || cfg.settings.Provider != "" {
		t.Fatalf("leftover config.toml was applied: model=%q provider=%q", cfg.settings.Model, cfg.settings.Provider)
	}
	for _, src := range cfg.sources {
		if filepath.Base(src.Path) == "config.toml" {
			t.Fatalf("leftover config.toml listed as a source: %+v", cfg.sources)
		}
	}
}
