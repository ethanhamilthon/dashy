package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPartialCodeEscapes(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"co`, ""}, {`{"command":"abc`, "abc"}, {`{"command":"a\n`, "a\n"},
		{`{"command":"x\`, "x"}, {`{"command":"\ud83d`, ""}, {`{"command":"\ud83d\ude80`, "🚀"},
		{`{"command":"\\ud83d`, `\ud83d`}, {`{"command":"print(\"hi\")"}`, `print("hi")`},
	} {
		if got := partialCode(tc.input); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestToolCallStreamingIsDisplayOnlyAndSequential(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			t.Setenv("DASHY_HOME", t.TempDir())
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(v any) { fmt.Fprintf(w, "data: %s\n\n", raw(v)) }
				if requests == 1 {
					for i := 0; i < 2; i++ {
						send(map[string]any{"type": "response.output_item.added", "output_index": i, "item": map[string]any{"type": "function_call", "id": fmt.Sprint(i), "call_id": fmt.Sprint(i), "name": "bash", "arguments": ""}})
					}
					for _, part := range []struct {
						i     int
						delta string
					}{{0, `{"command":"echo `}, {1, `{"command":"echo `}, {0, `10"`}, {1, `15"`}} {
						send(map[string]any{"type": "response.function_call_arguments.delta", "output_index": part.i, "delta": part.delta})
					}
					if failed {
						send(map[string]string{"type": "error", "message": "stream interrupted"})
						return
					}
					output := []any{}
					for i, code := range []string{"echo 10", "echo 15"} {
						args := string(raw(map[string]any{"command": code, "timeout": nil}))
						send(map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "arguments": args})
						item := map[string]any{"type": "function_call", "id": fmt.Sprint(i), "call_id": fmt.Sprint(i), "name": "bash", "arguments": args}
						send(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
						output = append(output, item)
					}
					send(map[string]any{"type": "response.completed", "response": map[string]any{"output": output, "usage": map[string]int{"input_tokens": 100, "output_tokens": 20}}})
				} else {
					var p struct{ Input []json.RawMessage }
					_ = json.NewDecoder(r.Body).Decode(&p)
					if !strings.Contains(string(p.Input[len(p.Input)-1]), "15") {
						t.Error("calls did not execute sequentially")
					}
					send(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{}}})
				}
			}))
			defer server.Close()
			marker := filepath.Join(t.TempDir(), "marker")
			a, err := New(t.TempDir(), ConfigFiles{})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
			var provisional []Event
			err = a.Run(context.Background(), "go", func(e Event) {
				if e.Provisional {
					provisional = append(provisional, e)
					if e.Meta != nil {
						t.Error("provisional usage invented")
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Errorf("tool executed before response completed")
					}
				}
			})
			if (err != nil) != failed {
				t.Fatalf("unexpected result: %v", err)
			}
			if len(provisional) < 6 {
				t.Fatal("arguments were not streamed")
			}
			var calls []Record
			for _, record := range a.Records() {
				if record.Kind == "code" {
					calls = append(calls, record)
				}
			}
			if failed {
				if len(calls) != 0 {
					t.Fatalf("unfinished calls saved: %+v", calls)
				}
				if len(a.History) != 1 {
					t.Fatalf("partial calls committed to API history: %d", len(a.History))
				}
			} else {
				if len(calls) != 2 || calls[0].Text != "echo 10" || calls[1].Text != "echo 15" {
					t.Fatalf("calls mixed or duplicated: %+v", calls)
				}
				if calls[0].Meta.Usage.Input != 100 || calls[1].Meta.Usage.Input != 100 {
					t.Fatal("request metadata missing")
				}
			}
			a.Close() // release ownership so the session can be reopened
			b, err := New(a.Shell.Dir, ConfigFiles{}, a.SessionInfo()["id"].(string))
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			if failed {
				if len(b.Records()) != 1 {
					t.Fatalf("expected only initial user message on reopen after failure, got %d", len(b.Records()))
				}
			} else {
				if len(b.Records()) != len(a.Records()) {
					t.Fatal("stream display records not persisted")
				}
			}
		})
	}
}
