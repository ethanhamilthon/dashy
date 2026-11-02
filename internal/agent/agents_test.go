package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentsAssemblyOrderingAndNoGlobal(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	globalFile := filepath.Join(home, "AGENTS.md")
	if err := os.WriteFile(globalFile, []byte("Global instructions.\n"), 0600); err != nil {
		t.Fatal(err)
	}

	localFile := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(localFile, []byte("Project instructions.\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. With NoGlobal: false -> both files in order: global then local, separated by blank line
	a, err := New(cwd, ConfigFiles{NoGlobal: false})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	prompt := a.instructions()
	wantHeader1 := "# " + globalFile
	wantHeader2 := "# " + localFile

	if !strings.Contains(prompt, wantHeader1) {
		t.Fatalf("prompt missing global header %q:\n%s", wantHeader1, prompt)
	}
	if !strings.Contains(prompt, wantHeader2) {
		t.Fatalf("prompt missing local header %q:\n%s", wantHeader2, prompt)
	}

	idx1 := strings.Index(prompt, wantHeader1)
	idx2 := strings.Index(prompt, wantHeader2)
	if idx1 >= idx2 {
		t.Fatalf("expected global before local in prompt order: idx1=%d, idx2=%d", idx1, idx2)
	}

	// Verify blank line separation between sections
	expected := fmt.Sprintf("# %s\nGlobal instructions.\n\n# %s\nProject instructions.\n", globalFile, localFile)
	if prompt != expected {
		t.Fatalf("assembled prompt mismatch:\ngot:\n%q\nwant:\n%q", prompt, expected)
	}

	// 2. With NoGlobal: true -> only local file
	aNoGlobal, err := New(cwd, ConfigFiles{NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer aNoGlobal.Close()

	promptNoGlobal := aNoGlobal.instructions()
	if strings.Contains(promptNoGlobal, wantHeader1) {
		t.Fatalf("NoGlobal prompt contains global header %q:\n%s", wantHeader1, promptNoGlobal)
	}
	if !strings.Contains(promptNoGlobal, wantHeader2) {
		t.Fatalf("NoGlobal prompt missing local header %q:\n%s", wantHeader2, promptNoGlobal)
	}
	expectedNoGlobal := fmt.Sprintf("# %s\nProject instructions.\n", localFile)
	if promptNoGlobal != expectedNoGlobal {
		t.Fatalf("assembled prompt mismatch:\ngot:\n%q\nwant:\n%q", promptNoGlobal, expectedNoGlobal)
	}
}

func TestAgentsNestedFences(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	content := "# Rules\n\nPreamble text\n\n> ```dashy\n> echo quote_marker\n> ```\n\n- ```dashy\n  echo list_marker\n  ```\n\n```bash\necho bash_marker\n```\n\n```dashy extra\necho extra_marker\n```\n\n````dashy\ncat << 'EOF'\n```\necho nested\n```\nEOF\n````\n\nFooter text\n"

	localFile := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(localFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	prompt := a.instructions()

	if !strings.Contains(prompt, "> ```dashy") || strings.Contains(prompt, "\nquote_marker\n") {
		t.Fatalf("nested fence inside blockquote was executed or altered:\n%s", prompt)
	}
	if !strings.Contains(prompt, "- ```dashy") || strings.Contains(prompt, "\nlist_marker\n") {
		t.Fatalf("nested fence inside list was executed or altered:\n%s", prompt)
	}
	if !strings.Contains(prompt, "```bash") || strings.Contains(prompt, "\nbash_marker\n") {
		t.Fatalf("code block with non-dashy info was executed or altered:\n%s", prompt)
	}
	if !strings.Contains(prompt, "```dashy extra") || strings.Contains(prompt, "\nextra_marker\n") {
		t.Fatalf("dashy block with extra info string was executed or altered:\n%s", prompt)
	}

	wantNested := "```\necho nested\n```"
	if !strings.Contains(prompt, wantNested) {
		t.Fatalf("4-backtick dashy block did not produce inner backticks:\n%s", prompt)
	}
}

func TestAgentsFailureText(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	content := "# Failure Test\n\n```dashy\necho \"standard error line 1\" >&2\necho \"standard error line 2\" >&2\nexit 42\n```\n"

	localFile := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(localFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	prompt := a.instructions()
	if !strings.Contains(prompt, "[dashy block failed: exit status 42]") {
		t.Fatalf("prompt missing failure header:\n%s", prompt)
	}
	if !strings.Contains(prompt, "standard error line 2") {
		t.Fatalf("prompt missing stderr tail:\n%s", prompt)
	}
}

func TestAgentsTimeout(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	content := "# Timeout Test\n\n```dashy\nsleep 5\n```\n"

	localFile := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(localFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	origTimeout := dashyBlockTimeout
	dashyBlockTimeout = 50 * time.Millisecond
	defer func() { dashyBlockTimeout = origTimeout }()

	a, err := New(cwd, ConfigFiles{NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	prompt := a.instructions()
	if !strings.Contains(prompt, "[dashy block failed: timed out after 50ms]") {
		t.Fatalf("prompt missing timeout failure message:\n%s", prompt)
	}
}

func TestAgentsTruncation(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	content := "# Truncation Test\n\n```dashy\nprintf \"START_MARKER\"\nhead -c 100000 /dev/zero | tr '\\0' 'A'\nprintf \"END_MARKER\"\n```\n"

	localFile := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(localFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	prompt := a.instructions()
	if !strings.Contains(prompt, "START_MARKER") {
		t.Fatalf("truncation did not keep the start")
	}
	if strings.Contains(prompt, "END_MARKER") {
		t.Fatalf("truncation did not truncate at 64 KiB; found END_MARKER")
	}

	header := fmt.Sprintf("# %s\n# Truncation Test\n\n", localFile)
	subContent := strings.TrimPrefix(prompt, header)
	subContent = strings.TrimSuffix(subContent, "\n")
	if len(subContent) != DashyBlockMaxOutput {
		t.Fatalf("expected truncated output length %d, got %d", DashyBlockMaxOutput, len(subContent))
	}
}

func TestAgentsLazyFreezeAndSource(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	agentsPath := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("Version 1:\n```dashy\necho $DASHY_MODE:$DASHY_SOURCE\n```\n"), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.System != "" {
		t.Fatalf("expected lazy assembly, but a.System was already populated: %q", a.System)
	}

	p1 := a.instructions()
	if !strings.Contains(p1, "Version 1:\ntui:startup") {
		t.Fatalf("expected tui:startup in prompt, got:\n%s", p1)
	}

	if err := os.WriteFile(agentsPath, []byte("Version 2 modified\n"), 0600); err != nil {
		t.Fatal(err)
	}

	p2 := a.instructions()
	if p2 != p1 {
		t.Fatalf("prompt was not frozen; got:\n%s\nwant:\n%s", p2, p1)
	}

	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if a.promptFrozen {
		t.Fatal("new session must not be promptFrozen before first request")
	}

	if err := os.WriteFile(agentsPath, []byte("Version 3:\n```dashy\necho $DASHY_MODE:$DASHY_SOURCE\n```\n"), 0600); err != nil {
		t.Fatal(err)
	}

	pNew := a.instructions()
	if !strings.Contains(pNew, "Version 3:\ntui:new") {
		t.Fatalf("expected tui:new in new session prompt, got:\n%s", pNew)
	}
}

func TestAgentsConfigFlag(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	defaultFile := filepath.Join(cwd, "AGENTS.md")
	if err := os.WriteFile(defaultFile, []byte("Default local.\n"), 0600); err != nil {
		t.Fatal(err)
	}

	customFile := filepath.Join(cwd, "custom_role.md")
	if err := os.WriteFile(customFile, []byte("Custom role instructions.\n"), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{Config: "custom_role.md", NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	prompt := a.instructions()
	if strings.Contains(prompt, "Default local.") {
		t.Fatalf("prompt should not contain default local instructions when --config is given:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Custom role instructions.") {
		t.Fatalf("prompt missing custom role instructions:\n%s", prompt)
	}
	if !strings.Contains(prompt, "# "+customFile) {
		t.Fatalf("prompt missing custom file header:\n%s", prompt)
	}

	_, err = New(cwd, ConfigFiles{Config: "nonexistent.md"})
	if err == nil {
		t.Fatal("expected startup error for missing --config file, got nil")
	}
}

func TestAgentsEmptyWhenNothingFound(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	a, err := New(cwd, ConfigFiles{NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	prompt := a.instructions()
	if prompt != "" {
		t.Fatalf("expected empty prompt when no AGENTS.md exists, got: %q", prompt)
	}
}

func TestAgentsEnvironmentVars(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", home)

	agentsPath := filepath.Join(cwd, "AGENTS.md")
	script := "# Env Test\n\n```dashy\necho MODE=$DASHY_MODE\necho SOURCE=$DASHY_SOURCE\necho SESSION=$DASHY_SESSION_ID\necho HOME=$DASHY_HOME\n```\n"
	if err := os.WriteFile(agentsPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Print mode + startup
	a, err := New(cwd, ConfigFiles{NoGlobal: true, Mode: "print"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	p := a.instructions()
	if !strings.Contains(p, "MODE=print") {
		t.Fatalf("expected MODE=print, got:\n%s", p)
	}
	if !strings.Contains(p, "SOURCE=startup") {
		t.Fatalf("expected SOURCE=startup, got:\n%s", p)
	}
	if !strings.Contains(p, "SESSION="+a.sessionID()) {
		t.Fatalf("expected SESSION=%s, got:\n%s", a.sessionID(), p)
	}
	if !strings.Contains(p, "HOME="+home) {
		t.Fatalf("expected HOME=%s, got:\n%s", home, p)
	}

	// Save session so we can resume it
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	oldID := a.sessionID()

	// Switch to a new session first
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}

	// 2. Resume mode via Resume(id)
	if err := a.Resume(oldID); err != nil {
		t.Fatal(err)
	}
	pResume := a.instructions()
	if !strings.Contains(pResume, "SOURCE=resume") {
		t.Fatalf("expected SOURCE=resume on Resume(id), got:\n%s", pResume)
	}

	// Close first agent so lock is released
	a.Close()

	// 3. Resume mode via New(cwd, files, resumeID)
	aResume, err := New(cwd, ConfigFiles{NoGlobal: true, Mode: "print"}, oldID)
	if err != nil {
		t.Fatal(err)
	}
	defer aResume.Close()
	pResumeStartup := aResume.instructions()
	if !strings.Contains(pResumeStartup, "SOURCE=resume") {
		t.Fatalf("expected SOURCE=resume on New with target id, got:\n%s", pResumeStartup)
	}
}
