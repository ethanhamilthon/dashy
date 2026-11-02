package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Empty tool output is a real record: after compaction it must not borrow the text
// of a different derived item when the session is reopened.
func TestCompactedJournalSurvivesReopenExactly(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) { writeSummary(w, "condensed") })
	defer closeAll()
	a.History = []json.RawMessage{
		raw(map[string]any{"role": "user", "content": "q0"}),
		raw(map[string]any{"role": "assistant", "content": "let me check"}),
		raw(map[string]any{"type": "function_call", "name": "bash", "arguments": `{"command":"ls"}`, "call_id": "c0"}),
		raw(map[string]any{"type": "function_call_output", "call_id": "c0", "output": string(raw(map[string]any{"output": "", "error": false}))}),
		raw(map[string]any{"role": "assistant", "content": "done0"}),
		raw(map[string]any{"role": "user", "content": "q1"}),
		raw(map[string]any{"type": "function_call", "name": "bash", "arguments": `{"command":"cat x"}`, "call_id": "c1"}),
		raw(map[string]any{"type": "function_call_output", "call_id": "c1", "output": string(raw(map[string]any{"output": "data", "error": false}))}),
		raw(map[string]any{"role": "assistant", "content": "done1"}),
	}
	a.settings.CompactKeepTurns = 1
	a.records = a.Records()
	a.records[1].Meta = &Metadata{Duration: 1500 * time.Millisecond, Usage: &Usage{Input: 10, Output: 2, Cached: -1}, RequestID: "r-1"}
	a.records[3].ID = "empty-output"
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	want := a.Records()
	if want[3].Kind != "output" || want[3].Text != "" {
		t.Fatalf("empty output lost before save: %+v", want[3])
	}
	id := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(id); err != nil {
		t.Fatal(err)
	}
	if got := a.Records(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopen changed the journal:\nwant %+v\ngot  %+v", want, got)
	}
	if len(a.History) == 0 || !strings.Contains(string(a.History[0]), "compact_summary") {
		t.Fatalf("compacted model context not restored: %s", a.History)
	}
}

// Sessions written before the literal flag existed keep restoring elided text.
func TestLegacyElidedRecordsStillRestore(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {})
	defer closeAll()
	a.History = compactHistory(t, 2)
	a.records = a.Records()
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	want := a.Records()
	legacyPath := a.HistoryPath
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	// Remove the flag an older writer never produced; elided text must still restore.
	legacy := strings.ReplaceAll(string(data), `,"literal":true`, "")
	if !strings.Contains(string(data), `"kind":"user"}`) || strings.Contains(legacy, `"literal"`) {
		t.Fatalf("unexpected session layout: %s", data)
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(strings.TrimSuffix(filepath.Base(legacyPath), ".jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := a.Records(); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy session restored differently:\nwant %+v\ngot  %+v", want, got)
	}
}
