package control

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"dashy/internal/agent"
	"dashy/internal/shell"
)

// fakeModel answers provider requests: streamed ones with an answer, a summary
// request (not streamed) with SUMMARY. gate, when set, holds every request until
// it is closed or the request is cancelled.
type fakeModel struct {
	mu     sync.Mutex
	bodies []string
	gate   chan struct{}
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	body := string(data)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	n := len(f.bodies)
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if !strings.Contains(body, `"stream":true`) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"SUMMARY","annotations":[]}]}]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer %d\"}]}]}}\n\n", n)
}

func (f *fakeModel) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// harness is a controller over a real agent whose events are recorded in order.
type harness struct {
	t      *testing.T
	ctl    *Controller
	a      *agent.Agent
	model  *fakeModel
	dir    string
	mu     sync.Mutex
	events []Event
}

func newHarness(t *testing.T, model *fakeModel) *harness {
	t.Helper()
	return newHarnessWith(t, model, nil)
}

// newHarnessWith is newHarness with pre run on the agent before the controller exists.
func newHarnessWith(t *testing.T, model *fakeModel, pre func(*agent.Agent)) *harness {
	t.Helper()
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)
	s := agent.DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = agent.Provider{BaseURL: server.URL}
	if err := agent.SaveSettings(home, s); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(dir, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if pre != nil {
		pre(a)
	}
	h := &harness{t: t, a: a, model: model, dir: dir}
	h.ctl = New(a, func(e Event) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, e)
	})
	t.Cleanup(func() {
		h.ctl.Close()
		a.Close()
	})
	return h
}

func (h *harness) log() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.events...)
}

// await polls the event log until pred holds.
func (h *harness) await(what string, pred func([]Event) bool) []Event {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events := h.log()
		if pred(events) {
			return events
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; events: %s", what, describe(events))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func describe(events []Event) string {
	var parts []string
	for _, e := range events {
		parts = append(parts, e.Kind+"/"+e.Status+"/"+e.Text)
	}
	return strings.Join(parts, " | ")
}

func doneEvents(events []Event) []Event {
	var done []Event
	for _, e := range events {
		if e.Kind == "done" {
			done = append(done, e)
		}
	}
	return done
}

// settle waits until the controller is idle again.
func (h *harness) settle() []Event {
	h.t.Helper()
	return h.await("idle", func(events []Event) bool {
		return !h.ctl.Status().Busy && len(doneEvents(events)) > 0
	})
}

func (h *harness) send(text string, images ...shell.Image) {
	h.t.Helper()
	if err := h.ctl.Input(agent.Input{Text: text, Images: images}); err != nil {
		h.t.Fatalf("input %q: %v", text, err)
	}
}

func codeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

func TestTurnEmitsStartedThenExactlyOneDone(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	h.send("hello")
	events := h.settle()
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "started,agent,agent,done" {
		t.Fatalf("kinds = %s", got)
	}
	if done := doneEvents(events); len(done) != 1 || done[0].Status != "ok" || done[0].Text != "answer 1" {
		t.Fatalf("done = %+v", done)
	}
}

// inFlight waits until the model has received n requests, so later input is queued
// behind a request that is really in flight (not taken at a step boundary).
func (h *harness) inFlight(n int) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.model.requests()) < n {
		if time.Now().After(deadline) {
			h.t.Fatalf("request %d never arrived", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Input while a turn runs is steering: the model receives it before the turn ends.
func TestSteeringJoinsRunningTurn(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	h.send("steer me")
	close(gate)
	h.settle()
	reqs := h.model.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1], "steer me") {
		t.Fatalf("steering not sent to the model: %d requests", len(reqs))
	}
}

// A task result queued during a busy turn precedes composer steering sent later.
func TestNotificationKeepsOrderWithSteering(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	if _, err := agent.RunTask(h.dir, h.a.SessionID(), "echo notice A"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); h.a.RunningTasks() > 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("task result did not arrive")
		}
	}
	h.send("steer B")
	close(gate)
	h.settle()
	reqs := h.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	a, b := strings.Index(reqs[1], "notice A"), strings.Index(reqs[1], "steer B")
	if a < 0 || b < a {
		t.Fatalf("notification and steering out of order: A=%d B=%d", a, b)
	}
}

// Stop cancels the turn, and unaccepted steering comes back as unsent input.
func TestStopReturnsUnacceptedSteering(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	h.send("queued text")
	if got := h.ctl.Status().Queued; len(got) != 1 {
		t.Fatalf("queued = %v", got)
	}
	h.ctl.Stop()
	done := doneEvents(h.settle())
	if len(done) != 1 || done[0].Status != "cancelled" || len(done[0].Unsent) != 1 || done[0].Unsent[0].Text != "queued text" {
		t.Fatalf("done = %+v", done)
	}
}

func TestRecallReturnsNewestInstructionOnce(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	h.send("one")
	h.send("two")
	if in, err := h.ctl.Recall(); err != nil || in.Text != "two" {
		t.Fatalf("recall = %v, %v", in, err)
	}
	if in, err := h.ctl.Recall(); err != nil || in.Text != "one" {
		t.Fatalf("second recall = %v, %v", in, err)
	}
	if _, err := h.ctl.Recall(); codeOf(err) != "not_recallable" {
		t.Fatalf("empty recall: %v", err)
	}
}

// Settings changed while a turn runs are accepted and saved; the turn keeps running.
// The turn is started by a task result, as it can be while the picker is open.
func TestSettingsChangeWhileNotificationTurnRuns(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, &fakeModel{gate: gate})
	if _, err := agent.RunTask(h.dir, h.a.SessionID(), "echo background result"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !h.ctl.Status().Busy; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("notification did not start a turn")
		}
	}
	for name, err := range map[string]error{
		"reasoning": h.ctl.ChangeSetting(SettingReasoning, 1),
		"lines":     h.ctl.ChangeSetting(SettingLines, 1),
		"model":     h.ctl.SelectModel("p", "other", "high"),
	} {
		if err != nil {
			t.Errorf("%s while busy: %v", name, err)
		}
	}
	after := h.ctl.Settings()
	if after.ReasoningSummary || after.ShowLines != "full" || after.Model != "other" || after.Effort != "high" {
		t.Fatalf("settings not applied while busy: %+v", after)
	}
	if !h.ctl.Status().Busy {
		t.Fatal("the running turn was interrupted by a settings change")
	}
}

// Titles: the first line of the first message, a rename, and the reset back to the
// first line are all carried by the events the terminal reports.
func TestTitleFollowsFirstLineRenameAndReset(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	h.send("  first line\nsecond line")
	h.settle()
	if got := h.ctl.Title(); got != "first line" {
		t.Fatalf("title = %q", got)
	}
	if err := h.ctl.Rename("Named"); err != nil {
		t.Fatal(err)
	}
	if last := h.log()[len(h.log())-1]; last.Title != "Named" {
		t.Fatalf("rename event title = %q", last.Title)
	}
	if err := h.ctl.Rename(""); err != nil {
		t.Fatal(err)
	}
	if last := h.log()[len(h.log())-1]; last.Title != "first line" {
		t.Fatalf("reset event title = %q", last.Title)
	}
}

// A resumed session has its custom title and its first line, without any event
// having happened yet.
func TestResumedSessionHasItsTitle(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	h.send("resumed question\nmore")
	h.settle()
	h.ctl.Rename("Kept")
	id := h.a.SessionID()
	if err := h.ctl.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.ctl.Resume(id); err != nil {
		t.Fatal(err)
	}
	if got := h.ctl.Title(); got != "Kept" {
		t.Fatalf("resumed title = %q", got)
	}
	if sessions, _ := h.ctl.Sessions(); len(sessions) != 1 || sessions[0].Title != "Kept" {
		t.Fatalf("resume list: %+v", sessions)
	}
}

// Settings changes go through the controller: each is applied, validated and rejected
// with a stable code when it cannot apply.
func TestSettingsAndProvidersThroughTheController(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	if _, _, err := h.ctl.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	if codeOf(h.ctl.SelectProvider("nope")) != "not_found" || codeOf(h.ctl.SelectModel("", "", "")) != "invalid" {
		t.Fatal("unknown provider or empty model accepted")
	}
	if err := h.ctl.SelectModel("p", "m2", "high"); err != nil {
		t.Fatal(err)
	}
	if err := h.ctl.AddProvider("q", "https://q.example/v1", "sk-x"); err != nil {
		t.Fatal(err)
	}
	if codeOf(h.ctl.AddProvider("q", "https://q.example/v1", "")) != "failed" {
		t.Fatal("duplicate provider accepted")
	}
	for _, setting := range []string{SettingReasoning, SettingLines, SettingShellOutput} {
		if err := h.ctl.ChangeSetting(setting, 1); err != nil {
			t.Fatal(err)
		}
	}
	s := h.ctl.Settings()
	if s.Model != "m2" || s.Effort != "high" || s.ShowLines != "full" || s.ReasoningSummary || s.ShellOutput != 16384 || s.Providers["q"].APIKey != "sk-x" {
		t.Fatalf("settings: %+v", s)
	}
}

// Rename is refused while a turn runs; quitting during a turn waits
// for the cancellation and then closes the session.
func TestBusyRejectionsAndQuitDuringTurn(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	model := &fakeModel{}
	h := newHarness(t, model)
	model.mu.Lock()
	model.gate = gate
	model.mu.Unlock()
	h.send("slow")
	h.await("request", func([]Event) bool { return len(model.requests()) > 0 })
	if codeOf(h.ctl.Rename("x")) != "busy" {
		t.Fatal("rename accepted during a turn")
	}
	if closed, err := h.ctl.Quit(); closed || err != nil {
		t.Fatalf("quit during turn: closed %v, %v", closed, err)
	}
	select {
	case <-h.ctl.Closed():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not close after the turn was cancelled")
	}
	if codeOf(h.ctl.Input(agent.Input{Text: "late"})) != "closed" {
		t.Fatal("input accepted after quit")
	}
}

// AttachmentsDir is called from the UI (paste) while a deferred session switch rewrites
// the agent on another goroutine, so it must go through the controller lock.
func TestAttachmentsDirDuringSessionSwitch(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = h.ctl.AttachmentsDir()
		}
	}()
	for range 5 {
		if err := h.ctl.NewSession(); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

// /reload re-reads DASHY.md (even one created after startup). While a turn runs it
// does not interrupt it and applies once the turn ends; idle it applies at once.
func TestReloadRebuildsPromptAfterTurn(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	local := filepath.Join(h.dir, "DASHY.md")
	write := func(text string) {
		if err := os.WriteFile(local, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	turns := func(n int) {
		h.await(fmt.Sprintf("%d turns", n), func(events []Event) bool {
			return !h.ctl.Status().Busy && len(doneEvents(events)) == n
		})
	}
	write("v1")
	h.send("a")
	turns(1)
	if r := h.model.requests()[0]; strings.Contains(r, "v1") {
		t.Fatalf("a DASHY.md created after startup was used without /reload: %s", r)
	}
	h.ctl.Reload()
	h.model.mu.Lock()
	h.model.gate = make(chan struct{})
	h.model.mu.Unlock()
	h.send("b")
	h.await("request b", func([]Event) bool { return len(h.model.requests()) == 2 })
	write("v2")
	h.ctl.Reload()
	if !h.ctl.Status().Busy {
		t.Fatal("reload interrupted the running turn")
	}
	close(h.model.gate)
	turns(2)
	h.send("c")
	turns(3)
	reqs := h.model.requests()
	if !strings.Contains(reqs[1], "v1") || !strings.Contains(reqs[2], "v2") {
		t.Fatalf("prompt not rebuilt after reload:\nb: %s\nc: %s", reqs[1], reqs[2])
	}
	if events := h.log(); !slices.ContainsFunc(events, func(e Event) bool { return strings.Contains(e.Text, "after the current turn") }) {
		t.Fatalf("no deferred reload notice: %s", describe(events))
	}
}

// Input queued before the controller exists starts a turn at once, ahead of newer input.
func TestPrequeuedInputGoesFirst(t *testing.T) {
	model := &fakeModel{}
	h := newHarnessWith(t, model, func(a *agent.Agent) {
		a.Queue().PushNotice(agent.Input{Text: "NOTICE-A", Notice: true})
	})
	h.await("the prequeued turn", func(events []Event) bool { return len(doneEvents(events)) == 1 })
	if err := h.ctl.Input(agent.Input{Text: "B"}); err != nil {
		t.Fatal(err)
	}
	h.await("the second turn", func(events []Event) bool { return len(doneEvents(events)) == 2 })
	bodies := model.requests()
	if len(bodies) < 2 || !strings.Contains(bodies[0], "NOTICE-A") || strings.Contains(bodies[0], `"B"`) {
		t.Fatalf("requests %q", bodies)
	}
}
