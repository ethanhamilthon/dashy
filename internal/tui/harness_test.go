package tui

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/agent"
	"dashy/internal/control"
)

// testModel returns a model backed by a real agent. The provider stub answers
// each request after hold is closed; a nil hold answers at once.
func testModel(t *testing.T, hold <-chan struct{}) (model, *agent.Agent) {
	return testModelConfig(t, hold, "")
}

// testModelConfig is testModel with extra configuration (root keys or tables).
func testModelConfig(t *testing.T, hold <-chan struct{}, extra string) (model, *agent.Agent) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	requests := make(chan struct{}, 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			select {
			case requests <- struct{}{}:
			default:
			}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary text","annotations":[]}]}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n")
	}))
	t.Cleanup(server.Close)
	config := fmt.Sprintf("provider='p'\nmodel='m'\n%s[providers.p]\nbase_url=%q\n", extra, server.URL)
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	m := newModel(a, "/project")
	t.Cleanup(m.ctl.Close)
	requestSignals.Store(m.ctl, requests)
	return m, a
}

// requestSignals maps a test controller to a channel that receives one value per
// streamed provider request, so tests can wait until a turn is really in flight.
var requestSignals sync.Map

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

// busyModel starts a turn whose answer waits for release, so the model is busy
// exactly as it is during a real request. Calling release lets the answer finish.
func busyModel(t *testing.T) (model, func()) {
	t.Helper()
	m, _, release := heldModel(t, "")
	next, _ := m.submitInput("busy")
	m = next.(model)
	if !m.ctl.Status().Busy {
		t.Fatal("turn did not start")
	}
	awaitRequest(t, m)
	return settle(m), release
}

// heldModel returns a model whose provider answers only after release is called.
func heldModel(t *testing.T, extra string) (model, *agent.Agent, func()) {
	t.Helper()
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	m, a := testModelConfig(t, hold, extra)
	return m, a, release
}

// compactableModel holds a model whose history has enough turns to compact;
// the summary request waits for release.
func compactableModel(t *testing.T) (model, func()) {
	t.Helper()
	m, a, release := heldModel(t, "compact_keep_turns = 1\n")
	a.History = append(a.History,
		[]byte(`{"role":"user","content":"one"}`),
		[]byte(`{"role":"assistant","content":"done one"}`),
		[]byte(`{"role":"user","content":"two"}`),
		[]byte(`{"role":"assistant","content":"done two"}`))
	return m, release
}

// settle applies every controller event already emitted, as the program does
// when it delivers them, and returns the updated model.
func settle(m model) model {
	for {
		event, ok := m.events.TryPop()
		if !ok {
			return m
		}
		next, _ := m.Update(event)
		m = next.(model)
	}
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

// key sends one key press and applies the controller events it caused.
func key(m model, k tea.KeyMsg) model {
	next, _ := m.Update(k)
	return settle(next.(model))
}

// startBusy begins a held turn through the controller without touching the
// composer, so tests can type while the model is busy.
func startBusy(t *testing.T, m model) model {
	t.Helper()
	if _, err := m.ctl.Submit(control.Request{Type: "input", Text: "busy"}); err != nil {
		t.Fatal(err)
	}
	awaitRequest(t, m)
	return settle(m)
}

// agentEvent wraps an agent event as the controller delivers it.
func agentEvent(e agent.Event) control.Event { return control.Event{Kind: "agent", Agent: &e} }
