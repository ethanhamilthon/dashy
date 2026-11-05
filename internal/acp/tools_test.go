package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrustedExecutionAndTerminalToolEvents(t *testing.T) {
	dir := t.TempDir()
	settings(t, func(body string) string {
		if strings.Contains(body, "function_call_output") {
			return answer("done")
		}
		return command("touch ran; exit 7")
	})
	w := connect(t)
	w.initialize(t)
	session := w.newSession(t, 2, dir)
	w.prompt(t, 3, session, "run")
	if f := w.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran")); err != nil {
		t.Fatal(err)
	}
	states := []string{}
	for _, update := range w.updates {
		var p struct {
			Update struct {
				SessionUpdate, Status, ToolCallID string
				RawOutput                         struct{ ExitCode int }
			}
		}
		_ = json.Unmarshal(update, &p)
		if strings.HasPrefix(p.Update.SessionUpdate, "tool_call") {
			if p.Update.ToolCallID == "" {
				t.Fatal("missing tool ID")
			}
			states = append(states, p.Update.Status)
			if p.Update.Status == "failed" && p.Update.RawOutput.ExitCode != 7 {
				t.Fatal(string(update))
			}
		}
	}
	if strings.Join(states, ",") != "pending,in_progress,failed" {
		t.Fatal(states)
	}
}

func TestCancellationDuringToolAndOverlap(t *testing.T) {
	settings(t, func(string) string { return command("sleep 60") })
	w := connect(t)
	w.initialize(t)
	session := w.newSession(t, 2, t.TempDir())
	w.prompt(t, 3, session, "work")
	for {
		f := w.next(t)
		if f.Method == "session/update" && strings.Contains(string(f.Params), "in_progress") {
			break
		}
		if f.Method == "" {
			t.Fatalf("tool ended too early: %+v", f)
		}
	}
	w.prompt(t, 4, session, "overlap")
	if f := w.response(t, 4); f.Error == nil || f.Error.Code != -32600 {
		t.Fatal(f)
	}
	w.cancel(t, session)
	w.cancel(t, session)
	if f := w.response(t, 3); f.Error != nil || !strings.Contains(string(f.Result), "cancelled") {
		t.Fatal(f)
	}
	w.prompt(t, 5, session, "again")
	w.send(t, map[string]any{"jsonrpc": "2.0", "method": "$/cancel_request", "params": map[string]int{"id": 5}})
	if f := w.response(t, 5); f.Error == nil || f.Error.Code != -32800 {
		t.Fatal(f)
	}
}

func TestTwoSessionsStayIsolated(t *testing.T) {
	var mu sync.Mutex
	var inputs []string
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	settings(t, func(body string) string {
		var p struct{ Input json.RawMessage }
		_ = json.Unmarshal([]byte(body), &p)
		input := string(p.Input)
		mu.Lock()
		inputs = append(inputs, input)
		mu.Unlock()
		if strings.Contains(input, `"first"`) {
			<-gate
			return answer("first done")
		}
		if strings.Contains(input, "function_call_output") {
			return answer("done")
		}
		return command("pwd")
	})
	w := connect(t)
	w.initialize(t)
	dir1, dir2 := t.TempDir(), t.TempDir()
	a := w.newSession(t, 2, dir1)
	b := w.newSession(t, 3, dir2)
	w.prompt(t, 4, a, "first")
	w.prompt(t, 5, b, "second")
	if f := w.response(t, 5); f.Error != nil {
		t.Fatal(f)
	}
	w.cancel(t, a)
	if f := w.response(t, 4); !strings.Contains(string(f.Result), "cancelled") {
		t.Fatal(f)
	}
	release()
	canonical, _ := filepath.EvalSymlinks(dir2)
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, input := range inputs {
		if strings.Contains(input, "function_call_output") {
			found = true
			if !strings.Contains(input, canonical) || strings.Contains(input, `"first"`) {
				t.Fatalf("session context leaked: %s", input)
			}
		}
	}
	if !found {
		t.Fatal("second session never completed its tool")
	}
}

func TestInstructionBlocksExecuteWithoutApprovals(t *testing.T) {
	settings(t, func(string) string { return answer("ok") })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DASHY.md"), []byte("```dashy\ntouch executed\n```"), 0600); err != nil {
		t.Fatal(err)
	}
	w := connect(t)
	w.initialize(t)
	session := w.newSession(t, 2, dir)
	w.prompt(t, 3, session, "hi")
	if f := w.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	if _, err := os.Stat(filepath.Join(dir, "executed")); err != nil {
		t.Fatal(err)
	}
	for _, update := range w.updates {
		if strings.Contains(string(update), `"tool_call`) {
			t.Fatal("instruction block reported as a tool call:", string(update))
		}
	}
}

func TestDisconnectCancelsAndReleasesSession(t *testing.T) {
	home := settings(t, func(string) string { return command("sleep 60") })
	w := connect(t)
	w.initialize(t)
	dir := t.TempDir()
	session := w.newSession(t, 2, dir)
	w.prompt(t, 3, session, "run")
	for {
		f := w.next(t)
		if f.Method == "session/update" && strings.Contains(string(f.Params), "in_progress") {
			break
		}
	}
	w.conn.Close()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown hung")
	}
	if _, err := os.Stat(filepath.Join(home, "run", session+".sock")); err == nil {
		t.Fatal("session socket leaked")
	}
}
