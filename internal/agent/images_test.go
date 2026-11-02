package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dashy/internal/shell"
)

func TestViewDeliveredAfterAllToolOutputsAndPersists(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "image.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	requests := 0
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct{ Input []json.RawMessage }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			items := []any{
				map[string]any{"type": "function_call", "name": "bash", "call_id": "v1", "arguments": string(raw(map[string]any{"command": "echo first", "timeout": nil}))},
				map[string]any{"type": "function_call", "name": "bash", "call_id": "v2", "arguments": string(raw(map[string]any{"command": "echo second call", "timeout": 5}))},
			}
			fmt.Fprintf(w, "data: %s\n\n", raw(map[string]any{"type": "response.completed", "response": map[string]any{"output": items}}))
			return
		}
		if len(body.Input) != 6 {
			t.Errorf("unexpected item count: %d", len(body.Input))
			return
		}
		for i, id := range []string{"v1", "v2"} {
			var item struct {
				Type, Output string
				CallID       string `json:"call_id"`
			}
			if err := json.Unmarshal(body.Input[3+i], &item); err != nil {
				t.Error(err)
			}
			if item.Type != "function_call_output" || item.CallID != id || strings.Contains(item.Output, "data:image") {
				t.Errorf("wrong output pairing or image leaked into tool output: %+v", item)
			}
		}
		var msg struct {
			Role    string
			Content []struct {
				Type, Text string
				URL        string `json:"image_url"`
			}
		}
		if err := json.Unmarshal(body.Input[5], &msg); err != nil {
			t.Error(err)
		}
		if len(msg.Content) > 0 && !strings.Contains(msg.Content[0].Text, path) {
			t.Error("original path missing from view notification")
		}
		if msg.Role != "user" || len(msg.Content) != 2 || msg.Content[0].Type != "input_text" || !strings.Contains(msg.Content[0].Text, "<notify>") || msg.Content[1].Type != "input_image" || !strings.HasPrefix(msg.Content[1].URL, "data:image/png;base64,") {
			t.Errorf("missing queued image: %+v", msg)
		}
		captured = string(body.Input[5])
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
	defer server.Close()
	a, err := New(dir, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "test", Model: "test", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
	removed := false
	err = a.Run(context.Background(), "inspect image", func(e Event) {
		if e.Kind == "output" && !removed {
			// dashy view captures bytes in the CLI process, then uses the socket.
			image, err := shell.CaptureImage(path)
			if err == nil {
				err = Send(a.shellEnv()["DASHY_ENDPOINT"], Message{Image: &image})
			}
			if err != nil {
				t.Error(err)
			}
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
			removed = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || captured == "" {
		t.Fatalf("image not delivered: requests=%d", requests)
	}
	a.Close() // release ownership so the session can be reopened
	restored, err := New(dir, ConfigFiles{}, a.SessionInfo()["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if len(restored.History) != 6 || string(restored.History[5]) != captured {
		t.Fatal("image content not preserved in saved history")
	}
	records := RecordsForItem(restored.History[5])
	if len(records) != 1 || records[0].Kind != "notice" {
		t.Fatalf("restored image notice: %v", records)
	}
}
