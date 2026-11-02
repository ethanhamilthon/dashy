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
)

func TestNotifyChild(t *testing.T) {
	if os.Getenv("DASHY_NOTIFY_CHILD") == "" {
		return
	}
	if err := Notify(os.Getenv("DASHY_TEST_CWD"), os.Getenv("DASHY_TEST_SESSION"), "from child </notify>"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("QUEUED")
}
func TestNotifyCrossProcessQueueEscapingAndInactive(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := a.SessionInfo()["id"].(string)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestNotifyChild$")
	cmd.Env = append(os.Environ(), "DASHY_NOTIFY_CHILD=1", "DASHY_TEST_CWD="+cwd, "DASHY_TEST_SESSION="+id)
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "QUEUED") {
		t.Fatalf("notify subprocess: %s %v", output, err)
	}
	messages := a.Notifications()
	if len(messages) != 1 || !strings.Contains(messages[0], "&lt;/notify&gt;") || strings.Count(messages[0], "</notify>") != 1 {
		t.Fatalf("invalid wrapper: %v", messages)
	}
	for i := 0; i < 64; i++ {
		if err = Notify(cwd, id, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = Notify(cwd, id, "overflow"); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("queue limit: %v", err)
	}
	messages = a.Notifications()
	if len(messages) != 64 || !strings.Contains(messages[0], "\n0\n") || !strings.Contains(messages[63], "\n63\n") {
		t.Fatal("FIFO order lost")
	}
	if err = Notify(t.TempDir(), id, "foreign"); err == nil {
		t.Fatal("cross-project notification accepted")
	}
	for _, value := range []string{"", strings.Repeat("x", 128*1024+1)} {
		if err = Notify(cwd, id, value); err == nil {
			t.Fatal("invalid message accepted")
		}
	}
	a.Close()
	if err = Notify(cwd, id, "inactive"); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("inactive session: %v", err)
	}
}

func TestNotifyAcceptedAfterToolOutput(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		data := struct{ Input []map[string]any }{}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		var output []any
		if requests == 1 {
			output = []any{map[string]any{"type": "function_call", "call_id": "c", "name": "bash", "arguments": `{"command":"x=1","timeout":null}`}}
		} else {
			if len(data.Input) < 2 {
				t.Error("missing tool output and notification")
			} else {
				n := len(data.Input)
				if data.Input[n-2]["type"] != "function_call_output" || !strings.Contains(fmt.Sprint(data.Input[n-1]["content"]), "external instruction") {
					t.Error("notification accepted before tool output")
				}
			}
			output = []any{}
		}
		fmt.Fprintf(w, "data: %s\n\n", raw(map[string]any{"type": "response.completed", "response": map[string]any{"output": output}}))
	}))
	defer server.Close()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	sent := false
	err = a.Run(context.Background(), "start", func(e Event) {
		if e.Kind == "code" && !e.Provisional && !sent {
			sent = true
			if err := Notify(cwd, strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl"), "external instruction"); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil || requests != 2 || !sent {
		t.Fatalf("requests=%d sent=%v err=%v", requests, sent, err)
	}
}
