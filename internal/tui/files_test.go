package tui

import (
	"context"
	"github.com/charmbracelet/bubbles/cursor"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFindFileTokenBasics(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		cursor int
		wantOk bool
		wantQ  string
		wantS  int
		wantE  int
	}{
		{"empty", "", 0, false, "", 0, 0},
		{"just at", "@", 1, true, "", 0, 1},
		{"at with word", "@foo", 4, true, "foo", 0, 4},
		{"at with dir", "@dir/", 5, true, "dir/", 0, 5},
		{"cursor inside token", "check @dir/file more", 12, true, "dir/f", 6, 15},
		{"token with parens", "(@file)", 6, true, "file", 1, 6},
		{"token with quotes", "@\"my file.txt\"", 13, true, "my file.txt", 0, 14},
		{"multiline token", "line 1\nline 2 @src/main.go", 26, true, "src/main.go", 14, 26},
		{"space after token", "@foo ", 5, false, "", 0, 0},
		{"negative cursor", "@foo", -1, false, "", 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			token, ok := findFileToken(tc.text, tc.cursor)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOk)
			}
			if ok {
				if token.query != tc.wantQ {
					t.Errorf("query = %q, want %q", token.query, tc.wantQ)
				}
				if token.start != tc.wantS || token.end != tc.wantE {
					t.Errorf("range = [%d, %d], want [%d, %d]", token.start, token.end, tc.wantS, tc.wantE)
				}
			}
		})
	}
}

func TestFilterRanking(t *testing.T) {
	indexed := []string{
		"cmd/dashy/main.go",
		"internal/agent/agent.go",
		"internal/tui/files.go",
		"main.go",
		"dashy.json",
		"scripts/build_release.sh",
	}

	// Query "main":
	// Basename prefix matches: "cmd/dashy/main.go" (base "main.go"), "main.go" (base "main.go")
	// Both are Category 0. Deterministic tie-break by path: cmd/dashy/main.go < main.go.
	res := filterFileCompletions(indexed, "main")
	if len(res) < 2 || res[0] != "cmd/dashy/main.go" || res[1] != "main.go" {
		t.Fatalf("query 'main' results = %v", res)
	}

	// Query "das":
	// Basename prefix matches: "cmd/dashy/main.go" (base "main.go", no match on base)
	// "dashy.json": base is "dashy.json", prefix match -> Cat 0
	// "cmd/dashy/main.go": path contains "das" -> Cat 1
	res = filterFileCompletions(indexed, "das")
	if len(res) < 2 {
		t.Fatalf("query 'das' results = %v", res)
	}
	if res[0] != "dashy.json" {
		t.Errorf("res[0] = %q, want %q", res[0], "dashy.json")
	}
	if res[1] != "cmd/dashy/main.go" {
		t.Errorf("res[1] = %q, want %q", res[1], "cmd/dashy/main.go")
	}

	// Empty query returns up to 100 entries sorted
	empty := filterFileCompletions(indexed, "")
	if len(empty) != len(indexed) {
		t.Fatalf("empty query len = %d, want %d", len(empty), len(indexed))
	}
}

func TestFileCompletionAcceptFileReplacesTokenOnly(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.input.SetValue("Please read @src/ and review")
	_ = m.setCursorOffset(17)
	token, ok := findFileToken(m.input.Value(), 17)
	if !ok {
		t.Fatal("token not found")
	}
	m.fileCompletions = []string{"src/main.go"}
	m.fileTokenStart = token.start
	m.fileTokenEnd = token.end

	m.acceptFileCompletion()

	want := "Please read @src/main.go and review"
	if got := m.input.Value(); got != want {
		t.Fatalf("composer after file accept = %q, want %q", got, want)
	}
	if m.hasFileCompletions() {
		t.Fatal("file completions should be closed after file acceptance")
	}
}

func TestFileCompletionEnterDoesNotSubmit(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.fileCompletions = []string{"main.go"}
	m.fileTokenStart = 0
	m.fileTokenEnd = 2
	m.input.SetValue("@m")
	m.input.CursorEnd()

	// First Enter: accepts file completion
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	if got := m.input.Value(); got != "@main.go" {
		t.Fatalf("first enter value = %q, want %q", got, "@main.go")
	}
	if m.ctl.Status().Busy {
		t.Fatal("first Enter submitted turn while accepting file")
	}
	if m.hasFileCompletions() {
		t.Fatal("completions still active after file acceptance")
	}
}

func TestEscClosesActiveCompletionBeforeTurnCancellation(t *testing.T) {
	m, _, _ := heldModel(t)
	m = startBusy(t, m)
	m.fileCompletions = []string{"file1.go", "file2.go"}

	// First Esc closes completions
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)

	if m.hasFileCompletions() {
		t.Fatal("first Esc did not close completions")
	}
	if m.ctl.Status().Cancelling {
		t.Fatal("first Esc cancelled turn instead of closing completions")
	}

	// Second Esc cancels the busy turn; the turn ends with a cancellation notice.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = awaitIdle(t, next.(model))
	if !hasEvent(m, "Turn cancelled.") {
		t.Fatal("second Esc did not cancel the turn")
	}
}

func TestGitTrackedAndUntracked(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runCmd := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v failed: %v", args, err)
		}
	}
	runCmd("init")
	runCmd("config", "user.name", "test")
	runCmd("config", "user.email", "test@test.com")

	_ = os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("untracked"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("ignored"), 0644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0644)

	runCmd("add", "tracked.txt", ".gitignore")
	runCmd("commit", "-m", "init")

	ctx, cancel := context.WithTimeout(context.Background(), fileScanTimeout)
	defer cancel()

	paths, err := scanProjectFiles(ctx, dir)
	if err != nil {
		t.Fatalf("scanProjectFiles failed: %v", err)
	}

	foundTracked, foundUntracked, foundIgnored := false, false, false
	for _, p := range paths {
		if p == "tracked.txt" {
			foundTracked = true
		}
		if p == "untracked.txt" {
			foundUntracked = true
		}
		if p == "ignored.txt" {
			foundIgnored = true
		}
	}

	if !foundTracked {
		t.Error("tracked.txt not found in git scan")
	}
	if !foundUntracked {
		t.Error("untracked.txt not found in git scan")
	}
	if foundIgnored {
		t.Error("ignored.txt leaked into git scan")
	}
}

func TestFileCompletionE2EUpdateTypingBlinkEnterEsc(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "util.go"), []byte("package main"), 0644)

	m, _, release := heldModel(t)
	m.cwd = dir

	// Genuine Update typing of "@ma"
	for _, r := range "@ma" {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}

	if m.input.Value() != "@ma" {
		t.Fatalf("input value = %q, want @ma", m.input.Value())
	}
	if !m.fileLoading {
		t.Fatal("expected fileLoading to be true after typing @ma")
	}

	// Debounce and scan
	next, scanCmd := m.Update(fileDebounceMsg{debounceGen: m.fileDebounceGen, fileGen: m.fileGen})
	m = next.(model)
	if scanCmd == nil {
		t.Fatal("expected scanCmd from debounce")
	}
	scanMsg := scanCmd()
	next, _ = m.Update(scanMsg)
	m = next.(model)

	if !m.hasFileCompletions() {
		t.Fatal("expected hasFileCompletions() to be true after scan")
	}
	if len(m.fileCompletions) != 1 || m.fileCompletions[0] != "main.go" {
		t.Fatalf("fileCompletions = %v, want ['main.go']", m.fileCompletions)
	}

	// Press Enter: accepts file completion
	next, acceptCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)
	if acceptCmd != nil {
		if noopMsg := acceptCmd(); noopMsg != nil {
			next, _ = m.Update(noopMsg)
			m = next.(model)
		}
	}

	if m.input.Value() != "@main.go" {
		t.Fatalf("input value after Enter = %q, want @main.go", m.input.Value())
	}
	if m.hasFileCompletions() {
		t.Fatal("expected completions closed after accepting file")
	}
	if m.fileLoading {
		t.Fatal("expected fileLoading false after accepting file")
	}

	genBefore := m.fileGen

	// Non-key message: cursor.BlinkMsg
	next, blinkCmd := m.Update(cursor.BlinkMsg{})
	if blinkCmd != nil {
		t.Fatalf("cursor.BlinkMsg generated non-nil cmd: %v", blinkCmd)
	}
	m = next.(model)

	if m.hasFileCompletions() {
		t.Fatal("cursor.BlinkMsg reopened file completions after Enter")
	}
	if m.fileLoading {
		t.Fatal("cursor.BlinkMsg started file loading after Enter")
	}
	if m.fileGen != genBefore {
		t.Fatalf("cursor.BlinkMsg bumped fileGen: %d -> %d", genBefore, m.fileGen)
	}
	// Also test another non-key message (window size or noop)
	next, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = next.(model)
	if m.hasFileCompletions() || m.fileLoading {
		t.Fatal("WindowSizeMsg reopened file completion")
	}

	// Subsequent Enter submits normally: when busy, it queues steering after the file is read
	m = startBusy(t, m)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(model)

	if queued := m.ctl.Status().Queued; len(queued) != 1 || queued[0].Text != "@main.go" {
		t.Fatalf("subsequent Enter did not submit/queue steering: queued = %v", queued)
	}
	if m.input.Value() != "" {
		t.Fatalf("composer not cleared after submit: %q", m.input.Value())
	}

	// Test Esc dismisses completion and stays dismissed on non-key messages
	release()
	m = awaitIdle(t, m)
	for _, r := range "@ut" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(model)
	}
	next, scanCmd = m.Update(fileDebounceMsg{debounceGen: m.fileDebounceGen, fileGen: m.fileGen})
	m = next.(model)
	if scanCmd != nil {
		scanMsg = scanCmd()
		next, _ = m.Update(scanMsg)
		m = next.(model)
	}
	if !m.hasFileCompletions() {
		t.Fatal("expected completions for @ut")
	}

	// Esc dismisses
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(model)
	if m.hasFileCompletions() || m.fileLoading {
		t.Fatal("Esc did not close completion")
	}

	// Idle blink does NOT reopen
	next, _ = m.Update(cursor.BlinkMsg{})
	m = next.(model)
	if m.hasFileCompletions() || m.fileLoading {
		t.Fatal("cursor.BlinkMsg revived completion dismissed by Esc")
	}

	// Moving cursor clears suppression: Left arrow moves cursor from 3 to 2
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m = next.(model)
	if m.fileDismissed {
		t.Fatal("cursor movement did not clear fileDismissed")
	}

	// Typing clears suppression
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	m = next.(model)
	if m.fileDismissed {
		t.Fatal("typing did not clear fileDismissed")
	}
}

func TestUpDownNavigatesFileCompletions(t *testing.T) {
	m := bareModel(t, t.TempDir())
	m.fileCompletions = []string{"a.go", "b.go", "c.go"}
	m.fileSelected = 0

	// Down moves to 1
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if m.fileSelected != 1 {
		t.Fatalf("selected = %d, want 1", m.fileSelected)
	}

	// Down moves to 2
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if m.fileSelected != 2 {
		t.Fatalf("selected = %d, want 2", m.fileSelected)
	}

	// Down wraps to 0
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(model)
	if m.fileSelected != 0 {
		t.Fatalf("selected = %d, want 0", m.fileSelected)
	}

	// Up wraps to 2
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = next.(model)
	if m.fileSelected != 2 {
		t.Fatalf("selected = %d, want 2", m.fileSelected)
	}
}

func TestScanWalkBounded(t *testing.T) {
	dir := t.TempDir()

	// Create sample structure
	_ = os.MkdirAll(filepath.Join(dir, "src", "pkg"), 0755)
	_ = os.WriteFile(filepath.Join(dir, "src", "pkg", "app.go"), []byte("package pkg"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root"), 0644)

	// Create ignored directories
	_ = os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0755)
	_ = os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("git config"), 0644)
	_ = os.MkdirAll(filepath.Join(dir, ".dashy"), 0755)
	_ = os.WriteFile(filepath.Join(dir, ".dashy", "state"), []byte("state"), 0644)
	_ = os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0755)
	_ = os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("js"), 0644)

	// Create symlink to directory (must not traverse)
	_ = os.Symlink(filepath.Join(dir, "src"), filepath.Join(dir, "link_to_src"))

	ctx, cancel := context.WithTimeout(context.Background(), fileScanTimeout)
	defer cancel()

	paths, err := scanProjectFiles(ctx, dir)
	if err != nil {
		t.Fatalf("scanProjectFiles error: %v", err)
	}

	for _, p := range paths {
		if strings.HasPrefix(p, ".git") || strings.HasPrefix(p, ".dashy") || strings.HasPrefix(p, "node_modules") {
			t.Fatalf("ignored directory leaked into paths: %s", p)
		}
		if strings.HasPrefix(p, "link_to_src/") {
			t.Fatalf("traversed directory symlink: %s", p)
		}
	}

	// Check that real files are found
	hasRoot := false
	hasApp := false
	for _, p := range paths {
		if p == "root.txt" {
			hasRoot = true
		}
		if p == "src/pkg/app.go" {
			hasApp = true
		}
	}
	if !hasRoot || !hasApp {
		t.Fatalf("expected paths root.txt and src/pkg/app.go in %v", paths)
	}
}

func TestQuotedPathWithSpacesAcceptance(t *testing.T) {
	m := bareModel(t, t.TempDir())
	input := "look at @\"my fold"
	m.input.SetValue(input)
	m.input.CursorEnd()

	token, ok := findFileToken(input, len([]rune(input)))
	if !ok {
		t.Fatal("token not found")
	}

	m.fileCompletions = []string{"my folder/document.pdf"}
	m.fileTokenStart = token.start
	m.fileTokenEnd = token.end

	m.acceptFileCompletion()

	want := "look at @\"my folder/document.pdf\""
	if got := m.input.Value(); got != want {
		t.Fatalf("quoted path replacement = %q, want %q", got, want)
	}
}
