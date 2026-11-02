package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSteeringAtSafeBoundaries(t *testing.T) {
	for _, tools := range []bool{false, true} {
		t.Run(fmt.Sprint(tools), func(t *testing.T) {
			t.Setenv("DASHY_HOME", t.TempDir())
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var payload struct{ Input []json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if requests == 1 {
					fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"First\"}\n\n")
					output := []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "First"}}}}
					if tools {
						for _, id := range []string{"a", "b"} {
							output = append(output, map[string]any{"type": "function_call", "name": "bash", "call_id": id, "arguments": string(raw(map[string]any{"command": "echo 42", "timeout": nil}))})
						}
					}
					fmt.Fprintf(w, "data: %s\n\n", raw(map[string]any{"type": "response.completed", "response": map[string]any{"output": output}}))
				} else {
					var kinds, texts []string
					for _, item := range payload.Input {
						var v struct{ Role, Type, Content string }
						// Assistant content is structured, so inspect only the user items separately.
						var envelope map[string]json.RawMessage
						_ = json.Unmarshal(item, &envelope)
						_ = json.Unmarshal(envelope["role"], &v.Role)
						_ = json.Unmarshal(envelope["type"], &v.Type)
						_ = json.Unmarshal(envelope["content"], &v.Content)
						if v.Role == "user" && strings.HasPrefix(v.Content, "<notify>") {
							continue
						}
						if v.Role == "user" {
							kinds = append(kinds, "user")
							texts = append(texts, v.Content)
						} else {
							kinds = append(kinds, v.Type)
						}
					}
					want := []string{"user", "message", "user", "user"}
					if tools {
						want = []string{"user", "message", "function_call", "function_call", "function_call_output", "function_call_output", "user", "user"}
					}
					if !reflect.DeepEqual(kinds, want) || !reflect.DeepEqual(texts, []string{"start", "adjust first", "adjust second"}) {
						t.Errorf("unsafe steering order: %v %v", kinds, texts)
					}
					fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Adjusted\"}]}]}}\n\n")
				}
			}))
			defer server.Close()
			a, err := New(t.TempDir(), ConfigFiles{})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
			steering := &Steering{}
			queued := false
			var live []Record
			err = a.Run(context.Background(), "start", func(e Event) {
				live = ApplyEvent(live, e)
				if e.Kind == "delta" && !queued {
					steering.Push("adjust first")
					steering.Push("adjust second")
					queued = true
				}
			}, steering)
			if err != nil {
				t.Fatal(err)
			}
			if requests != 2 || !reflect.DeepEqual(live, a.Records()) {
				t.Fatalf("steering/live journal mismatch: requests=%d", requests)
			}
			a.Close() // release ownership so the session can be reopened
			restored, err := New(a.Shell.Dir, ConfigFiles{}, a.SessionInfo()["id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if len(restored.Records()) != len(live) {
				t.Fatalf("restored count %d != live count %d", len(restored.Records()), len(live))
			}
			for i := range live {
				if restored.Records()[i].Kind != live[i].Kind || restored.Records()[i].Text != live[i].Text {
					t.Fatalf("record %d mismatch: got %+v, want %+v", i, restored.Records()[i], live[i])
				}
			}
			if !strings.Contains(live[len(live)-1].Text, "Adjusted") {
				t.Fatal("follow-up request missing")
			}
		})
	}
}
