package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dashy/internal/agent"
)

func load(t *testing.T, w *wire, id int, session, cwd string) frame {
	t.Helper()
	w.request(t, id, "session/load", map[string]any{"sessionId": session, "cwd": cwd, "mcpServers": []any{}})
	return w.response(t, id)
}

func TestLoadRestoresHistoryAndProviderContext(t *testing.T) {
	requests := make(chan string, 8)
	settings(t, func(body string) string {
		requests <- body
		if !strings.Contains(body, "function_call_output") {
			return command("mkdir replay-marker")
		}
		return answer("remembered")
	})
	dir := t.TempDir()
	w := connect(t)
	w.initialize(t)
	id := w.newSession(t, 2, dir)
	w.prompt(t, 3, id, "remember this conversation")
	if f := w.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	<-requests
	<-requests
	if err := os.Remove(filepath.Join(dir, "replay-marker")); err != nil {
		t.Fatal(err)
	}
	// A new connection owns no in-memory agent: the load must read SQLite.
	w.conn.Close()
	<-w.done
	w2 := connect(t)
	w2.initialize(t)
	if f := load(t, w2, 2, id, dir); f.Error != nil {
		t.Fatal(f.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "replay-marker")); !os.IsNotExist(err) {
		t.Fatalf("history replay executed a tool: %v", err)
	}
	counts := map[string]int{}
	for _, raw := range w2.updates {
		var notification struct {
			Update struct {
				Kind string `json:"sessionUpdate"`
			}
		}
		if err := json.Unmarshal(raw, &notification); err != nil {
			t.Fatal(err)
		}
		counts[notification.Update.Kind]++
	}
	for _, kind := range []string{"user_message_chunk", "agent_message_chunk", "tool_call", "tool_call_update"} {
		if counts[kind] != 1 {
			t.Fatalf("replay counts: %v", counts)
		}
	}
	w2.prompt(t, 3, id, "what do you remember?")
	if f := w2.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	body := <-requests
	for _, text := range []string{"remember this conversation", "remembered", "what do you remember?"} {
		if !strings.Contains(body, text) {
			t.Fatalf("provider lost history %q: %s", text, body)
		}
	}
	// Loading an already open session replays without appending to its history.
	w2.updates = nil
	if f := load(t, w2, 4, id, dir); f.Error != nil {
		t.Fatal(f.Error)
	}
	users := 0
	for _, raw := range w2.updates {
		if strings.Contains(string(raw), "user_message_chunk") {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("load duplicated or lost messages: %d", users)
	}
}

func TestLoadLargeHistoryBackpressure(t *testing.T) {
	settings(t, func(string) string { return answer("answer") })
	dir := t.TempDir()
	a, err := agent.New(dir, agent.Options{Mode: "acp", NoGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 80 {
		if err := a.Run(context.Background(), fmt.Sprintf("message %d", i), func(agent.Event) {}); err != nil {
			a.Close()
			t.Fatal(err)
		}
	}
	id := a.SessionID()
	a.Close()
	w := connect(t)
	w.initialize(t)
	if f := load(t, w, 2, id, dir); f.Error != nil {
		t.Fatal(f.Error)
	}
	if len(w.updates) != 161 {
		t.Fatalf("large replay: %d updates", len(w.updates))
	}
	w.updates = nil
	if f := load(t, w, 3, id, dir); f.Error != nil {
		t.Fatal(f.Error)
	}
	if len(w.updates) != 161 {
		t.Fatalf("repeat replay: %d updates", len(w.updates))
	}
}

func TestLoadValidation(t *testing.T) {
	settings(t, func(string) string { return answer("ok") })
	w := connect(t)
	w.initialize(t)
	dir := t.TempDir()
	for _, id := range []string{"", "../escape", "missing"} {
		if f := load(t, w, 2, id, dir); f.Error == nil {
			t.Fatalf("loaded invalid session %q", id)
		}
	}
	id := w.newSession(t, 3, dir)
	if f := load(t, w, 4, id, t.TempDir()); f.Error == nil {
		t.Fatal("loaded session in wrong workspace")
	}
}

func TestContextUsageAndModelSwitch(t *testing.T) {
	settings(t, func(string) string {
		return `data: {"type":"response.completed","response":{"usage":{"input_tokens":42,"output_tokens":7},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}` + "\n\n"
	}, `{"data":[{"id":"m","context_length":128000},{"id":"m2","context_window":64000}]}`)
	w := connect(t)
	w.initialize(t)
	dir := t.TempDir()
	id := w.newSession(t, 2, dir)
	check := func(size int64, used int64) {
		t.Helper()
		var latest struct {
			Update struct {
				Kind       string `json:"sessionUpdate"`
				Used, Size int64
			}
		}
		found := false
		for _, raw := range w.updates {
			var n struct {
				Update struct {
					Kind       string `json:"sessionUpdate"`
					Used, Size int64
				}
			}
			if err := json.Unmarshal(raw, &n); err != nil {
				t.Fatal(err)
			}
			if n.Update.Kind == "usage_update" {
				latest = n
				found = true
			}
		}
		if !found || latest.Update.Size != size || (used >= 0 && latest.Update.Used != used) {
			t.Fatalf("context update: %+v (found=%v), want %d/%d", latest, found, used, size)
		}
	}
	check(128000, -1)
	w.updates = nil
	w.prompt(t, 3, id, "hello")
	if f := w.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	check(128000, 42)
	w.request(t, 4, "session/set_config_option", map[string]string{"sessionId": id, "configId": "model", "value": "p/m2"})
	if f := w.response(t, 4); f.Error != nil {
		t.Fatal(f.Error)
	}
	w.updates = nil
	w.prompt(t, 5, id, "hello again")
	if f := w.response(t, 5); f.Error != nil {
		t.Fatal(f.Error)
	}
	check(64000, 42)
	w.conn.Close()
	<-w.done
	w = connect(t)
	w.initialize(t)
	if f := load(t, w, 2, id, dir); f.Error != nil || !strings.Contains(string(f.Result), "p/m2") {
		t.Fatal(f)
	}
	check(64000, -1)
}
