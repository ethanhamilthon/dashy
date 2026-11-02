package agent

import (
	"strings"
	"testing"
)

// While a bash call streams, the display shows the complete prefix of its command;
// escapes and multi-byte characters split by a delta are withheld until complete.
func TestPartialCodeShowsCompletePrefix(t *testing.T) {
	cases := map[string]string{
		`{"command":"echo hi`:         "echo hi",
		`{"command":"printf \"a\`:     `printf "a`,
		`{"command":"x", "timeout":1`: "x",
		`{"command":"`:                "",
		`{"other":1`:                  "",
	}
	for arguments, want := range cases {
		if got := partialCode(arguments); got != want {
			t.Errorf("partialCode(%q) = %q, want %q", arguments, got, want)
		}
	}
}

// Streamed argument deltas are display-only: the call is saved once, when its item completes.
func TestToolCallStreamingIsDisplayOnly(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n > 1 {
			return textReply("done")
		}
		return sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"bash","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":\"echo "}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"hi\"}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"command\":\"echo hi\"}"}}`,
			`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"c1","name":"bash","arguments":"{\"command\":\"echo hi\"}"}]}}`)
	}}
	a := newTestAgent(t, fp, Options{})
	events, err := turn(t, a, "run it")
	if err != nil {
		t.Fatal(err)
	}
	sawPreview := false
	for _, e := range events {
		sawPreview = sawPreview || (e.Kind == "code" && e.Provisional && e.Text == "echo hi")
	}
	calls := 0
	for _, item := range a.History {
		if strings.Contains(string(item), `"function_call"`) {
			calls++
		}
	}
	if !sawPreview || calls != 1 {
		t.Fatalf("preview %v, saved calls %d", sawPreview, calls)
	}
}
