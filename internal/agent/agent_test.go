package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeProvider struct {
	mu     sync.Mutex
	bodies []string
	reply  func(n int, body string) string
	status func(n int) int
	hold   bool
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
	if f.hold {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

func (f *fakeProvider) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "data: %s\n\n", e)
	}
	return b.String()
}

func textReply(text string) string {
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]}}`, text))
}

func bashReply(callID, command string) string {
	args, _ := json.Marshal(map[string]any{"command": command, "timeout": nil})
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":%q,"name":"bash","arguments":%q}]}}`, callID, args))
}

func newTestAgent(t *testing.T, fp *fakeProvider, opts Options) *Agent {
	return newTestAgentWith(t, fp, opts, nil)
}

func newTestAgentWith(t *testing.T, fp *fakeProvider, opts Options, pre func(home, dir string)) *Agent {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)
	s := DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = Provider{BaseURL: server.URL}
	if err := SaveSettings(home, s); err != nil {
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
	a.cfg.sel = selection{}
	if err := a.update(func(s *Settings) error { *s = DefaultSettings(); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := turn(t, a, "hi"); err == nil || !strings.Contains(err.Error(), "No provider configured") {
		t.Fatalf("err = %v", err)
	}
	if len(fp.requests()) != 0 {
		t.Fatal("a request was sent without a model")
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

func TestCompletedResponseWithoutStreamClose(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{hold: true, reply: func(int, string) string { return textReply("hi") }}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Run(ctx, "hello", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if got := a.FinalText(); got != "hi" {
		t.Fatalf("final %q", got)
	}
}

func TestModelsStopOnRepeatedPage(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return `{"data":[{"id":"a"},{"id":"b"}],"has_more":true}`
	}}
	a := newTestAgent(t, fp, Options{})
	choices, errs := AllModels(context.Background(), a.Settings().Providers)
	if len(errs) != 0 || len(choices) != 2 || len(fp.requests()) != 2 {
		t.Fatalf("choices %v errs %v requests %d", choices, errs, len(fp.requests()))
	}
}
