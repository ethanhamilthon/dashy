package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// compactHistory builds n turns of user/assistant/tool pairs.
func compactHistory(t *testing.T, turns int) []json.RawMessage {
	t.Helper()
	var history []json.RawMessage
	for i := 0; i < turns; i++ {
		history = append(history,
			raw(map[string]any{"role": "user", "content": fmt.Sprintf("question %d", i)}),
			raw(map[string]any{"type": "function_call", "name": "bash", "arguments": "{}", "call_id": fmt.Sprintf("call-%d", i)}),
			raw(map[string]any{"type": "function_call_output", "call_id": fmt.Sprintf("call-%d", i), "output": "ok"}),
			raw(map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d", i)}),
		)
	}
	return history
}

func TestTurnStartCountsOnlyUserMessages(t *testing.T) {
	history := compactHistory(t, 6)
	if got := turnStart(history, 2); got != 16 {
		t.Fatalf("turnStart = %d, want 16", got)
	}
	if got := turnStart(history, 100); got != 0 {
		t.Fatalf("turnStart with too many turns = %d, want 0", got)
	}
}

func TestCompactKeepsTailAndSummary(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		writeSummary(w, "condensed")
	})
	defer closeAll()
	a.History = compactHistory(t, 6)
	tail := append([]json.RawMessage(nil), a.History[16:]...)
	a.compactKeepTurns = 2
	result, err := a.Compact(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	records := a.Records()
	if len(records) == 0 || records[0].Kind != "notice" || !strings.Contains(records[0].Text, "condensed") {
		t.Fatalf("compaction summary not derived in records: %+v", records)
	}
	if !strings.Contains(result, "kept the last 2 turns") {
		t.Fatalf("result = %q", result)
	}
	if len(a.History) != len(tail)+1 {
		t.Fatalf("history length = %d, want %d", len(a.History), len(tail)+1)
	}
	if !strings.Contains(string(a.History[0]), "compact_summary") {
		t.Fatalf("first item = %s", a.History[0])
	}
	for i := range tail {
		if string(a.History[i+1]) != string(tail[i]) {
			t.Fatalf("tail item %d changed", i)
		}
	}
	calls, outputs := 0, 0
	for _, item := range a.History {
		if strings.Contains(string(item), "function_call_output") {
			outputs++
		} else if strings.Contains(string(item), "function_call") {
			calls++
		}
	}
	if calls != outputs {
		t.Fatalf("orphaned calls: %d calls, %d outputs", calls, outputs)
	}
}

func TestCompactNothingToDo(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("summary must not be requested")
	})
	defer closeAll()
	a.History = compactHistory(t, 2)
	a.compactKeepTurns = 4
	result, err := a.Compact(context.Background(), "")
	if err != nil || result != "Nothing to compact." {
		t.Fatalf("result = %q, err = %v", result, err)
	}
	if len(a.History) != 8 {
		t.Fatalf("history changed: %d items", len(a.History))
	}
}

func TestCompactUsesSessionInstructionsAndPrompt(t *testing.T) {
	var body string
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		json.NewDecoder(r.Body).Decode(&request)
		body = string(raw(request))
		writeSummary(w, "summarized")
	})
	defer closeAll()
	a.System = "custom system"
	a.History = compactHistory(t, 5)
	a.compactKeepTurns = 1
	if _, err := a.Compact(context.Background(), "focus on the parser"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"custom system", "focus on the parser", "Summarize this conversation"} {
		if !strings.Contains(body, want) {
			t.Fatalf("summary request missing %q", want)
		}
	}
	if strings.Contains(body, "bash") {
		t.Fatal("summary request must not offer tools")
	}
}

func TestCompactFailureKeepsHistoryWithOrWithoutPrompt(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom","type":"test"}}`)
	})
	defer closeAll()
	a.History = compactHistory(t, 5)
	a.compactKeepTurns = 1
	before := append([]json.RawMessage(nil), a.History...)
	for _, prompt := range []string{"", "focus"} {
		if _, err := a.Compact(context.Background(), prompt); err == nil {
			t.Fatal("expected an error")
		}
		if !reflect.DeepEqual(a.History, before) {
			t.Fatal("failed compaction changed the history")
		}
	}
}

func TestAutomaticCompactionFailurePreservesHistory(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/responses") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"boom","type":"test"}}`)
		}
	})
	defer closeAll()
	a.History = compactHistory(t, 5)
	a.compactKeepTurns = 1
	before, err := a.sessionData()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	var events []Event
	a.compactIfNeeded(context.Background(), func(e Event) { events = append(events, e) })
	after, err := a.sessionData()
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed compaction changed the session: %v", err)
	}
	saved, err := os.ReadFile(a.HistoryPath)
	if err != nil || string(saved) != string(before) {
		t.Fatalf("failed compaction changed the saved session: %v", err)
	}
	if len(events) != 1 || events[0].Kind != "error" || !strings.Contains(events[0].Text, "Compaction failed:") {
		t.Fatalf("compaction failure not reported: %+v", events)
	}
}

func TestCompactDisabledByZeroThreshold(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("disabled compaction must not request a summary")
	})
	defer closeAll()
	a.History = compactHistory(t, 5)
	a.settings.AutoCompact = false
	a.compactIfNeeded(context.Background(), func(Event) {})
	if len(a.History) != 20 {
		t.Fatalf("history changed: %d items", len(a.History))
	}
}

func summaryAgent(t *testing.T, summary http.HandlerFunc) (*Agent, func()) {
	t.Helper()
	t.Setenv("DASHY_HOME", t.TempDir())
	server := httptest.NewServer(summary)
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", AutoCompact: true, Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	a.compactThreshold = 100
	a.compactKeepTurns = 1
	return a, func() { a.Close(); server.Close() }
}

func writeSummary(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[]}]}]}`, text)
}

func TestEstimateTokensChargesImagesOnce(t *testing.T) {
	payload := strings.Repeat("A", 400*1024)
	history := []json.RawMessage{
		raw(map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "look"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + payload, "detail": "auto"},
		}}),
	}
	if got := estimateTokens(history); got > 5000 {
		t.Fatalf("a 400 KiB image counted as %d tokens", got)
	}
	if got := estimateTokens(history); got < imageTokens/tokenBytes {
		t.Fatalf("image not counted at all: %d", got)
	}
}

func TestCompactSkipsExistingSummary(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an existing summary must not be summarized again")
	})
	defer closeAll()
	a.History = append([]json.RawMessage{raw(map[string]any{"role": "user", "content": "<compact_summary>old</compact_summary>"})}, compactHistory(t, 4)...)
	a.compactKeepTurns = 4
	result, err := a.Compact(context.Background(), "")
	if err != nil || result != "Nothing to compact." {
		t.Fatalf("result = %q, err = %v", result, err)
	}
}
