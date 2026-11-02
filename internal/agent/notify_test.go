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

func TestCleanAttachments(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	oldFile := filepath.Join(dir, "old.png")
	if err := os.WriteFile(oldFile, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	eightDaysAgo := now.Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, eightDaysAgo, eightDaysAgo); err != nil {
		t.Fatal(err)
	}

	newFile := filepath.Join(dir, "new.png")
	if err := os.WriteFile(newFile, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := CleanAttachments(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("file older than 7 days was not deleted")
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatal("recent file was deleted")
	}
}

func TestSocketPathLimits(t *testing.T) {
	// Standard short path
	p1 := SocketPath("/tmp/run", "sess-1")
	if p1 != "/tmp/run/sess-1.sock" {
		t.Fatalf("expected direct path, got %q", p1)
	}
	if len(p1) >= 104 {
		t.Fatalf("path too long: %d", len(p1))
	}

	// Long session ID with moderate runDir: uses short hash
	longID := strings.Repeat("long-session-id-component-", 4)
	p2 := SocketPath("/tmp/run", longID)
	if len(p2) >= 104 {
		t.Fatalf("hash path too long: %d (%q)", len(p2), p2)
	}
	if !strings.HasPrefix(p2, "/tmp/run/") || !strings.HasSuffix(p2, ".sock") {
		t.Fatalf("unexpected hash path: %q", p2)
	}

	// Long runDir: falls back to /tmp/dashy-*.sock
	longDir := strings.Repeat("/very_long_directory_name", 6)
	p3 := SocketPath(longDir, "sess-1")
	if len(p3) >= 104 {
		t.Fatalf("fallback path too long: %d (%q)", len(p3), p3)
	}
	if !strings.HasPrefix(p3, "/tmp/dashy-") {
		t.Fatalf("expected /tmp fallback, got %q", p3)
	}
}
