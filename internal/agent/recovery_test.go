package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A call without output after a crash is closed with an interrupted result and is never rerun.
func TestInterruptedCallIsClosedWithoutRerunning(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	a.History = append(a.History, raw(map[string]any{"role": "user", "content": "start"}),
		raw(map[string]any{"type": "function_call", "call_id": "c1", "name": "bash", "arguments": `{"command":"touch marker"}`}))
	if _, err := turn(t, a, "continue"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Shell.Dir, "marker")); err == nil {
		t.Fatal("interrupted command was rerun")
	}
	joined := ""
	for _, item := range a.History {
		joined += string(item)
	}
	if !strings.Contains(joined, "interrupted") || !strings.Contains(joined, "c1") {
		t.Fatalf("no interrupted result in history: %s", joined)
	}
}

// A response is committed only when it completes: a failed stream leaves no partial item.
func TestFailedResponseLeavesNoPartialItem(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"partial answer"}]}}`,
			`{"type":"response.failed","response":{"error":{"code":"server_error","message":"cut off"}}}`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err == nil {
		t.Fatal("failed response reported as success")
	}
	items := strings.Join(storedItems(t, a), "\n")
	if strings.Contains(items, "partial answer") || !strings.Contains(items, "hi") {
		t.Fatalf("saved session %q", items)
	}
}
