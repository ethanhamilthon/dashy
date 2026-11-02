package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSingleKindSessionLines(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if err := a.Rename("test session"); err != nil {
		t.Fatal(err)
	}
	a.History = []json.RawMessage{
		raw(map[string]any{"role": "user", "content": "hello"}),
		raw(map[string]any{"type": "function_call", "name": "bash", "arguments": `{"command":"pwd"}`, "call_id": "c1"}),
		raw(map[string]any{"type": "function_call_output", "call_id": "c1", "output": string(raw(map[string]any{"output": "/tmp", "error": false}))}),
		raw(map[string]any{"role": "assistant", "content": "you are in /tmp"}),
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(a.HistoryPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines (1 meta + 4 history), got %d: %s", len(lines), data)
	}

	// Line 0 is meta
	var meta struct {
		Meta struct {
			Title string `json:"title"`
		} `json:"meta"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &meta); err != nil || meta.Meta.Title != "test session" {
		t.Fatalf("expected meta line, got: %s", lines[0])
	}

	// Lines 1..4 are LLM history items directly
	for i, line := range lines[1:] {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i+1, err)
		}
		for _, forbidden := range []string{"version", "complete", "items", "records", "record", "literal", "selection"} {
			if _, exists := obj[forbidden]; exists {
				t.Fatalf("line %d contains forbidden envelope field %q: %s", i+1, forbidden, line)
			}
		}
	}
}

func TestTornLastLineTolerance(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.History = []json.RawMessage{
		raw(map[string]any{"role": "user", "content": "msg 1"}),
		raw(map[string]any{"role": "assistant", "content": "resp 1"}),
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	want := a.Records()

	// Append a torn line (partial JSON) to the file
	file, err := os.OpenFile(a.HistoryPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{\"role\":\"user\",\"content\":\"inc"); err != nil {
		t.Fatal(err)
	}
	file.Close()

	// Reopen the session: torn last line must be tolerated and dropped
	id := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	a.Close()
	reopened, err := New(cwd, ConfigFiles{}, id)
	if err != nil {
		t.Fatalf("failed to reopen session with torn last line: %v", err)
	}
	defer reopened.Close()

	if !reflect.DeepEqual(reopened.Records(), want) {
		t.Fatalf("reopened records = %+v, want %+v", reopened.Records(), want)
	}
}

func TestDerivedTUIRecords(t *testing.T) {
	items := []json.RawMessage{
		raw(map[string]any{"role": "user", "content": "user question"}),
		raw(map[string]any{"role": "user", "content": "<notify>\nexternal steering\n</notify>"}),
		raw(map[string]any{"role": "user", "content": "<compact_summary>\nshort summary of past turns\n</compact_summary>"}),
		raw(map[string]any{
			"type":    "reasoning",
			"summary": []any{map[string]string{"type": "summary_text", "text": "my reasoning summary"}},
			"content": []any{map[string]string{"type": "reasoning_text", "text": "thinking deeper"}},
		}),
		raw(map[string]any{"type": "function_call", "name": "bash", "arguments": `{"command":"echo hi"}`}),
		raw(map[string]any{"type": "function_call_output", "output": `{"output":"hi\n","error":false}`}),
		raw(map[string]any{"type": "function_call_output", "output": `{"output":"command failed\n","error":true}`}),
		raw(map[string]any{"role": "assistant", "content": "done"}),
	}

	a := &Agent{History: items}
	records := a.Records()

	expected := []struct {
		kind string
		text string
	}{
		{"user", "user question"},
		{"notice", "<notify>\nexternal steering\n</notify>"},
		{"notice", "Compacted conversation: short summary of past turns"},
		{"reasoning", "my reasoning summary\nthinking deeper"},
		{"code", "echo hi"},
		{"output", "hi\n"},
		{"error", "command failed\n"},
		{"assistant", "done"},
	}

	if len(records) != len(expected) {
		t.Fatalf("expected %d records, got %d: %+v", len(expected), len(records), records)
	}
	for i, exp := range expected {
		if records[i].Kind != exp.kind || records[i].Text != exp.text {
			t.Errorf("record %d: got {kind: %q, text: %q}, want {kind: %q, text: %q}",
				i, records[i].Kind, records[i].Text, exp.kind, exp.text)
		}
	}
}

func TestErrorsAndCancellationsNotPersisted(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.History = []json.RawMessage{
		raw(map[string]any{"role": "user", "content": "hi"}),
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	// Read file and ensure no error or notice is written
	data, err := os.ReadFile(a.HistoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"error"`) || strings.Contains(string(data), `"cancelled"`) {
		t.Fatalf("unexpected content in session file: %s", data)
	}
}

func TestResumeListSessions(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	a.History = []json.RawMessage{
		raw(map[string]any{"role": "user", "content": "first message text"}),
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}

	sessions, err := a.Sessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %v (err: %v)", sessions, err)
	}
	if sessions[0].Title != "first message text" {
		t.Fatalf("expected title 'first message text', got %q", sessions[0].Title)
	}
	if sessions[0].ID == "" || sessions[0].Mod.IsZero() {
		t.Fatalf("session ID or Mod time missing: %+v", sessions[0])
	}

	// Rename session
	if err := a.Rename("custom title"); err != nil {
		t.Fatal(err)
	}
	sessions, err = a.Sessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %v", sessions)
	}
	if sessions[0].Title != "custom title" {
		t.Fatalf("expected title 'custom title', got %q", sessions[0].Title)
	}
}
