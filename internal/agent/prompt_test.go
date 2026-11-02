package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeContractOnlyInToolDescription(t *testing.T) {
	for _, system := range []string{"", "Custom instructions retained verbatim."} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Instructions string
				Tools        []struct{ Name, Description string }
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := system
			if body.Instructions != want {
				t.Errorf("system prompt changed: %q; want %q", body.Instructions, want)
			}
			if len(body.Tools) != 1 || body.Tools[0].Name != "bash" || body.Tools[0].Description != bashToolDescription {
				t.Errorf("missing runtime contract: %+v", body.Tools)
			}
			if strings.Contains(body.Instructions, "<prefix>") || strings.Contains(body.Instructions, "<prefix_output>") {
				t.Error("retired prefix block leaked into instructions")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
		}))
		a := &Agent{System: system}
		a.applySettings(Settings{Provider: "test", Model: "test", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
		_, err := a.request(context.Background(), func(Event) {})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBashToolDescriptionContract(t *testing.T) {
	requiredSnippets := []string{
		"/bin/bash --noprofile --norc",
		"timeout: seconds, null = 120, maximum 600",
		"max_output: bytes kept, null = 8192, 0 = no truncation",
		"dashy notify",
		"dashy -p \"TASK\"",
		"dashy view PATH...",
		"dashy --help",
		"DASHY_SESSION_ID",
		"DASHY_ENDPOINT",
		"```dashy",
	}
	for _, snippet := range requiredSnippets {
		if !strings.Contains(BashToolDescription, snippet) {
			t.Errorf("BashToolDescription missing required snippet %q", snippet)
		}
	}
}
