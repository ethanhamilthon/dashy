package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/shell"
)

func TestTurnsExecutionAndHistory(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var p struct {
			Instructions string
			Input        []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if requests == 1 {
			var last struct{ Role, Content string }
			_ = json.Unmarshal(p.Input[len(p.Input)-1], &last)
			if last.Role != "user" || last.Content != "hello" {
				t.Errorf("first request must contain user message, got %v", last)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")
	}))
	defer server.Close()
	a := &Agent{Shell: &shell.Runner{}, HistoryPath: filepath.Join(t.TempDir(), "history.json")}
	configureTestAgent(t, a, Settings{Provider: "test", Model: "test-model", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
	defer a.Close()
	for i := 0; i < 2; i++ {
		if err := a.Run(context.Background(), "hello", func(Event) {}); err != nil {
			t.Fatal(err)
		}
	}
	if requests != 2 {
		t.Fatalf("expected 2 requests, got %d", requests)
	}
}

func TestMissingModelOrProviderDoesNotSendRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	for _, settings := range []Settings{{}, {Provider: "test"}, {Model: "test-model"}} {
		a := &Agent{}
		configureTestAgent(t, a, settings)
		if _, err := a.request(context.Background(), func(Event) {}); err == nil {
			t.Fatal("expected configuration error")
		}
	}
	if requests != 0 {
		t.Fatalf("sent %d requests with incomplete settings", requests)
	}
}

func TestSDKRequestAndHistory(t *testing.T) {
	// Do not inherit OpenAI credentials or organization from the environment.
	t.Setenv("OPENAI_API_KEY", "wrong")
	t.Setenv("OPENAI_ORG_ID", "wrong-org")
	t.Setenv("OPENAI_PROJECT_ID", "wrong-project")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer selected-key" {
			t.Errorf("wrong endpoint or credentials: %s %q", r.URL, r.Header.Get("Authorization"))
		}
		if r.Header.Get("OpenAI-Organization") != "" || r.Header.Get("OpenAI-Project") != "" {
			t.Error("inherited unrelated OpenAI configuration")
		}
		var p struct {
			Model         string
			Store, Stream bool
			Reasoning     struct{ Effort, Summary string }
			Input         []json.RawMessage
			Tools         []struct {
				Name   string
				Strict bool
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Model != "custom" || p.Store || !p.Stream || p.Reasoning.Effort != "high" || p.Reasoning.Summary != "auto" || len(p.Tools) != 1 || p.Tools[0].Name != "bash" || !p.Tools[0].Strict {
			t.Errorf("unexpected payload: %+v", p)
		}
		if len(p.Input) != 2 || !strings.Contains(string(p.Input[1]), `"opaque":"keep"`) {
			t.Errorf("history not preserved: %s", p.Input)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// Multiline SSE is handled by the SDK, not by a line-based JSON parser.
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\n"+"data: \"delta\":\"hello\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"reasoning\",\"id\":\"r\",\"opaque\":\"keep\"}]}}\n\n")
	}))
	defer server.Close()
	a := &Agent{Shell: &shell.Runner{}, History: []json.RawMessage{raw(map[string]string{"role": "user", "content": "hi"}), json.RawMessage(`{"type":"reasoning","id":"r","opaque":"keep"}`)}}
	configureTestAgent(t, a, Settings{Provider: "test", Model: "custom", Effort: "high", Providers: map[string]Provider{"test": {BaseURL: server.URL + "/v1/", APIKey: "selected-key"}}})
	var text string
	items, err := a.request(context.Background(), func(e Event) { text += e.Text })
	if err != nil || text != "hello" || len(items) != 1 || !strings.Contains(string(items[0]), `"opaque":"keep"`) {
		t.Fatalf("items=%s text=%q err=%v", items, text, err)
	}
}

func TestSDKStreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"http", `{"error":{"message":"bad key"}}`, 401},
		{"failed", `data: {"type":"response.failed","response":{"error":{"message":"failed"}}}` + "\n\n", 200},
		{"incomplete", `data: {"type":"response.incomplete","response":{}}` + "\n\n", 200},
		{"error", `data: {"type":"error","message":"bad stream"}` + "\n\n", 200},
		{"truncated", `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			a := &Agent{Shell: &shell.Runner{}}
			configureTestAgent(t, a, Settings{Provider: "test", Model: "m", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
			if _, err := a.request(context.Background(), func(Event) {}); err == nil {
				t.Fatal("expected stream error")
			}
			if requests != 1 {
				t.Fatalf("unexpected retries: %d", requests)
			}
		})
	}
}

func TestSDKRequestCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	a := &Agent{Shell: &shell.Runner{}}
	configureTestAgent(t, a, Settings{Provider: "test", Model: "m", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.request(ctx, func(Event) {}); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not cancel")
	}
}

func TestSDKIgnoresEmptySSEFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "\n: heartbeat\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
		fmt.Fprint(w, "\n: heartbeat\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n\n")
	}))
	defer server.Close()
	a := &Agent{Shell: &shell.Runner{}}
	configureTestAgent(t, a, Settings{Provider: "test", Model: "m", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
	items, err := a.request(context.Background(), func(Event) {})
	if err != nil || len(items) != 1 || a.lastRequest.Usage == nil || a.lastRequest.Usage.Input != 10 {
		t.Fatalf("items=%s metadata=%+v err=%v", items, a.lastRequest, err)
	}
}
