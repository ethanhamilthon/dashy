package tui

import (
	"bytes"
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
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/agent"
	"dashy/internal/control"
	"dashy/internal/osc7501"
)

// syncBuffer collects terminal reports written by the reporter.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fixture is a TUI model over a real agent and provider stub.
type fixture struct {
	m       model
	a       *agent.Agent
	out     *syncBuffer
	fail    atomic.Bool // answer every request with HTTP 400
	hold    atomic.Pointer[chan struct{}]
	reached chan struct{} // one value per streamed request that reached the provider
	project string
}

// newFixture builds a model whose provider answers "answer" after the hold channel
// (when set) is closed. home is DASHY_HOME; providers is written to settings.json.
func newFixture(t *testing.T, providers bool) *fixture {
	t.Helper()
	f := &fixture{out: &syncBuffer{}, project: t.TempDir(), reached: make(chan struct{}, 64)}
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		if f.fail.Load() {
			http.Error(w, `{"error":{"message":"rejected"}}`, http.StatusBadRequest)
			return
		}
		stream := bytes.Contains(body, []byte(`"stream":true`))
		if stream {
			select {
			case f.reached <- struct{}{}:
			default:
			}
		}
		if hold := f.hold.Load(); hold != nil {
			select {
			case <-*hold:
			case <-r.Context().Done():
				return
			}
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary text","annotations":[]}]}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n")
	}))
	t.Cleanup(server.Close)
	s := agent.DefaultSettings()
	if providers {
		s.Provider, s.Model = "p", "m"
		s.Providers["p"] = agent.Provider{BaseURL: server.URL}
	}
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(home, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(f.project, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	reporter := osc7501.New(f.out, true)
	f.a, f.m = a, newModel(a, f.project, reporter)
	requestSignals.Store(f.m.ctl, f.reached)
	t.Cleanup(func() {
		f.m.ctl.Close()
		a.Close()
		reporter.Close()
	})
	return f
}

// pump applies the controller events queued so far, as the program would.
func (f *fixture) pump() {
	for {
		e, ok := f.m.events.TryPop()
		if !ok {
			f.m = flushFrame(f.m)
			return
		}
		next, _ := f.m.Update(e)
		f.m = next.(model)
	}
}

// settle pumps events until the controller is idle again.
func (f *fixture) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		f.pump()
		if !f.m.ctl.Status().Busy {
			f.pump()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// run sends text through the composer path and waits for the turn to finish.
func (f *fixture) run(t *testing.T, text string) {
	t.Helper()
	next, _ := f.m.sendInput(agent.Input{Text: text})
	f.m = next.(model)
	f.settle(t)
}

// command executes a slash command as the composer does.
func (f *fixture) command(t *testing.T, name string) {
	t.Helper()
	next, _ := f.m.runCommand(name)
	f.m = next.(model)
	f.pump()
}

// flushFrame applies a refresh that the frame throttle deferred.
func flushFrame(m model) model {
	if m.dirty {
		next, _ := m.Update(redraw{})
		m = next.(model)
	}
	return m
}

// key sends one key press to the model, as the program would.
func (f *fixture) key(k tea.KeyMsg) {
	if next, _, handled := f.m.pickerKey(k); handled {
		f.m = next.(model)
	}
}

// lastReport returns the state and the decoded msg of the last OSC 7501 report written.
func lastReport(t *testing.T, out string) (state, msg string) {
	t.Helper()
	state, encoded := lastRaw(out)
	if encoded == "" {
		return state, ""
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return state, string(data)
}

// lastRaw is the state and the encoded msg of the last OSC 7501 report written.
func lastRaw(out string) (state, msg string) {
	i := strings.LastIndex(out, "\x1b]7501;")
	if i < 0 {
		return "", ""
	}
	body := strings.TrimSuffix(out[i+len("\x1b]7501;"):], "\x1b\\")
	for _, pair := range strings.Split(body, ":") {
		if v, ok := strings.CutPrefix(pair, "state="); ok {
			state = v
		}
		if v, ok := strings.CutPrefix(pair, "msg="); ok {
			msg = v
		}
	}
	return state, msg
}

// requestSignals maps a test controller to the channel that receives one value per
// streamed provider request, so tests can wait until a turn is really in flight.
var requestSignals sync.Map

// heldFixture is a fixture whose provider answers only after the returned release is called.
func heldFixture(t *testing.T) (*fixture, func()) {
	t.Helper()
	f := newFixture(t, true)
	hold := make(chan struct{})
	f.hold.Store(&hold)
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	return f, release
}

// testModel returns a model over a real agent whose provider answers at once.
func testModel(t *testing.T) (model, *agent.Agent) {
	t.Helper()
	f := newFixture(t, true)
	return f.m, f.a
}

// heldModel returns a model whose provider answers only after release is called.
func heldModel(t *testing.T) (model, *agent.Agent, func()) {
	t.Helper()
	f, release := heldFixture(t)
	return f.m, f.a, release
}

// compactableModel is a held model whose history has enough turns to compact;
// the summary request waits for release.
func compactableModel(t *testing.T) (model, func()) {
	t.Helper()
	m, a, release := heldModel(t)
	for i, text := range []string{"one", "two", "three", "four", "five"} {
		a.Records()
		_ = i
		a.History = append(a.History, []byte(`{"role":"user","content":"`+text+`"}`), []byte(`{"role":"assistant","content":"done `+text+`"}`))
	}
	return m, release
}

// awaitRequest blocks until the provider has received a streamed request for m.
func awaitRequest(t *testing.T, m model) {
	t.Helper()
	signals, ok := requestSignals.Load(m.ctl)
	if !ok {
		t.Fatal("model has no provider")
	}
	select {
	case <-signals.(chan struct{}):
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not receive the request")
	}
}

// busyModel starts a turn whose answer waits for release, so the model is busy as during a real request.
func busyModel(t *testing.T) (model, func()) {
	t.Helper()
	m, _, release := heldModel(t)
	m = startBusy(t, m)
	return m, release
}

// startBusy begins a held turn through the controller without touching the composer.
func startBusy(t *testing.T, m model) model {
	t.Helper()
	if err := m.ctl.Input(agent.Input{Text: "busy"}); err != nil {
		t.Fatal(err)
	}
	awaitRequest(t, m)
	return settle(m)
}

// awaitIdle waits for the active operation to finish, applying its events.
func awaitIdle(t *testing.T, m model) model {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m = settle(m)
		if !m.ctl.Status().Busy {
			return settle(m)
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle applies the controller events already emitted and returns the updated model.
func settle(m model) model {
	for {
		event, ok := m.events.TryPop()
		if !ok {
			return flushFrame(m)
		}
		next, _ := m.Update(event)
		m = next.(model)
	}
}

// key sends one key press and applies the controller events it caused.
func key(m model, k tea.KeyMsg) model {
	next, _ := m.Update(k)
	return settle(next.(model))
}

// agentEvent wraps an agent event as the controller delivers it.
func agentEvent(e agent.Event) control.Event { return control.Event{Kind: "agent", Agent: &e} }

// freshAgent opens a new project agent with an empty DASHY_HOME and no providers.
func freshAgent(t *testing.T) *agent.Agent {
	t.Helper()
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// freshModel is a model over freshAgent.
func freshModel(t *testing.T) (model, *agent.Agent) {
	t.Helper()
	a := freshAgent(t)
	return newModel(a, a.Shell.Dir, osc7501.New(io.Discard, false)), a
}

// bareModel is a model over freshAgent in the given working directory.
func bareModel(t *testing.T, cwd string) model {
	t.Helper()
	m, _ := freshModel(t)
	m.cwd = cwd
	return m
}

// awaitReport waits until the terminal has been told the state and returns its msg.
// Reports are coalesced, so they arrive shortly after the event.
func awaitReport(t *testing.T, f *fixture, state string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got, msg := lastReport(t, f.out.String()); got == state {
			return msg
		}
		if time.Now().After(deadline) {
			got, _ := lastReport(t, f.out.String())
			t.Fatalf("terminal state %q, want %q", got, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lastTitle is the decoded title of the last OSC 7501 report written ("" when none).
func lastTitle(out string) string {
	i := strings.LastIndex(out, "\x1b]7501;")
	if i < 0 {
		return ""
	}
	body := strings.TrimSuffix(out[i+len("\x1b]7501;"):], "\x1b\\")
	for _, pair := range strings.Split(body, ":") {
		if v, ok := strings.CutPrefix(pair, "title="); ok {
			data, _ := base64.StdEncoding.DecodeString(v)
			return string(data)
		}
	}
	return ""
}

// awaitTitle waits until the terminal has been told the session title want.
func awaitTitle(t *testing.T, f *fixture, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for lastTitle(f.out.String()) != want {
		if time.Now().After(deadline) {
			t.Fatalf("terminal title %q, want %q", lastTitle(f.out.String()), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
