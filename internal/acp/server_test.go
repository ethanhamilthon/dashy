package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dashy/internal/agent"

	sdk "github.com/coder/acp-go-sdk"
)

func answer(text string) string {
	return fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}]}}\n\n", text)
}
func command(cmd string) string {
	args, _ := json.Marshal(map[string]any{"command": cmd, "timeout": 2})
	item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": "call", "name": "bash", "arguments": string(args)})
	return fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"output\":[%s]}}\n\n", item)
}
func settings(t *testing.T, reply func(string) string, catalog ...string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			data := `{"data":[{"id":"m"},{"id":"m2"}]}`
			if len(catalog) > 0 {
				data = catalog[0]
			}
			fmt.Fprint(w, data)
			return
		}
		data, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, reply(string(data)))
	}))
	t.Cleanup(provider.Close)
	s := agent.DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = agent.Provider{BaseURL: provider.URL, APIKey: "sk-test-secret"}
	if err := agent.SaveSettings(home, s); err != nil {
		t.Fatal(err)
	}
	return home
}

type wire struct {
	conn    net.Conn
	decoder *json.Decoder
	done    chan struct{}
	updates []json.RawMessage
}

func connect(t *testing.T) *wire {
	t.Helper()
	client, server := net.Pipe()
	w := &wire{conn: client, decoder: json.NewDecoder(client), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		_ = Run(context.Background(), server, server, Options{Version: "test", NoGlobal: true})
	}()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-w.done:
		case <-time.After(5 * time.Second):
			t.Error("ACP shutdown hung")
		}
	})
	return w
}
func (w *wire) send(t *testing.T, v any) {
	t.Helper()
	w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(w.conn).Encode(v); err != nil {
		t.Fatal(err)
	}
}
func (w *wire) request(t *testing.T, id int, method string, params any) {
	w.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}
func (w *wire) cancel(t *testing.T, id string) {
	w.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": id}})
}
func (w *wire) next(t *testing.T) frame {
	t.Helper()
	w.conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	var f frame
	if err := w.decoder.Decode(&f); err != nil {
		t.Fatal(err)
	}
	if f.JSONRPC != "2.0" {
		t.Fatalf("invalid envelope: %+v", f)
	}
	if f.Method == "session/update" {
		var update sdk.SessionNotification
		if err := json.Unmarshal(f.Params, &update); err != nil {
			t.Fatalf("SDK rejected update: %s: %v", f.Params, err)
		}
		w.updates = append(w.updates, append(json.RawMessage(nil), f.Params...))
	}
	return f
}
func (w *wire) response(t *testing.T, id int) frame {
	t.Helper()
	for {
		f := w.next(t)
		if string(f.ID) == fmt.Sprint(id) && f.Method == "" {
			return f
		}
		if f.Method == "session/request_permission" {
			t.Fatal("unexpected permission request")
		}
	}
}
func (w *wire) initialize(t *testing.T) {
	w.request(t, 1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	f := w.response(t, 1)
	if f.Error != nil {
		t.Fatal(f.Error)
	}
}
func (w *wire) newSession(t *testing.T, id int, cwd string) string {
	w.request(t, id, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	f := w.response(t, id)
	if f.Error != nil {
		t.Fatal(f.Error)
	}
	var result struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(f.Result, &result); err != nil || result.SessionID == "" {
		t.Fatalf("new session: %s %v", f.Result, err)
	}
	return result.SessionID
}
func (w *wire) prompt(t *testing.T, id int, session, text string) {
	w.request(t, id, "session/prompt", map[string]any{"sessionId": session, "prompt": []any{map[string]string{"type": "text", "text": text}}})
}

func TestTextTurnAndUnsupportedMethods(t *testing.T) {
	settings(t, func(string) string {
		return "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" + answer("hello")
	})
	w := connect(t)
	w.request(t, 7, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	if f := w.response(t, 7); f.Error == nil || f.Error.Code != -32600 {
		t.Fatal(f)
	}
	w.initialize(t)
	session := w.newSession(t, 2, t.TempDir())
	w.prompt(t, 3, session, "hi")
	f := w.response(t, 3)
	if f.Error != nil || !strings.Contains(string(f.Result), "end_turn") {
		t.Fatal(f)
	}
	messages := 0
	for _, update := range w.updates {
		if strings.Contains(string(update), "agent_message_chunk") {
			messages++
			if !strings.Contains(string(update), "hello") {
				t.Fatal(string(update))
			}
		}
	}
	if messages != 1 {
		t.Fatalf("duplicate streamed/final messages: %d", messages)
	}
	for _, method := range []string{"session/fork", "session/set_mode", "fs/read_text_file"} {
		w.request(t, 4, method, map[string]string{"sessionId": session})
		if f := w.response(t, 4); f.Error == nil || f.Error.Code != -32601 {
			t.Fatal(f)
		}
	}
	w.request(t, 5, "session/prompt", map[string]any{"sessionId": session, "prompt": []any{map[string]string{"type": "image", "data": "bad", "mimeType": "image/png"}}})
	if f := w.response(t, 5); f.Error == nil || f.Error.Code != -32602 {
		t.Fatal(f)
	}
}
