package control

import (
	"bytes"
	"context"
	"encoding/json"
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
	if first && f.bash {
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"sleep 5\\\"}\"}]}}\n\n")
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
		defer h.mu.Unlock()
		h.events = append(h.events, e)
	})
	t.Cleanup(h.c.Close)
	return h
}

func (h *harness) log() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.events...)
}

func (h *harness) await(what string, pred func([]Event) bool) []Event {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
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

func (h *harness) input(text string, images ...shell.Image) string {
	h.t.Helper()
	if err := h.c.Input(agent.Input{Text: text, Images: images}); err != nil {
		h.t.Fatalf("input %s: %v", text, err)
	}
	return h.c.Status().Turn
}

func (h *harness) steer(text string, images ...shell.Image) {
	h.t.Helper()
	if err := h.c.Steer(agent.Input{Text: text, Images: images}); err != nil {
		h.t.Fatalf("steer %s: %v", text, err)
	}
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
	turn := h.input("hello")
	if turn == "" {
		t.Fatal("missing turn id")
	}
	events := h.await("done", func(events []Event) bool { return doneFor(events, turn) != nil })
	if n := countKind(events, "done"); n != 1 {
		t.Fatalf("done events = %d", n)
	}
	if d := doneFor(events, turn); d.Status != "ok" || d.Error != "" {
		t.Fatalf("done = %+v", d)
	}
	if !h.a.HasSavedSession() {
		t.Fatal("completed turn was not saved")
	}
	if got := h.c.History(); len(got) == 0 || got[len(got)-1].Text != "answer" {
		t.Fatalf("unexpected history: %+v", got)
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
	turn := h.input("first")
	<-model.started
	h.steer("focus on errors")
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
	if err := h.c.Steer(agent.Input{Text: "late"}); codeOf(err) != "not_busy" {
		t.Fatalf("steer while idle = %v", err)
	}
	if err := h.c.Input(agent.Input{}); codeOf(err) != "invalid" {
		t.Fatalf("empty input = %v", err)
	}
}

func TestRecallReturnsUnacceptedInstructionOnly(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.input("first")
	<-model.started
	h.steer("first follow-up")
	h.steer("second follow-up")
	in, err := h.c.Recall()
	if err != nil || in == nil || in.Text != "second follow-up" {
		t.Fatalf("recall = %+v, %v", in, err)
	}
	if queued := h.c.Status().Queued; len(queued) != 1 || queued[0].Text != "first follow-up" {
		t.Fatalf("queued after recall = %+v", queued)
	}
	close(model.gate)
	h.await("done", func(events []Event) bool { return doneFor(events, turn) != nil })
	if _, err := h.c.Recall(); codeOf(err) != "not_recallable" {
		t.Fatalf("recall of accepted steering = %v", err)
	}
}

func TestStopCancelsStreamingTurnAndReturnsQueuedInputs(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.input("long")
	<-model.started
	h.steer("queued one")
	h.steer("queued two")
	h.c.Stop()
	if !h.c.Status().Cancelling {
		t.Fatal("controller not cancelling")
	}
	close(model.gate)
	events := h.await("cancelled", func(events []Event) bool { return doneFor(events, turn) != nil })
	d := doneFor(events, turn)
	if d.Status != "cancelled" || len(d.Unsent) != 2 || d.Unsent[0].Text != "queued one" || d.Unsent[1].Text != "queued two" {
		t.Fatalf("done = %+v", d)
	}
	if countKind(events, "started") != 1 {
		t.Fatal("queued inputs were executed despite cancellation")
	}
	if !hasNotice(events, "Turn cancelled.") {
		t.Fatalf("cancellation notice missing: %+v", events)
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
	turn := h.input("run it")
	h.await("command running", func(events []Event) bool {
		for _, e := range events {
			if e.Kind == "agent" && e.Agent.Kind == "code" && !e.Agent.Provisional {
				return true
			}
		}
		return false
	})
	h.c.Stop()
	events := h.await("stopped", func(events []Event) bool { return doneFor(events, turn) != nil })
	d := doneFor(events, turn)
	if d == nil || d.Status != "cancelled" {
		t.Fatalf("expected cancelled turn, got %+v", d)
	}
}

func TestStopWhenIdleIsNoop(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	h.c.Stop()
	if h.c.Status().Busy {
		t.Fatal("stop while idle marked busy")
	}
}

func TestBusyGuardsRejectMutations(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.input("busy")
	<-model.started

	if err := h.c.Compact(""); codeOf(err) != "busy" {
		t.Fatalf("compact while busy = %v", err)
	}
	if err := h.c.Rename("new title"); codeOf(err) != "busy" {
		t.Fatalf("rename while busy = %v", err)
	}
	if err := h.c.SelectProvider("test"); codeOf(err) != "busy" {
		t.Fatalf("select provider while busy = %v", err)
	}
	if err := h.c.SelectModel("test", "m", ""); codeOf(err) != "busy" {
		t.Fatalf("select model while busy = %v", err)
	}
	if err := h.c.AddProvider("name", "http://x", "k"); codeOf(err) != "busy" {
		t.Fatalf("add provider while busy = %v", err)
	}
	if _, err := h.c.Providers(); codeOf(err) != "busy" {
		t.Fatalf("providers while busy = %v", err)
	}
	if st := h.c.Status(); st.Turn != turn || !st.Busy {
		t.Fatalf("status = %+v", st)
	}
	if err := h.c.Resume("missing"); codeOf(err) != "not_found" {
		t.Fatalf("resume missing: %v", err)
	}
	close(model.gate)
	h.await("done", func(events []Event) bool { return doneFor(events, turn) != nil })
}

func TestCancelThenNewSwitchesSessionAfterTurnEnds(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.input("old")
	<-model.started
	first := h.a.HistoryPath
	if err := h.c.NewSession(); err != nil {
		t.Fatalf("new error: %v", err)
	}
	if !h.c.Status().Cancelling {
		t.Fatal("turn not cancelling after deferred new")
	}
	close(model.gate)
	h.await("switched", func(events []Event) bool { return countKind(events, "session") == 1 })
	if h.a.HistoryPath == first {
		t.Fatal("session did not change")
	}
	d := doneFor(h.log(), turn)
	if d == nil || d.Status != "cancelled" {
		t.Fatalf("cancelled turn done = %+v", d)
	}
	var sawSession bool
	for _, e := range h.log() {
		if e.Kind == "session" {
			sawSession = true
			if len(e.Records) != 0 {
				t.Fatalf("new session records = %+v", e.Records)
			}
		}
	}
	if !sawSession {
		t.Fatal("session event never emitted")
	}
	if h.c.Status().Busy || h.c.Status().Cancelling {
		t.Fatalf("leftover busy state: %+v", h.c.Status())
	}
}

func TestQuitWhileBusyClosesAfterSaving(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	_ = h.input("work")
	<-model.started
	closed, err := h.c.Quit()
	if err != nil || closed {
		t.Fatalf("quit while busy closed immediately: %v, %v", closed, err)
	}
	close(model.gate)
	select {
	case <-h.c.Closed():
	case <-time.After(5 * time.Second):
		t.Fatal("quit did not close")
	}
	if h.c.Reason() != "quit" {
		t.Fatalf("reason = %q", h.c.Reason())
	}
	if !h.a.HasSavedSession() {
		t.Fatal("session was not saved before quit closed")
	}
	if err := h.c.Input(agent.Input{Text: "after"}); codeOf(err) != "closed" {
		t.Fatalf("operation after quit = %v", err)
	}
}

func TestIdleQuitSavesAndClosesImmediately(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	h.input("once")
	h.await("done", func(events []Event) bool { return countKind(events, "done") == 1 })
	closed, err := h.c.Quit()
	if err != nil || !closed {
		t.Fatalf("idle quit: %v, %v", closed, err)
	}
	select {
	case <-h.c.Closed():
	default:
		t.Fatal("Closed channel not closed")
	}
}

func TestSaveFailureKeepsControllerOpen(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	h.input("keep me")
	h.await("done", func(events []Event) bool { return countKind(events, "done") == 1 })
	_ = os.Chmod(filepath.Dir(h.a.HistoryPath), 0500)
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(h.a.HistoryPath), 0700) })
	closed, err := h.c.Quit()
	if err == nil || closed {
		t.Fatalf("save failure should fail quit: closed=%v err=%v", closed, err)
	}
	select {
	case <-h.c.Closed():
		t.Fatal("Closed channel closed despite failed save")
	default:
	}
	if h.c.Reason() != "" {
		t.Fatalf("reason set after failed save: %q", h.c.Reason())
	}
}

func TestCompactRunsInBackgroundAndCanBeCancelled(t *testing.T) {
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary text","annotations":[]}]}]}`)
	}))
	defer server.Close()
	h := newHarness(t, &fakeModel{}, "")
	h.a.ApplySettings(agent.Settings{Provider: "p", Model: "m", Providers: map[string]agent.Provider{"p": {BaseURL: server.URL}}})
	h.a.History = []json.RawMessage{
		[]byte(`{"role":"user","content":"first"}`),
		[]byte(`{"role":"assistant","content":"reply"}`),
	}
	if err := h.c.Compact("keep decisions"); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !h.c.Status().Compacting || !h.c.Status().Busy {
		t.Fatalf("status during compact = %+v", h.c.Status())
	}
	if err := h.c.Input(agent.Input{Text: "during"}); codeOf(err) != "compacting" {
		t.Fatalf("input during compact = %v", err)
	}
	h.c.Stop()
	close(gate)
	events := h.await("compact cancelled", func(events []Event) bool {
		for _, e := range events {
			if e.Kind == "done" && e.Operation == "compact" {
				return true
			}
		}
		return false
	})
	d := doneFor(events, events[len(events)-1].Turn)
	if d.Status != "cancelled" {
		t.Fatalf("compact done = %+v", d)
	}
}

func TestCompactReplacesHistoryWithSummary(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	h.a.History = []json.RawMessage{
		[]byte(`{"role":"user","content":"one"}`),
		[]byte(`{"role":"assistant","content":"done one"}`),
		[]byte(`{"role":"user","content":"two"}`),
		[]byte(`{"role":"assistant","content":"done two"}`),
		[]byte(`{"role":"user","content":"three"}`),
		[]byte(`{"role":"assistant","content":"done three"}`),
		[]byte(`{"role":"user","content":"four"}`),
		[]byte(`{"role":"assistant","content":"done four"}`),
		[]byte(`{"role":"user","content":"five"}`),
		[]byte(`{"role":"assistant","content":"done five"}`),
	}
	if err := h.c.Compact(""); err != nil {
		t.Fatalf("compact: %v", err)
	}
	h.await("compact done", func(events []Event) bool {
		for _, e := range events {
			if e.Kind == "done" && e.Operation == "compact" && e.Status == "ok" {
				return true
			}
		}
		return false
	})
}

func TestPendingImagesBlockSessionSwitch(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	h.input("busy")
	<-model.started
	image := shell.Image{URL: "data:image/png;base64,AA=="}
	if err := h.c.Steer(agent.Input{Images: []shell.Image{image}}); err != nil {
		t.Fatalf("steer with image: %v", err)
	}
	if err := h.c.NewSession(); codeOf(err) != "pending_images" {
		t.Fatalf("new with queued image: %v", err)
	}
	close(model.gate)
}

func TestSetupMissingBlocksInputWithHint(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := New(a, nil)
	if err := c.Input(agent.Input{Text: "hi"}); codeOf(err) != "setup_required" || !strings.Contains(err.Error(), "No provider configured") {
		t.Fatalf("input without provider: %v", err)
	}
}

func TestProvidersNeverExposeCredentials(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	if err := h.c.AddProvider("secret", "https://user:pw@example.test/v1?token=abc", "sk-test-123"); err != nil {
		t.Fatal(err)
	}
	list, err := h.c.Providers()
	if err != nil {
		t.Fatal(err)
	}
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
	if err := h.c.AddProvider("secret", "https://x", "sk-other"); codeOf(err) != "failed" || strings.Contains(err.Error(), "sk-other") {
		t.Fatalf("duplicate provider error = %v", err)
	}
	if err := h.c.SelectModel("nope", "m", ""); codeOf(err) != "not_found" {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestCommandsListAndHelp(t *testing.T) {
	cmds := Commands()
	if len(cmds) != 8 {
		t.Fatalf("commands count = %d", len(cmds))
	}
	help := Help("keyboard shortcuts")
	if !strings.Contains(help, "/new") || !strings.Contains(help, "/settings") || !strings.Contains(help, "keyboard shortcuts") {
		t.Fatalf("help text = %q", help)
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
	turn := h.input("fields")
	events := h.await("done", func(events []Event) bool { return doneFor(events, turn) != nil })
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
