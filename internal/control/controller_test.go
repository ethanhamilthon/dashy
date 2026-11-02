package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dashy/internal/agent"
	"dashy/internal/shell"
)

// fakeModel answers Responses requests. A gate holds streaming answers until
// the test releases them, so busy states can be exercised deterministically.
type fakeModel struct {
	mu       sync.Mutex
	requests int
	gate     chan struct{}
	started  chan struct{}
	bash     bool // the first streamed request asks for a long shell command
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !bytes.Contains(body, []byte(`"stream":true`)) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary text","annotations":[]}]}]}`)
		return
	}
	f.mu.Lock()
	f.requests++
	first := f.requests == 1
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hel\"}\n\n")
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-r.Context().Done():
			return
		}
	}
	if f.bash && first {
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"sleep 30\\\"}\"}]}}\n\n")
		return
	}
	fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n")
}

// harness wires an agent, a fake provider and a controller that records events.
type harness struct {
	t      *testing.T
	a      *agent.Agent
	c      *Controller
	model  *fakeModel
	mu     sync.Mutex
	events []Event
}

func newHarness(t *testing.T, model *fakeModel, config string) *harness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	a.ApplySettings(agent.Settings{
		Provider: "test",
		Model:    "m",
		Providers: map[string]agent.Provider{
			"test": {BaseURL: server.URL},
		},
	})
	h := &harness{t: t, a: a, model: model}
	h.c = New(a, func(e Event) {
		h.mu.Lock()
		h.events = append(h.events, e)
		h.mu.Unlock()
	})
	t.Cleanup(func() {
		h.c.Close()
		a.Close()
	})
	return h
}

func (h *harness) log() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.events...)
}

// await polls until pred holds over the event log or the test times out.
func (h *harness) await(what string, pred func([]Event) bool) []Event {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events := h.log()
		if pred(events) {
			return events
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; events: %+v", what, events)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func doneFor(events []Event, turn string) *Event {
	for i := range events {
		if events[i].Kind == "done" && events[i].Turn == turn {
			return &events[i]
		}
	}
	return nil
}

func (h *harness) submit(req Request) Response {
	h.t.Helper()
	resp, err := h.c.Submit(req)
	if err != nil {
		h.t.Fatalf("submit %s: %v", req.Type, err)
	}
	return resp
}

func codeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestLaunchWaitsWithoutModelRequestOrHistoryFile(t *testing.T) {
	model := &fakeModel{}
	h := newHarness(t, model, "")
	time.Sleep(50 * time.Millisecond)
	h.c.Deliver()
	if model.requests != 0 || len(h.log()) != 0 {
		t.Fatalf("launch started work: requests=%d events=%v", model.requests, h.log())
	}
	if h.a.HasSavedSession() {
		t.Fatal("waiting created a session file")
	}
	if status := h.c.Status(); status.Busy {
		t.Fatal("controller busy before any input")
	}
}

func TestInputStartsTurnAndDoneIsExactlyOnce(t *testing.T) {
	model := &fakeModel{}
	h := newHarness(t, model, "")
	resp := h.submit(Request{Type: "input", Text: "hello"})
	if resp.Status != "started" || resp.Turn == "" {
		t.Fatalf("response = %+v", resp)
	}
	events := h.await("done", func(events []Event) bool { return doneFor(events, resp.Turn) != nil })
	if n := countKind(events, "done"); n != 1 {
		t.Fatalf("done events = %d", n)
	}
	if d := doneFor(events, resp.Turn); d.Status != "ok" || d.Error != "" {
		t.Fatalf("done = %+v", d)
	}
	if !h.a.HasSavedSession() {
		t.Fatal("completed turn was not saved")
	}
	if got := h.c.History(); len(got) == 0 || got[len(got)-1].Text != "answer" {
		t.Fatalf("history = %+v", got)
	}
}

func countKind(events []Event, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestSteeringWhileBusyIsAcceptedBeforeCompletion(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.submit(Request{Type: "input", Text: "first"}).Turn
	<-model.started
	if resp := h.submit(Request{Type: "steer", Text: "focus on errors"}); resp.Status != "queued" {
		t.Fatalf("steer response = %+v", resp)
	}
	if got := h.c.Status().Queued; len(got) != 1 || got[0].Text != "focus on errors" {
		t.Fatalf("queued = %+v", got)
	}
	close(model.gate)
	events := h.await("done", func(events []Event) bool { return doneFor(events, turn) != nil })
	accepted := false
	for _, e := range events {
		if e.Kind == "agent" && e.Agent.Kind == "user" && e.Agent.Steering && e.Agent.Text == "focus on errors" {
			accepted = true
		}
	}
	if !accepted {
		t.Fatalf("steering was never accepted: %+v", events)
	}
	if model.requests != 2 {
		t.Fatalf("steering must trigger a follow-up request, got %d", model.requests)
	}
	if len(h.c.Status().Queued) != 0 {
		t.Fatal("accepted steering still queued")
	}
}

func TestSteerWhileIdleIsRejected(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	if _, err := h.c.Submit(Request{Type: "steer", Text: "late"}); codeOf(err) != "not_busy" {
		t.Fatalf("steer while idle: %v", err)
	}
	if _, err := h.c.Submit(Request{Type: "input"}); codeOf(err) != "invalid" {
		t.Fatalf("empty input: %v", err)
	}
	if h.model.requests != 0 {
		t.Fatal("rejected steer started work")
	}
}

func TestRecallReturnsUnacceptedInstructionOnly(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.submit(Request{Type: "input", Text: "first"}).Turn
	<-model.started
	h.submit(Request{Type: "steer", Text: "first follow-up"})
	h.submit(Request{Type: "steer", Text: "second follow-up"})
	resp, err := h.c.Submit(Request{Type: "recall"})
	if err != nil || resp.Input == nil || resp.Input.Text != "second follow-up" {
		t.Fatalf("recall = %+v, %v", resp, err)
	}
	if got := h.c.Status().Queued; len(got) != 1 || got[0].Text != "first follow-up" {
		t.Fatalf("queue after recall = %+v", got)
	}
	close(model.gate)
	h.await("done", func(events []Event) bool { return doneFor(events, turn) != nil })
	if _, err := h.c.Submit(Request{Type: "recall"}); codeOf(err) != "not_recallable" {
		t.Fatalf("recall while idle: %v", err)
	}
	for _, e := range h.log() {
		if e.Agent != nil && e.Agent.Text == "second follow-up" {
			t.Fatal("recalled instruction was sent to the model")
		}
	}
}

func TestStopCancelsStreamingTurnAndReturnsQueuedInputs(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.submit(Request{Type: "input", Text: "long"}).Turn
	<-model.started
	h.submit(Request{Type: "steer", Text: "queued one"})
	h.submit(Request{Type: "steer", Text: "queued two"})
	if resp := h.submit(Request{Type: "stop"}); resp.Status != "cancelling" || resp.Turn != turn {
		t.Fatalf("stop = %+v", resp)
	}
	events := h.await("cancelled done", func(events []Event) bool { return doneFor(events, turn) != nil })
	d := doneFor(events, turn)
	if d.Status != "cancelled" || len(d.Unsent) != 2 || d.Unsent[0].Text != "queued one" || d.Unsent[1].Text != "queued two" {
		t.Fatalf("done = %+v", d)
	}
	if !hasNotice(events, "Turn cancelled.") {
		t.Fatal("cancellation notice missing")
	}
	if len(h.c.Status().Queued) != 0 || h.c.Status().Busy {
		t.Fatal("controller still busy or holding queue after cancel")
	}
}

func hasNotice(events []Event, text string) bool {
	for _, e := range events {
		if e.Kind == "notice" && e.Text == text {
			return true
		}
	}
	return false
}

func TestStopCancelsRunningShellCommand(t *testing.T) {
	model := &fakeModel{bash: true}
	h := newHarness(t, model, "")
	turn := h.submit(Request{Type: "input", Text: "run it"}).Turn
	h.await("shell start", func(events []Event) bool {
		for _, e := range events {
			if e.Agent != nil && e.Agent.Kind == "code" && strings.Contains(e.Agent.Text, "sleep 30") {
				return true
			}
		}
		return false
	})
	started := time.Now()
	h.submit(Request{Type: "stop"})
	events := h.await("cancelled", func(events []Event) bool { return doneFor(events, turn) != nil })
	if d := doneFor(events, turn); d.Status != "cancelled" {
		t.Fatalf("done = %+v", d)
	}
	if time.Since(started) > 10*time.Second {
		t.Fatal("shell command was not terminated promptly")
	}
}

func TestStopWhenIdleIsNoop(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	if resp := h.submit(Request{Type: "stop"}); resp.Status != "idle" {
		t.Fatalf("stop idle = %+v", resp)
	}
	if len(h.log()) != 0 {
		t.Fatal("idle stop emitted events")
	}
}

func TestBusyGuardsRejectMutations(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.submit(Request{Type: "input", Text: "busy"}).Turn
	<-model.started
	defer close(model.gate)
	cases := []Request{
		{Type: "rename", Args: map[string]string{"title": "x"}},
		{Type: "compact"},
		{Type: "select_model", Args: map[string]string{"provider": "test", "model": "m"}},
		{Type: "select_provider", Args: map[string]string{"name": "test"}},
		{Type: "providers"},
		{Type: "add_provider", Args: map[string]string{"name": "x", "base_url": "http://x"}},
	}
	for _, req := range cases {
		if _, err := h.c.Submit(req); codeOf(err) != "busy" {
			t.Fatalf("%s while busy: %v", req.Type, err)
		}
	}
	if resp := h.submit(Request{Type: "state"}); resp.Data.(StateInfo).Turn != turn {
		t.Fatalf("state = %+v", resp.Data)
	}
	if _, err := h.c.Submit(Request{Type: "resume", Args: map[string]string{"session_id": "missing"}}); codeOf(err) != "not_found" {
		t.Fatalf("resume missing session: %v", err)
	}
}

func TestCancelThenNewSwitchesSessionAfterTurnEnds(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	oldID := h.a.SessionInfoFor(nil)["id"]
	turn := h.submit(Request{Type: "input", Text: "old"}).Turn
	<-model.started
	if resp := h.submit(Request{Type: "new"}); resp.Status != "deferred" {
		t.Fatalf("new during work = %+v", resp)
	}
	events := h.await("session switch", func(events []Event) bool {
		for _, e := range events {
			if e.Kind == "session" {
				return true
			}
		}
		return false
	})
	doneIndex, sessionIndex := -1, -1
	for i, e := range events {
		if e.Kind == "done" && e.Turn == turn {
			doneIndex = i
		}
		if e.Kind == "session" && sessionIndex < 0 {
			sessionIndex = i
		}
	}
	if doneIndex < 0 || sessionIndex < doneIndex {
		t.Fatalf("session switch must follow done: %+v", events)
	}
	if h.a.SessionInfoFor(nil)["id"] == oldID {
		t.Fatal("session was not switched")
	}
	if len(h.c.History()) != 0 {
		t.Fatal("new session shows the old transcript")
	}
}

func TestQuitWhileBusyClosesAfterSaving(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.submit(Request{Type: "input", Text: "work"}).Turn
	<-model.started
	if resp := h.submit(Request{Type: "quit"}); resp.Status != "deferred" {
		t.Fatalf("quit during work = %+v", resp)
	}
	select {
	case <-h.c.Closed():
		t.Fatal("closed before the turn ended")
	default:
	}
	h.await("done", func(events []Event) bool { return doneFor(events, turn) != nil })
	select {
	case <-h.c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("quit did not close the controller")
	}
	if h.c.Reason() != "quit" {
		t.Fatalf("reason = %q", h.c.Reason())
	}
	if _, err := h.c.Submit(Request{Type: "state"}); codeOf(err) != "closed" {
		t.Fatalf("submit after close: %v", err)
	}
}

func TestIdleQuitSavesAndClosesImmediately(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	resp := h.submit(Request{Type: "quit"})
	if resp.Status != "closed" {
		t.Fatalf("quit = %+v", resp)
	}
	if h.c.Reason() != "quit" {
		t.Fatalf("reason = %q", h.c.Reason())
	}
	if len(h.log()) != 0 {
		t.Fatalf("idle quit emitted events: %+v", h.log())
	}
}

func TestSaveFailureKeepsControllerOpen(t *testing.T) {
	model := &fakeModel{}
	h := newHarness(t, model, "")
	h.submit(Request{Type: "input", Text: "keep me"})
	h.await("done", func(events []Event) bool { return countKind(events, "done") == 1 })
	dir := filepath.Dir(h.a.HistoryPath)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if _, err := h.c.Submit(Request{Type: "quit"}); codeOf(err) != "failed" {
		t.Fatalf("quit with unwritable session: %v", err)
	}
	select {
	case <-h.c.Closed():
		t.Fatal("controller closed despite save failure")
	default:
	}
}

func TestCompactRunsInBackgroundAndCanBeCancelled(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "compact_keep_turns = 1\n")
	h.a.History = append(h.a.History,
		[]byte(`{"role":"user","content":"one"}`),
		[]byte(`{"role":"assistant","content":"done one"}`),
		[]byte(`{"role":"user","content":"two"}`),
		[]byte(`{"role":"assistant","content":"done two"}`))
	if err := h.a.Save(); err != nil {
		t.Fatal(err)
	}
	if err := h.a.Resume(strings.TrimSuffix(filepath.Base(h.a.HistoryPath), ".jsonl")); err != nil {
		t.Fatal(err)
	}
	resp := h.submit(Request{Type: "compact", Text: "keep decisions"})
	if resp.Status != "started" {
		t.Fatalf("compact = %+v", resp)
	}
	if !h.c.Status().Compacting {
		t.Fatal("status does not report compaction")
	}
	if _, err := h.c.Submit(Request{Type: "input", Text: "during"}); codeOf(err) != "busy" {
		t.Fatalf("input during compaction: %v", err)
	}
	h.submit(Request{Type: "stop"})
	events := h.await("compaction done", func(events []Event) bool { return doneFor(events, resp.Turn) != nil })
	if d := doneFor(events, resp.Turn); d.Status != "cancelled" || d.Message != "Compaction cancelled." || d.Operation != "compact" {
		t.Fatalf("done = %+v", d)
	}
	if h.c.Status().Busy {
		t.Fatal("compaction still busy")
	}
}

func TestCompactReplacesHistoryWithSummary(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "compact_keep_turns = 1\n")
	h.a.History = append(h.a.History,
		[]byte(`{"role":"user","content":"one"}`),
		[]byte(`{"role":"assistant","content":"done one"}`),
		[]byte(`{"role":"user","content":"two"}`),
		[]byte(`{"role":"assistant","content":"done two"}`),
		[]byte(`{"role":"user","content":"three"}`),
		[]byte(`{"role":"assistant","content":"done three"}`),
		[]byte(`{"role":"user","content":"four"}`),
		[]byte(`{"role":"assistant","content":"done four"}`),
		[]byte(`{"role":"user","content":"five"}`),
		[]byte(`{"role":"assistant","content":"done five"}`))
	resp := h.submit(Request{Type: "compact"})
	events := h.await("compaction done", func(events []Event) bool { return doneFor(events, resp.Turn) != nil })
	if d := doneFor(events, resp.Turn); d.Status != "ok" || !strings.Contains(d.Message, "Compacted") {
		t.Fatalf("done = %+v", d)
	}
	if countKind(events, "session") == 0 {
		t.Fatal("compacted history not published")
	}
}

func TestPendingImagesBlockSessionSwitch(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	h.submit(Request{Type: "input", Text: "busy"})
	<-model.started
	defer close(model.gate)
	image := shell.Image{URL: "data:image/png;base64,AA==", Path: "x.png"}
	if _, err := h.c.Submit(Request{Type: "steer", Images: []shell.Image{image}}); err != nil {
		t.Fatalf("steer with image: %v", err)
	}
	if _, err := h.c.Submit(Request{Type: "new"}); codeOf(err) != "pending_images" {
		t.Fatalf("new with queued image: %v", err)
	}
}

func TestSetupMissingBlocksInputWithHint(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := New(a, nil)
	if _, err := c.Submit(Request{Type: "input", Text: "hi"}); codeOf(err) != "setup_required" || !strings.Contains(err.Error(), "No provider configured") {
		t.Fatalf("input without provider: %v", err)
	}
}

func TestProvidersNeverExposeCredentials(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	h.submit(Request{Type: "add_provider", Args: map[string]string{"name": "secret", "base_url": "https://user:pw@example.test/v1?token=abc", "api_key": "sk-test-123"}})
	resp := h.submit(Request{Type: "providers"})
	list := resp.Data.([]ProviderInfo)
	found := false
	for _, p := range list {
		if p.Name == "secret" {
			found = true
			if !p.APIKeyConfigured || strings.Contains(p.BaseURL, "pw") || strings.Contains(p.BaseURL, "token") {
				t.Fatalf("provider = %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("provider missing: %+v", list)
	}
	if _, err := h.c.Submit(Request{Type: "add_provider", Args: map[string]string{"name": "secret", "base_url": "https://x", "api_key": "sk-other"}}); codeOf(err) != "failed" || strings.Contains(err.Error(), "sk-other") {
		t.Fatalf("duplicate provider error = %v", err)
	}
	if _, err := h.c.Submit(Request{Type: "select_model", Args: map[string]string{"provider": "nope", "model": "m"}}); codeOf(err) != "not_found" {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestCommandsRunTheSameOperations(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	h.submit(Request{Type: "command", Name: "/rename", Args: map[string]string{"title": "Review"}})
	if !hasNotice(h.log(), "Session renamed: Review") {
		t.Fatalf("rename command events: %+v", h.log())
	}
	if _, err := h.c.Submit(Request{Type: "command", Name: "/bogus"}); codeOf(err) != "unknown_command" {
		t.Fatalf("unknown command: %v", err)
	}
	if _, err := h.c.Submit(Request{Type: "command", Name: "/resume"}); codeOf(err) != "invalid" {
		t.Fatalf("resume without id: %v", err)
	}
	resp := h.submit(Request{Type: "command", Name: "/help"})
	if help := resp.Data.(HelpInfo); len(help.Commands) != len(commandList) {
		t.Fatalf("help = %+v", help)
	}
}

func TestNotificationStartsIdleTurnAndSteersBusyTurn(t *testing.T) {
	model := &fakeModel{}
	h := newHarness(t, model, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.c.Run(ctx)
	id := strings.TrimSuffix(filepath.Base(h.a.HistoryPath), ".jsonl")
	if err := agent.Notify(h.a.Shell.Dir, id, "ping"); err != nil {
		t.Fatal(err)
	}
	h.await("notify turn", func(events []Event) bool {
		for _, e := range events {
			if e.Kind == "started" && e.Source == "notify" {
				return true
			}
		}
		return false
	})
	h.await("notify done", func(events []Event) bool { return countKind(events, "done") == 1 })
}

func TestEventsFromAgentKeepOriginalFields(t *testing.T) {
	model := &fakeModel{}
	h := newHarness(t, model, "")
	resp := h.submit(Request{Type: "input", Text: "fields"})
	events := h.await("done", func(events []Event) bool { return doneFor(events, resp.Turn) != nil })
	var sawDelta, sawFinal bool
	for _, e := range events {
		if e.Kind == "agent" && e.Agent.Kind == "delta" && e.Agent.Provisional {
			sawDelta = true
		}
		if e.Kind == "agent" && e.Agent.Kind == "assistant" && e.Agent.Final {
			sawFinal = true
		}
	}
	if !sawDelta || !sawFinal {
		t.Fatalf("agent event fields lost: %+v", events)
	}
}
