package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dashy/internal/shell"
)

func TestRecordsForLegacyItems(t *testing.T) {
	for _, tc := range []struct{ item, kind, text string }{
		{`{"role":"user","content":"hello"}`, "user", "hello"},
		{`{"role":"assistant","content":"string content"}`, "assistant", "string content"},
		{`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"intermediate"},{"type":"refusal","refusal":"refused"}]}`, "assistant", "intermediate\nrefused"},
		{`{"type":"reasoning","summary":[{"text":"summary"}],"content":[{"text":"thinking"}]}`, "reasoning", "summary\nthinking"},
		{`{"type":"function_call","name":"bash","arguments":"{\"command\":\"ls -l\"}"}`, "code", "ls -l"},
		{`{"type":"function_call_output","output":"{\"output\":\"oops\",\"error\":true}"}`, "error", "oops"},
		{`{"type":"function_call_output","output":"raw tool output"}`, "output", "raw tool output"},
		{`{"type":"web_search_call","id":"w"}`, "notice", `{"type":"web_search_call","id":"w"}`},
	} {
		records := RecordsForItem(json.RawMessage(tc.item))
		if len(records) != 1 || records[0].Kind != tc.kind || records[0].Text != tc.text {
			t.Errorf("%s => %+v", tc.item, records)
		}
	}
	if records := RecordsForItem(json.RawMessage(`{"type":"reasoning","encrypted_content":"secret"}`)); len(records) != 0 {
		t.Fatal("encrypted reasoning must not be rendered")
	}
}
func TestFullTurnLiveAndHistory(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	dir := t.TempDir()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var payload struct{ Input []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		for _, item := range payload.Input {
			if strings.Contains(string(item), `"duration"`) {
				t.Error("UI metadata leaked into API input")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\n")
			output := []any{
				map[string]any{"type": "reasoning", "summary": []any{map[string]string{"type": "summary_text", "text": "complete summary"}}},
				map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "Before tools"}}},
				map[string]any{"type": "function_call", "name": "bash", "call_id": "a", "arguments": string(raw(map[string]any{"command": "printf first", "timeout": nil}))},
				map[string]any{"type": "function_call", "name": "bash", "call_id": "b", "arguments": string(raw(map[string]any{"command": "pwd; echo problem >&2; exit 3", "timeout": nil}))},
			}
			fmt.Fprintf(w, "data: %s\n\n", raw(map[string]any{"type": "response.completed", "response": map[string]any{"output": output, "usage": map[string]any{"input_tokens": 1000, "output_tokens": 100, "input_tokens_details": map[string]int{"cached_tokens": 200}}}}))
		} else {
			if len(payload.Input) != 7 {
				t.Errorf("missing history/tool outputs: %d", len(payload.Input))
			}
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"# Final\\nAll done\"}]}],\"usage\":{\"input_tokens\":1100,\"output_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":400}}}}\n\n")
		}
	}))
	defer server.Close()
	a, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "test", Model: "m", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
	var live []Record
	if err = a.Run(context.Background(), "go", func(e Event) { live = ApplyEvent(live, e) }); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests %d", requests)
	}
	if !reflect.DeepEqual(live, a.Records()) {
		t.Fatalf("live != journal\n%+v\n%+v", live, a.Records())
	}
	kinds := []string{}
	for _, r := range live {
		kinds = append(kinds, r.Kind)
	}
	want := []string{"user", "reasoning", "assistant", "code", "code", "output", "error", "assistant"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("kinds: %v", kinds)
	}
	if live[1].Text != "complete summary" || live[2].Text != "Before tools" || !strings.Contains(live[6].Text, dir) || !strings.Contains(live[6].Text, "problem") {
		t.Fatalf("incomplete content: %+v", live)
	}
	for _, i := range []int{3, 4} {
		if live[i].Meta.Usage.Input != 1000 {
			t.Errorf("tool usage must be per-request: %+v", live[i])
		}
	}
	final := live[len(live)-1]
	if *final.Meta.Usage != (Usage{Input: 2100, Output: 120, Cached: 600}) || *final.Meta.ContextInput != 1100 || final.Meta.Duration <= 0 {
		t.Fatalf("summary or context wrong: %+v", final.Meta)
	}
	a.Close() // release ownership so the session can be reopened
	restored, err := New(dir, ConfigFiles{}, a.SessionInfo()["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !reflect.DeepEqual(live, restored.Records()) {
		t.Fatal("display metadata did not survive reload")
	}
	info, err := os.Stat(a.HistoryPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("history permissions: %v %v", info, err)
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(a.HistoryPath), ".history-*"))
	if len(files) != 0 {
		t.Fatal("temporary history files left behind")
	}
}
func TestHistoryFallbackAndUnknownUsage(t *testing.T) {
	a := &Agent{History: []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"old"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"output_text","text":"answer"}]}`),
	}}
	records := a.Records()
	if len(records) != 2 || records[1].Text != "answer" || records[1].Meta != nil {
		t.Fatalf("%+v", records)
	}
}

func TestItemContentIsLiveAndSurvivesFailedStream(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"intermediate\"}]}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"intermediate complete\"}]}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"error\",\"message\":\"later failure\"}\n\n")
	}))
	defer server.Close()
	dir := t.TempDir()
	a, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "test", Model: "m", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
	var live []Record
	err = a.Run(context.Background(), "go", func(e Event) { live = ApplyEvent(live, e) })
	if err == nil || len(live) != 3 || live[1].Text != "intermediate complete" {
		t.Fatalf("live=%+v err=%v", live, err)
	}
	a.Close() // release ownership so the session can be reopened
	restored, err := New(dir, ConfigFiles{}, a.SessionInfo()["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	records := restored.Records()
	if len(records) != 3 || records[1].Text != "intermediate complete" {
		t.Fatalf("lost partial response: %+v", records)
	}
}

func TestUnavailableUsageStaysUnknown(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n")
	}))
	defer server.Close()
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	if err = a.Run(context.Background(), "go", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	records := a.Records()
	meta := records[len(records)-1].Meta
	if meta == nil || meta.Usage != nil || meta.ContextInput != nil {
		t.Fatalf("missing usage fabricated: %+v", meta)
	}
}

func TestGenericToolHistoryUsesGenericRendering(t *testing.T) {
	call := json.RawMessage(`{"type":"function_call","name":"other_tool","call_id":"old","arguments":"{\"command\":\"pwd\"}"}`)
	output := json.RawMessage(`{"type":"function_call_output","call_id":"old","output":"{\"output\":\"oops\",\"error\":true}"}`)
	a := &Agent{History: []json.RawMessage{call, output}}
	records := a.Records()
	if len(records) != 2 || records[0].Kind != "code" || records[1].Kind != "error" {
		t.Fatalf("unexpected generic tool rendering: %+v", records)
	}
}

func TestReasoningSummaryCanBeDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Reasoning map[string]any
			Tools     []struct{ Name string }
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if _, ok := payload.Reasoning["summary"]; ok {
			t.Error("summary must be omitted")
		}
		if len(payload.Tools) != 1 || payload.Tools[0].Name != "bash" {
			t.Errorf("tools: %+v", payload.Tools)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
	defer server.Close()
	a := &Agent{Shell: &shell.Runner{}}
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", ReasoningSummary: "off", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	if _, err := a.request(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
}

func TestOmittedCacheUsageStaysUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":20}}}\n\n")
	}))
	defer server.Close()
	a := &Agent{Shell: &shell.Runner{}}
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	if _, err := a.request(context.Background(), func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if a.lastRequest.Usage == nil || a.lastRequest.Usage.Cached != -1 || a.lastRequest.Usage.Input != 100 {
		t.Fatalf("missing cache fabricated: %+v", a.lastRequest.Usage)
	}
}
