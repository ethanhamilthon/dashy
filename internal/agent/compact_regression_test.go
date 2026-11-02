package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAutomaticCompactionJournalSurvivesRunAndResume(t *testing.T) {
	requests := 0
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if _, streaming := payload["stream"]; !streaming {
			writeSummary(w, "condensed")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"new answer"}]}]}}`+"\n\n")
	})
	defer closeAll()
	a.History = compactHistory(t, 5)
	a.settings.CompactKeepTurns = 2
	before := a.Records()
	if err := a.Run(context.Background(), "new question", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("got %d requests, want summary and turn", requests)
	}
	want := a.Records()
	// Compaction shortens only the model context: the display keeps every earlier record.
	if len(want) < len(before) || !reflect.DeepEqual(want[:len(before)], before) {
		t.Fatalf("compaction changed the earlier display: %+v", want)
	}
	for _, record := range want {
		if strings.Contains(record.Text, "condensed") {
			t.Fatalf("summary replaced the display journal: %+v", record)
		}
	}
	for _, text := range []string{"question 0", "answer 0", "question 4", "answer 4", "new question", "new answer"} {
		found := false
		for _, record := range want {
			found = found || strings.Contains(record.Text, text)
		}
		if !found {
			t.Errorf("compacted journal lost %q: %+v", text, want)
		}
	}
	id := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := a.Resume(id); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Records(), want) {
		t.Fatal("resume changed the compacted journal")
	}
}

func TestCompactSaveFailureRestoresHistoryAndJournal(t *testing.T) {
	a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		writeSummary(w, "condensed")
	})
	defer closeAll()
	a.History = compactHistory(t, 5)
	a.records = append(a.Records(), Record{Kind: "error", Text: "keep diagnostic"})
	history, records := a.History, a.Records()
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	path := a.HistoryPath
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A directory cannot be replaced by the atomic session-file rename.
	a.HistoryPath = t.TempDir()
	_, err = a.Compact(context.Background(), "")
	a.HistoryPath = path
	if err == nil {
		t.Fatal("expected save failure")
	}
	if !reflect.DeepEqual(a.History, history) || !reflect.DeepEqual(a.Records(), records) {
		t.Fatal("save failure changed the in-memory session")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(saved) {
		t.Fatalf("save failure changed the committed session: %v", err)
	}
}

func TestEstimateTokensCountsEachImageNotTextMentions(t *testing.T) {
	image := map[string]string{"type": "input_image", "image_url": "data:image/png;base64,AAAA"}
	estimate := func(parts ...any) int64 {
		return estimateTokens([]json.RawMessage{raw(map[string]any{"role": "user", "content": parts})})
	}
	if delta := estimate(image, image) - estimate(image); delta < imageTokens || delta > imageTokens+100 {
		t.Fatalf("second image added %d tokens, want about %d", delta, imageTokens)
	}
	if got := estimate(map[string]string{"type": "input_text", "text": "input_image"}); got >= imageTokens {
		t.Fatalf("text mention counted as an image: %d", got)
	}
}

func TestTurnStartIgnoresServiceMessages(t *testing.T) {
	history := compactHistory(t, 3)
	history = append(history, raw(map[string]any{"role": "user", "content": "<notify>prefix restarted</notify>"}),
		raw(map[string]any{"role": "user", "content": []any{
			map[string]string{"type": "input_text", "text": "<notify>Images queued by dashy view.</notify>"},
			map[string]string{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
		}}))
	if got := turnStart(history, 2); got != 4 {
		t.Fatalf("turnStart = %d, want second user question at 4", got)
	}
	for _, item := range []json.RawMessage{
		raw(map[string]any{"role": "user", "content": "<compact_summary>summary</compact_summary>"}),
		raw(map[string]any{"type": "function_call_output", "output": "ok"}),
	} {
		if userTurn(item) {
			t.Fatalf("service item counted as user turn: %s", item)
		}
	}
	imageInput := raw(map[string]any{"role": "user", "content": []any{
		map[string]string{"type": "input_text", "text": "[Image: screenshot.png]"},
		map[string]string{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
	}})
	if !userTurn(imageInput) {
		t.Fatal("user image attachment did not count as a turn")
	}
}

// Only a completed summary may replace the model context. A truncated, failed or
// cancelled summary must leave both the memory and the saved file untouched.
func TestIncompleteSummaryKeepsMemoryAndFile(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"incomplete", `{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","status":"incomplete","content":[{"type":"output_text","text":"partial","annotations":[]}]}]}`},
		{"failed", `{"id":"r","status":"failed","output":[]}`},
		{"cancelled", `{"id":"r","status":"cancelled","output":[]}`},
		{"no status", `{"id":"r","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"untagged","annotations":[]}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, closeAll := summaryAgent(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			})
			defer closeAll()
			a.History = compactHistory(t, 5)
			a.settings.CompactKeepTurns = 1
			if err := a.Save(); err != nil {
				t.Fatal(err)
			}
			history, records := append([]json.RawMessage(nil), a.History...), a.Records()
			saved, err := os.ReadFile(a.HistoryPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Compact(context.Background(), ""); err == nil {
				t.Fatal("expected an error for an incomplete summary")
			}
			if !reflect.DeepEqual(a.History, history) || !reflect.DeepEqual(a.Records(), records) {
				t.Fatal("incomplete summary changed the in-memory session")
			}
			after, err := os.ReadFile(a.HistoryPath)
			if err != nil || string(after) != string(saved) {
				t.Fatalf("incomplete summary changed the saved session: %v", err)
			}
		})
	}
}
