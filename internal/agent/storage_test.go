package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGlobalHomeDefaultsAndMissingFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DASHY_HOME", "")
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.home != filepath.Join(home, ".dashy") {
		t.Fatal(a.home)
	}
	if a.System != "You are Dashy, a coding agent" {
		t.Fatalf("unexpected default system: %q", a.System)
	}
	cfgPath := filepath.Join(a.home, "dashy.toml")
	if err := os.WriteFile(cfgPath, []byte("system = 'Keep this prompt'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := New(a.Shell.Dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.System != "Keep this prompt" {
		t.Fatalf("expected custom prompt, got %q", b.System)
	}
	info, err := os.Stat(cfgPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private config permission: %v", err)
	}
}

func TestScopedJSONLSessionsResumeAndCheckpoint(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.message("user", "first session")
	a.records = []Record{{Kind: "user", Text: "first session"}, {Kind: "code", ID: "partial", Text: "x = 42", Meta: &Metadata{Duration: time.Second, Usage: &Usage{Input: 100, Output: 10, Cached: 50}}}}
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
		t.Fatal("metadata lost on resume")
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
	// Corrupt an inactive session, then verify a failed switch cannot mutate the live session.
	if err = os.WriteFile(second, []byte("{\"version\":2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Resume(strings.TrimSuffix(filepath.Base(second), ".jsonl")); err == nil {
		t.Fatal("incomplete session accepted")
	}
	if a.HistoryPath != first || !reflect.DeepEqual(a.Records(), want) {
		t.Fatal("failed switch changed active session")
	}
	if err = os.WriteFile(second, data[:len(data)-1], 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Resume(strings.TrimSuffix(filepath.Base(second), ".jsonl")); err == nil {
		t.Fatal("unterminated session accepted")
	}
}

func TestSessionSwitchPreservesStatusBridgeAndVersion(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Start("0.6.0")
	ch := a.StatusEvents()
	if ch == nil {
		t.Fatal("StatusEvents returned nil channel")
	}
	// Drain startup events
startupDrain:
	for {
		select {
		case <-ch:
		default:
			break startupDrain
		}
	}

	a.message("user", "test session")
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	firstID := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err = a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if a.version != "0.6.0" {
		t.Fatalf("expected version 0.6.0 preserved, got %q", a.version)
	}
	foundStart := false
drainNew:
	for {
		select {
		case ev := <-ch:
			if ev["DASHY_EVENT"] == "session_start" {
				foundStart = true
				if ev["DASHY_VERSION"] != "0.6.0" {
					t.Fatalf("expected DASHY_VERSION 0.6.0, got %q", ev["DASHY_VERSION"])
				}
				if ev["DASHY_SESSION_SOURCE"] != "new" {
					t.Fatalf("expected session source new, got %q", ev["DASHY_SESSION_SOURCE"])
				}
			}
		default:
			break drainNew
		}
	}
	if !foundStart {
		t.Fatal("session_start event not found after NewSession")
	}

	if err = a.Resume(firstID); err != nil {
		t.Fatal(err)
	}
	if a.version != "0.6.0" {
		t.Fatalf("expected version 0.6.0 preserved after Resume, got %q", a.version)
	}
	foundResume := false
drainResume:
	for {
		select {
		case ev := <-ch:
			if ev["DASHY_EVENT"] == "session_start" && ev["DASHY_SESSION_SOURCE"] == "resume" {
				foundResume = true
				if ev["DASHY_VERSION"] != "0.6.0" {
					t.Fatalf("expected DASHY_VERSION 0.6.0 on resume, got %q", ev["DASHY_VERSION"])
				}
			}
		default:
			break drainResume
		}
	}
	if !foundResume {
		t.Fatal("session_start with source=resume not found after Resume")
	}
}
