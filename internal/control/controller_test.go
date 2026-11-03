package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// fakeModel answers provider requests: streamed ones with an answer, a summary
// request (not streamed) with SUMMARY. gate, when set, holds every request until
// it is closed or the request is cancelled.
type fakeModel struct {
	mu      sync.Mutex
	bodies  []string
	gate    chan struct{}
	summary bool
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
	home, dir := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	server := httptest.NewServer(model)
	t.Cleanup(server.Close)
	s := agent.DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = agent.Provider{BaseURL: server.URL}
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(home, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(dir, agent.Options{})
	if err != nil {
		t.Fatal(err)
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

// A notification acknowledged during a busy turn precedes composer steering sent later.
func TestNotificationKeepsOrderWithSteering(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, &fakeModel{gate: gate})
	h.send("first")
	h.inFlight(1)
	if err := agent.Notify(h.dir, h.a.SessionID(), "notice A"); err != nil {
		t.Fatal(err)
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

// Build n finished turns with distinct user text.
func runTurns(h *harness, n int) {
	for i := 0; i < n; i++ {
		h.send(fmt.Sprintf("turn %d", i))
		h.settle()
	}
}

func TestCompactionCanBeCancelledWhileSummarizing(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, &fakeModel{})
	runTurns(h, 6)
	h.model.mu.Lock()
	h.model.gate = gate
	h.model.mu.Unlock()
	before := len(h.model.requests())
	if err := h.ctl.Compact(""); err != nil {
		t.Fatal(err)
	}
	h.await("summary request", func([]Event) bool { return len(h.model.requests()) > before })
	h.ctl.Stop()
	done := doneEvents(h.settle())
	if len(done) != 7 || done[6].Operation != "compact" || done[6].Status != "cancelled" {
		t.Fatalf("done = %+v", doneEvents(h.log()))
	}
}

// Compaction shrinks only the history sent to the model: the display keeps the
// earlier conversation and the interface's own notes, and reports the compaction.
func TestCompactKeepsDisplay(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	runTurns(h, 6)
	h.ctl.Note(agent.Event{Kind: "notice", Text: "interface note"})
	if err := h.ctl.Compact(""); err != nil {
		t.Fatal(err)
	}
	h.settle()
	joined := fmt.Sprint(h.ctl.History())
	if !strings.Contains(joined, "turn 0") || !strings.Contains(joined, "interface note") || !strings.Contains(joined, "Compacted") {
		t.Fatalf("display after compaction: %s", joined)
	}
	if strings.Contains(fmt.Sprint(h.a.Records()), "turn 0") {
		t.Fatal("history was not compacted")
	}
}

// Automatic compaction, triggered inside a turn, keeps the display too and replaces no session.
func TestAutomaticCompactionKeepsDisplay(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	runTurns(h, 5)
	huge := strings.Repeat("x", 4*agent.DefaultCompactThreshold+100)
	h.send("big " + huge)
	events := h.settle()
	for _, e := range events {
		if e.Kind == "session" {
			t.Fatalf("compaction replaced the display: %s", describe(events))
		}
	}
	joined := fmt.Sprint(h.ctl.History())
	if !strings.Contains(joined, "turn 0") || !strings.Contains(joined, "Compacted") {
		t.Fatalf("display after automatic compaction: %s", joined)
	}
	if strings.Contains(fmt.Sprint(h.a.Records()), "turn 0") {
		t.Fatal("history was not compacted")
	}
}

// Images queued by dashy view and images in the composer share one allowance of four.
func TestImageAllowanceIsSharedAcrossSources(t *testing.T) {
	h := newHarness(t, &fakeModel{})
	png := shell.Image{URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t))}
	endpoint, err := agent.Endpoint(h.dir, h.a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := agent.Send(endpoint, agent.Message{Image: &png}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.ctl.Input(agent.Input{Text: "look", Images: []shell.Image{png, png, png}}); codeOf(err) != "attachment_limit" {
		t.Fatalf("three composer images on top of two views: %v", err)
	}
	if err := h.ctl.Input(agent.Input{Text: "look", Images: []shell.Image{png, png}}); err != nil {
		t.Fatalf("four images in total must be allowed: %v", err)
	}
}

// A settings change while a turn runs is rejected, and nothing changes. The turn
// is started by an external notification, as it can be while the picker is open.
func TestSettingsRejectedWhileNotificationTurnRuns(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	h := newHarness(t, &fakeModel{gate: gate})
	before := h.ctl.Settings()
	if err := agent.Notify(h.dir, h.a.SessionID(), "background result"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !h.ctl.Status().Busy; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("notification did not start a turn")
		}
	}
	for name, err := range map[string]error{
		"reasoning": h.ctl.ChangeSetting(SettingReasoning),
		"compact":   h.ctl.ChangeSetting(SettingAutoCompact),
		"lines":     h.ctl.ChangeSetting(SettingLines),
		"model":     h.ctl.SelectModel("p", "other", ""),
	} {
		if codeOf(err) != "busy" {
			t.Errorf("%s while busy: %v", name, err)
		}
	}
	if _, _, err := h.ctl.Models(t.Context()); codeOf(err) != "busy" {
		t.Errorf("model listing while busy: %v", err)
	}
	if after := h.ctl.Settings(); after.ReasoningSummary != before.ReasoningSummary || after.ShowLines != before.ShowLines || after.Model != before.Model {
		t.Fatalf("settings changed while busy: %+v", after)
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

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	return data
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
	for _, setting := range []string{SettingReasoning, SettingAutoCompact, SettingLines, SettingThreshold} {
		if err := h.ctl.ChangeSetting(setting); err != nil {
			t.Fatal(err)
		}
	}
	s := h.ctl.Settings()
	if s.Model != "m2" || s.Effort != "high" || s.ShowLines != "medium" || s.AutoCompact || s.CompactThreshold != 800_000 || s.Providers["q"].APIKey != "sk-x" {
		t.Fatalf("settings: %+v", s)
	}
}

// Rename and compaction are refused while a turn runs; quitting during a turn waits
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
	if codeOf(h.ctl.Rename("x")) != "busy" || codeOf(h.ctl.Compact("")) != "busy" {
		t.Fatal("rename or compaction accepted during a turn")
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
