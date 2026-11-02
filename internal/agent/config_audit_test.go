package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSelectWritesGlobalSourceWithoutFlattening(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	global := filepath.Join(home, "dashy.toml")
	write(t, global, "provider = 'p'\nmodel = 'g-model'\neffort = 'low'\n[providers.p]\nbase_url = 'https://p.example/v1'\napi_key = 'k'\n")
	local := filepath.Join(cwd, "dashy.toml")
	write(t, local, "system = 'local system'\n")
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.SelectionConfig() != global {
		t.Fatalf("model origin = %q, want global", a.SelectionConfig())
	}
	if err := a.Select("p", "new-model", "high"); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, local); got != "system = 'local system'\n" {
		t.Fatalf("project file changed: %q", got)
	}
	if got := readText(t, global); !strings.Contains(got, "new-model") || !strings.Contains(got, "[providers.p]") {
		t.Fatalf("global file not updated sparsely: %q", got)
	}
}

func TestSelectWritesLocalAndRoleSources(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	global := filepath.Join(home, "dashy.toml")
	write(t, global, "provider = 'p'\nmodel = 'g'\n[providers.p]\nbase_url = 'https://p.example/v1'\n")
	local := filepath.Join(cwd, "dashy.toml")
	write(t, local, "model = 'local'\n")
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Select("p", "x", ""); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, global); strings.Contains(got, "model = 'x'") || !strings.Contains(got, "model = 'g'") {
		t.Fatalf("global changed for a local model: %q", got)
	}
	if got := readText(t, local); !strings.Contains(got, "x") {
		t.Fatalf("local file not written: %q", got)
	}
	a.Close()

	role := filepath.Join(cwd, "roles", "dev.toml")
	write(t, role, "model = 'role-model'\n")
	base := readText(t, local)
	a, err = New(cwd, ConfigFiles{Agent: "roles/dev.toml"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.SelectionConfig() != role {
		t.Fatalf("model origin = %q, want role", a.SelectionConfig())
	}
	if err := a.Select("p", "role-2", ""); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, role); !strings.Contains(got, "role-2") {
		t.Fatalf("role file not written: %q", got)
	}
	if got := readText(t, local); got != base {
		t.Fatalf("base file changed by a role selection: %q", got)
	}
}

func TestSelectShadowedInheritedTupleIsRejected(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	global := filepath.Join(home, "dashy.toml")
	text := "provider = 'p'\nmodel = 'g'\n[providers.p]\nbase_url = 'https://p.example/v1'\n"
	write(t, global, text)
	write(t, filepath.Join(cwd, "dashy.toml"), "effort = 'high'\n")
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	before := a.Settings()
	err = a.Select("p", "m2", "low")
	if err == nil || !strings.Contains(err.Error(), "overrides") {
		t.Fatalf("shadowed selection accepted: %v", err)
	}
	if got := readText(t, global); got != text {
		t.Fatalf("rejected selection changed the global file: %q", got)
	}
	if a.Settings().Model != before.Model || a.Settings().Effort != before.Effort {
		t.Fatal("rejected selection changed settings")
	}
	if err := a.Select("p", "m2", "high"); err != nil {
		t.Fatalf("matching value rejected: %v", err)
	}
}

func TestAddProviderKeepsSessionSelectionAndRuntimeSettings(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	global := filepath.Join(home, "dashy.toml")
	write(t, global, "provider = 'p'\nmodel = 'm'\nsystem = 'start'\n[ui]\nmax_code_lines = 10\n[providers.p]\nbase_url = 'https://p.example/v1'\n")
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Disk edits are unrelated to adding r and must wait for an explicit reload.
	write(t, global, "provider = 'q'\nmodel = 'disk-model'\nsystem = 'edited'\n[ui]\nmax_code_lines = 99\n[providers.p]\nbase_url = 'https://p.example/v1'\n[providers.q]\nbase_url = 'https://q.example/v1'\n")
	if err := a.AddProvider("r", Provider{BaseURL: "https://r.example/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	s := a.Settings()
	if s.Provider != "p" || s.Model != "m" || a.System != "start" || s.MaxCodeLines != 10 {
		t.Fatalf("AddProvider reloaded unrelated state: %+v system=%q", s, a.System)
	}
	if _, ok := s.Providers["q"]; ok {
		t.Fatal("adding r loaded an unrelated provider from disk")
	}
	if _, ok := s.Providers["r"]; !ok {
		t.Fatal("new provider missing from registry")
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Settings().Providers["q"]; !ok {
		t.Fatal("explicit reload did not load q")
	}
}

func TestConcurrentAddProviderProcessesKeepAllProviders(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("p%d", i)
		cmd := exec.Command(os.Args[0], "-test.run=^TestConfigLockHelperProcess$")
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "DASHY_HOME="+home, "DASHY_TEST_ADD_PROVIDER="+name)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("%s: %v\n%s", name, err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	cfg, err := loadConfiguration(home, cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, ok := cfg.settings.Providers[fmt.Sprintf("p%d", i)]; !ok {
			t.Fatalf("lost update: provider p%d missing from %s", i, readText(t, cfg.primary))
		}
	}
}

// TestConfigLockHelperProcess is the child of TestConcurrentAddProviderProcessesKeepAllProviders.
func TestConfigLockHelperProcess(t *testing.T) {
	name := os.Getenv("DASHY_TEST_ADD_PROVIDER")
	if name == "" {
		t.Skip("helper process only")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.AddProvider(name, Provider{BaseURL: "https://" + name + ".example/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigWritesFollowSymlinkAndStayPrivate(t *testing.T) {
	home, cwd, store := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	target := filepath.Join(store, "dashy.toml")
	write(t, target, "system = 'kept'\n")
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "dashy.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.AddProvider("p", Provider{BaseURL: "https://p.example/v1", APIKey: "secret"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Select("p", "m", "low"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("alias replaced: %v %v", info, err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("target perms = %v (%v), want 0600", info.Mode().Perm(), err)
	}
	if got := readText(t, target); !strings.Contains(got, "kept") || !strings.Contains(got, "p.example") {
		t.Fatalf("target content: %q", got)
	}
}

// probeEnv fires a turn_end hook and returns the PROBE value the hook saw.
func probeEnv(t *testing.T, a *Agent, out string) string {
	t.Helper()
	os.Remove(out)
	a.Fire("turn_end", "DASHY_TURN_STATUS", "ok")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(out); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hook did not run")
	return ""
}

func TestFailedResumeKeepsHooksEnvAndSession(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	out := filepath.Join(t.TempDir(), "probe")
	global := filepath.Join(home, "dashy.toml")
	hook := func(value string) string {
		return "[env]\nPROBE = '" + value + "'\n[hooks.probe]\nevents = ['turn_end']\ncommand = 'printf %s \"$PROBE\" > " + out + "'\n"
	}
	write(t, global, hook("old"))
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	write(t, global, hook("new"))
	if err := a.Resume("missing-session"); err == nil {
		t.Fatal("resume of a missing session succeeded")
	}
	if strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl") != id {
		t.Fatal("failed resume changed the session")
	}
	if got := probeEnv(t, a, out); got != "old" {
		t.Fatalf("failed resume changed hook env: %q", got)
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if got := probeEnv(t, a, out); got != "new" {
		t.Fatalf("new session did not apply hook env: %q", got)
	}
}

func TestRoleOverlayPrecedenceDedupAndNoExtend(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	write(t, filepath.Join(home, "dashy.toml"), "system = 'global'\nmodel = 'global-model'\n[ui]\nmax_code_lines = 5\n")
	project := filepath.Join(cwd, "dashy.toml")
	write(t, project, "system = 'project'\n")
	role := filepath.Join(cwd, "roles", "dev.toml")
	write(t, role, "system = 'role'\n[ui]\nmax_code_lines = 7\n")

	a, err := New(cwd, ConfigFiles{Agent: "roles/dev.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if a.System != "role" || a.Settings().Model != "global-model" || a.Settings().MaxCodeLines != 7 {
		t.Fatalf("role not applied last: system=%q model=%q lines=%d", a.System, a.Settings().Model, a.Settings().MaxCodeLines)
	}
	if len(a.Sources) != 3 || a.Sources[2].Origin != "role" || a.Sources[2].Path != role {
		t.Fatalf("sources = %+v", a.Sources)
	}
	// Adding a provider writes the base file, never the role file.
	roleBefore := readText(t, role)
	if err := a.AddProvider("added", Provider{BaseURL: "https://added.example/v1", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if readText(t, role) != roleBefore || !strings.Contains(readText(t, project), "added") {
		t.Fatal("/login wrote the role file instead of the primary file")
	}
	a.Close()

	// A role that is the primary file is read once, at its role position.
	a, err = New(cwd, ConfigFiles{Agent: "dashy.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Sources) != 2 || a.System != "project" {
		t.Fatalf("duplicate role read: sources=%+v system=%q", a.Sources, a.System)
	}
	a.Close()

	a, err = New(cwd, ConfigFiles{NoExtend: true, Agent: "roles/dev.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Settings().Model != "" || a.System != "role" {
		t.Fatalf("--no-extend: model=%q system=%q", a.Settings().Model, a.System)
	}
	a.Close()

	if _, err := New(cwd, ConfigFiles{Agent: "missing.toml"}); err == nil {
		t.Fatal("missing role file accepted")
	}
}

// TestRoleAliasOfLayerIsAppliedLastOnce checks that a role reaching a layer through
// a symlink is applied last and the file is not applied a second time at the old spot.
func TestRoleAliasOfLayerIsAppliedLastOnce(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	global := filepath.Join(home, "dashy.toml")
	write(t, global, "system = 'global'\nmodel = 'global-model'\neffort = 'low'\n")
	project := filepath.Join(cwd, "dashy.toml")
	write(t, project, "system = 'project'\nmodel = 'project-model'\n")
	aliasGlobal := filepath.Join(cwd, "alias-global.toml")
	aliasProject := filepath.Join(cwd, "alias-project.toml")
	if err := os.Symlink(global, aliasGlobal); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project, aliasProject); err != nil {
		t.Fatal(err)
	}

	// The global file as role: it wins over the project file, and is read once.
	a, err := New(cwd, ConfigFiles{Agent: aliasGlobal})
	if err != nil {
		t.Fatal(err)
	}
	if a.System != "global" || a.Settings().Model != "global-model" || a.Settings().Effort != "low" {
		t.Fatalf("global alias not applied last: system=%q model=%q effort=%q", a.System, a.Settings().Model, a.Settings().Effort)
	}
	if len(a.Sources) != 2 || a.Sources[1].Origin != "role" || a.Sources[0].Path != project {
		t.Fatalf("sources = %+v", a.Sources)
	}
	// Selection writes through the alias to the real global file; the project file is untouched.
	projectBefore := readText(t, project)
	if a.SelectionConfig() != aliasGlobal {
		t.Fatalf("model origin = %q, want the role alias", a.SelectionConfig())
	}
	if err := a.Select("", "picked", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readText(t, global), "picked") || readText(t, project) != projectBefore {
		t.Fatal("selection did not reach the global file through the alias")
	}
	if info, err := os.Lstat(aliasGlobal); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("alias replaced: %v %v", info, err)
	}
	a.Close()

	// The project file as role: it wins over the global file.
	a, err = New(cwd, ConfigFiles{Agent: aliasProject})
	if err != nil {
		t.Fatal(err)
	}
	if a.System != "project" || a.Settings().Model != "project-model" || len(a.Sources) != 2 {
		t.Fatalf("project alias: system=%q model=%q sources=%+v", a.System, a.Settings().Model, a.Sources)
	}
	a.Close()

	// --no-extend: the global file is not a layer, but a role alias of it still applies.
	a, err = New(cwd, ConfigFiles{NoExtend: true, Agent: aliasGlobal})
	if err != nil {
		t.Fatal(err)
	}
	if a.System != "global" || a.Settings().Model != "picked" || len(a.Sources) != 2 {
		t.Fatalf("--no-extend with alias role: system=%q model=%q sources=%+v", a.System, a.Settings().Model, a.Sources)
	}
	a.Close()
}
