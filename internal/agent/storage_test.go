package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestScopedJSONLSessionsResumeAndCheckpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	cwd := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Verify project directory is $DASHY_HOME/projects/<hash of absolute cwd>
	absCwd, _ := filepath.Abs(cwd)
	hash := sha256.Sum256([]byte(filepath.Clean(absCwd)))
	expectedDir := filepath.Join(home, "projects", hex.EncodeToString(hash[:]))
	if a.sessionsDir != expectedDir {
		t.Fatalf("sessionsDir = %q, want %q", a.sessionsDir, expectedDir)
	}

	a.message("user", "first session")
	a.History = append(a.History, raw(map[string]any{"type": "function_call", "name": "bash", "arguments": `{"command":"x = 42"}`}))
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	first := a.HistoryPath
	want := a.Records()

	if err = a.NewSession(); err != nil {
		t.Fatal(err)
	}
	second := a.HistoryPath
	if second == first {
		t.Fatal("new session overwrote first")
	}
	a.message("user", "second session")
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSuffix(filepath.Base(first), ".jsonl")
	if err = a.Resume(id); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, a.Records()) {
		t.Fatal("records lost on resume")
	}
	b, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.History) != 0 || b.HasSavedSession() {
		t.Fatal("startup must open an unsaved new session before first message")
	}
	b.Close()
	a.Close() // release ownership so the session can be reopened

	resumed, err := New(cwd, ConfigFiles{}, id)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.HistoryPath != first || !reflect.DeepEqual(want, resumed.Records()) {
		t.Fatal("explicit resume did not restore session")
	}
	other, err := New(filepath.Join(t.TempDir(), "project"), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Shell.Close()
	if other.sessionsDir == a.sessionsDir {
		t.Fatal("same-basename projects collided")
	}
	if err = other.Resume(id); err == nil {
		t.Fatal("cross-project resume allowed")
	}
	if err = a.Resume("../../escape"); err == nil {
		t.Fatal("path traversal accepted")
	}
	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatal("not JSONL")
		}
	}
	for _, path := range []string{first, second} {
		info, e := os.Stat(path)
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("permissions: %s %v", path, e)
		}
	}

	// Corrupt an intermediate line: failed switch cannot mutate live session.
	if err = os.WriteFile(second, []byte("bad\n{\"role\":\"user\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Resume(strings.TrimSuffix(filepath.Base(second), ".jsonl")); err == nil {
		t.Fatal("corrupt intermediate line accepted")
	}
	if a.HistoryPath != first || !reflect.DeepEqual(a.Records(), want) {
		t.Fatal("failed switch changed active session")
	}

	// Torn last line is tolerated (dropped on open).
	tornData := append(data, []byte("{\"role\":\"assistant\",\"content\":\"incom")...)
	if err = os.WriteFile(second, tornData, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Resume(strings.TrimSuffix(filepath.Base(second), ".jsonl")); err != nil {
		t.Fatalf("torn last line was not tolerated: %v", err)
	}
	if !reflect.DeepEqual(a.Records(), want) {
		t.Fatalf("records after torn resume = %+v, want %+v", a.Records(), want)
	}
}
