package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/hooks"
	"dashy/internal/shell"
)

func TestCompletedItemSavedBeforeResponseFinishes(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	finished := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"complete answer"}]}}`+"\n\n")
		fmt.Fprint(w, `data: {"type":"response.output_text.delta","output_index":1,"delta":"unfinished fragment"}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	dir := t.TempDir()
	a, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	done := make(chan error, 1)
	go func() {
		done <- a.Run(context.Background(), "hello", func(e Event) {
			if e.Kind == "delta" {
				close(finished)
			}
		})
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("stream stalled")
	}
	// Read the committed snapshot while the writer is still in the response.
	b := &Agent{HistoryPath: a.HistoryPath, Shell: &shell.Runner{Dir: a.Shell.Dir}, settings: a.settings}
	if err = b.readSession(); err != nil {
		close(release)
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.HistoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "complete answer") || strings.Contains(string(data), "unfinished fragment") {
		t.Fatalf("incorrect checkpoint: %s", data)
	}
	if !strings.Contains(string(b.History[len(b.History)-1]), "complete answer") {
		t.Fatal("completed item missing from API history")
	}
	b.Close()
	close(release)
	<-done
}

func TestInterruptedCallsRecoveredWithoutRerunning(t *testing.T) {
	history := []json.RawMessage{
		raw(map[string]any{"type": "function_call", "call_id": "first"}),
		raw(map[string]any{"type": "function_call", "call_id": "second"}),
		raw(map[string]any{"type": "function_call_output", "call_id": "first", "output": "ok"}),
	}
	outputs := interruptedOutputs(history)
	if len(outputs) != 1 || !strings.Contains(string(outputs[0]), "second") || !strings.Contains(string(outputs[0]), "not rerun") {
		t.Fatalf("unexpected recovery: %s", outputs)
	}
	history = append(history, outputs...)
	if len(interruptedOutputs(history)) != 0 {
		t.Fatal("recovery not idempotent")
	}
}

func TestCrashDuringSecondToolKeepsFirstResult(t *testing.T) {
	if os.Getenv("DASHY_CRASH_HELPER") == "1" {
		a, err := New(os.Getenv("CRASH_DIR"), ConfigFiles{})
		if err != nil {
			os.Exit(2)
		}
		configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: os.Getenv("CRASH_SERVER")}}})
		_ = a.Run(context.Background(), "run tools", func(Event) {})
		os.Exit(3)
	}
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	started := filepath.Join(dir, "started")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			calls := []any{}
			for i, cmd := range []string{"echo first-result", "echo started > " + started + "; sleep 3"} {
				calls = append(calls, map[string]any{"type": "function_call", "call_id": fmt.Sprint(i), "name": "bash", "arguments": string(raw(map[string]any{"command": cmd}))})
			}
			fmt.Fprintf(w, "data: %s\n\n", raw(map[string]any{"type": "response.completed", "response": map[string]any{"output": calls}}))
		} else {
			var request struct {
				Input []json.RawMessage `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			found := false
			for _, item := range request.Input {
				if strings.Contains(string(item), "not rerun") {
					found = true
				}
			}
			if !found {
				t.Errorf("missing interrupted tool output: %s", request.Input)
			}
			fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[]}}`+"\n\n")
		}
	}))
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCrashDuringSecondToolKeepsFirstResult$")
	cmd.Env = append(os.Environ(), "DASHY_CRASH_HELPER=1", "CRASH_DIR="+dir, "CRASH_SERVER="+server.URL)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tool did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	a, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	sessions, err := a.Sessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %+v %v", sessions, err)
	}
	if err = a.Resume(sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range a.History {
		if strings.Contains(string(item), "first-result") {
			found = true
		}
	}
	if !found {
		t.Fatalf("first tool result lost after crash: %s", a.History)
	}
	if err = a.Run(context.Background(), "continue", func(Event) {}); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanRepairedBeforeReadyHookOutputsInCapturedRequest(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	var capturedInput []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		capturedInput = body.Input
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"response.completed","response":{"output":[]}}`+"\n\n")
	}))
	defer server.Close()

	dir := t.TempDir()
	a, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})

	// Set up history with an orphan function_call
	a.History = []json.RawMessage{
		raw(map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "start"}}}),
		raw(map[string]any{"type": "function_call", "call_id": "orphan_call_1", "name": "bash", "arguments": `{"command":"echo 1"}`}),
	}

	// Queue a ready hook output for this session
	sessionID := strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
	a.hooks.Set([]hooks.Hook{
		{
			Name:       "hook1",
			Events:     []string{"turn_end"},
			Command:    "echo from-hook",
			SendOutput: true,
		},
	})
	a.hooks.Fire("turn_end", map[string]string{
		"DASHY_SESSION_ID": sessionID,
		"DASHY_CWD":        dir,
	})
	a.hooks.Wait(2 * time.Second)

	// Run next turn
	err = a.Run(context.Background(), "user-input-2", func(Event) {})
	if err != nil {
		t.Fatal(err)
	}

	if len(capturedInput) < 4 {
		t.Fatalf("expected at least 4 items in captured request, got %d: %+v", len(capturedInput), capturedInput)
	}

	callIdx := -1
	callOutIdx := -1
	hookIdx := -1
	user2Idx := -1

	for i, item := range capturedInput {
		typ, _ := item["type"].(string)
		callID, _ := item["call_id"].(string)
		content, _ := json.Marshal(item)
		contentStr := string(content)

		if typ == "function_call" && callID == "orphan_call_1" {
			callIdx = i
		} else if typ == "function_call_output" && callID == "orphan_call_1" {
			callOutIdx = i
		} else if strings.Contains(contentStr, "hook_output") {
			hookIdx = i
		} else if strings.Contains(contentStr, "user-input-2") {
			user2Idx = i
		}
	}

	if callIdx == -1 || callOutIdx == -1 {
		t.Fatalf("missing function_call or function_call_output: %+v", capturedInput)
	}
	if callOutIdx != callIdx+1 {
		t.Fatalf("function_call (index %d) must be immediately followed by function_call_output (index %d), but was split! items: %+v", callIdx, callOutIdx, capturedInput)
	}
	if hookIdx <= callOutIdx {
		t.Fatalf("hook output (index %d) must appear after repaired function_call_output (index %d)", hookIdx, callOutIdx)
	}
	if user2Idx <= hookIdx {
		t.Fatalf("current turn input (index %d) must appear after hook output (index %d)", user2Idx, hookIdx)
	}
}
