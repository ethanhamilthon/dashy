package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeProvider answers requests. reply gets the request number (from 1) and the
// raw body and returns the response body: SSE for streamed requests.
type fakeProvider struct {
	mu     sync.Mutex
	bodies []string
	reply  func(n int, body string) string
	status func(n int) int // optional: HTTP status of request n; 0 means 200
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(data))
	n := len(f.bodies)
	f.mu.Unlock()
	if f.status != nil && f.status(n) != 0 {
		w.WriteHeader(f.status(n))
		fmt.Fprint(w, `{"error":{"message":"transient"}}`)
		return
	}
	out := f.reply(n, string(data))
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(string(data), `"stream":true`) {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	fmt.Fprint(w, out)
}

func (f *fakeProvider) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// sse frames one streamed response.
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "data: %s\n\n", e)
	}
	return b.String()
}

// textReply is a streamed response with one assistant message.
func textReply(text string) string {
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}}`, text))
}

// bashReply is a streamed response with one bash call.
func bashReply(callID, command string) string {
	args, _ := json.Marshal(map[string]any{"command": command, "timeout": nil, "max_output": nil})
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":%q,"name":"bash","arguments":%q}]}}`, callID, args))
}

// newTestAgent opens a project in a temp directory against fp. DASHY_HOME is temporary too.
func newTestAgent(t *testing.T, fp *fakeProvider, opts Options) *Agent {
	return newTestAgentWith(t, fp, opts, nil)
}

// newTestAgentWith is newTestAgent after pre has written files into home and the project directory.
func newTestAgentWith(t *testing.T, fp *fakeProvider, opts Options, pre func(home, dir string)) *Agent {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)
	s := DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = Provider{BaseURL: server.URL}
	if err := writeSettings(filepath.Join(home, "settings.json"), s); err != nil {
		t.Fatal(err)
	}
	if pre != nil {
		pre(home, dir)
	}
	a, err := New(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// turn runs one message and returns its events.
func turn(t *testing.T, a *Agent, text string) ([]Event, error) {
	t.Helper()
	var events []Event
	err := a.Run(context.Background(), text, func(e Event) { events = append(events, e) })
	return events, err
}

func TestToolCallThenAnswerIsCommittedInOrder(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, r := range a.Records() {
		kinds = append(kinds, r.Kind+":"+strings.TrimSpace(r.Text))
	}
	if got := strings.Join(kinds, "|"); got != "user:go|code:echo hi|output:hi|assistant:done" {
		t.Fatalf("records = %s", got)
	}
	if final := a.FinalText(); final != "done" {
		t.Fatalf("FinalText = %q", final)
	}
	if n := len(fp.requests()); n != 2 {
		t.Fatalf("requests = %d", n)
	}
}

func TestFinalTextIgnoresEarlierTurns(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, _ string) string { return textReply(fmt.Sprintf("answer %d", n)) }}
	a := newTestAgent(t, fp, Options{})
	turn(t, a, "one")
	turn(t, a, "two")
	if final := a.FinalText(); final != "answer 2" {
		t.Fatalf("FinalText = %q", final)
	}
}

func TestMissingModelSendsNoRequest(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("x") }}
	a := newTestAgent(t, fp, Options{})
	a.applySettings(DefaultSettings())
	if _, err := turn(t, a, "hi"); err == nil || !strings.Contains(err.Error(), "No provider configured") {
		t.Fatalf("err = %v", err)
	}
	if len(fp.requests()) != 0 {
		t.Fatal("a request was sent without a model")
	}
}

// compactTurns makes history with n user turns of size bytes each.
func compactTurns(t *testing.T, a *Agent, n, size int) {
	t.Helper()
	for i := 0; i < n; i++ {
		a.message("user", fmt.Sprintf("turn %d %s", i, strings.Repeat("x", size)))
		a.message("assistant", fmt.Sprintf("answer %d", i))
	}
}

func TestManualCompactionReplacesOldItemsWithSummary(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if strings.Contains(body, `"stream":true`) {
			return textReply("after")
		}
		return `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"SUMMARY","annotations":[]}]}]}`
	}}
	a := newTestAgent(t, fp, Options{})
	compactTurns(t, a, 6, 10)
	text, err := a.Compact(context.Background(), "")
	if err != nil || !strings.Contains(text, "Compacted") {
		t.Fatalf("compact: %q %v", text, err)
	}
	records := a.Records()
	joined := fmt.Sprint(records)
	if strings.Contains(joined, "turn 0") || !strings.Contains(joined, "SUMMARY") {
		t.Fatalf("records after compaction: %v", records)
	}
	if got := a.ContextTokens(); got <= 0 {
		t.Fatalf("context after compaction = %d", got)
	}
}

// estimateTokens counts history bytes, so a large history crosses the threshold.
func TestEstimateTokensCountsImagesAndText(t *testing.T) {
	big := json.RawMessage(`{"role":"user","content":"` + strings.Repeat("a", 4*CompactThreshold) + `"}`)
	if got := estimateTokens([]json.RawMessage{big}); got <= CompactThreshold {
		t.Fatalf("estimate %d", got)
	}
	img := json.RawMessage(`{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}`)
	if got := estimateTokens([]json.RawMessage{img}); got < imageTokens {
		t.Fatalf("image not charged: %d", got)
	}
}

func TestStreamFailureSurfacesAsError(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// Every request carries the same bash tool description and a strict schema.
func TestRequestCarriesStaticToolDescription(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Tools []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Strict      bool   `json:"strict"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(fp.requests()[0]), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "bash" || body.Tools[0].Description != BashToolDescription || !body.Tools[0].Strict {
		t.Fatalf("tools: %+v", body.Tools)
	}
}
