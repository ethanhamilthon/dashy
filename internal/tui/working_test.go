package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"dashy/internal/agent"
)

func TestWorkingIndicatorLifecycleAndScrolling(t *testing.T) {
	idle, _ := testModel(t)
	if idle.workingBlock() != "" {
		t.Fatal("idle indicator shown")
	}
	m, _ := busyModel(t)
	for i := 0; i < 40; i++ {
		m.add("user", "history")
	}
	m.refresh(true)
	if view := ansi.Strip(m.View()); !strings.Contains(view, "working") || !strings.Contains(view, "esc to stop") {
		t.Fatal("missing indicator or stop hint")
	}
	m.viewport.GotoTop()
	offset := m.viewport.YOffset
	next, cmd := m.Update(workingTick{m.ctl.Status().Started})
	m = next.(model)
	if cmd == nil || m.viewport.YOffset != offset {
		t.Fatal("tick must animate without stealing scroll")
	}
	next, cmd = m.Update(workingTick{time.Now().Add(-time.Hour)})
	m = next.(model)
	if cmd != nil {
		t.Fatal("stale timer restarted")
	}
	m.requestCancel()
	m = awaitIdle(t, m)
	if m.workingBlock() != "" {
		t.Fatal("indicator persisted after completion")
	}
	if !hasEvent(m, "Turn cancelled.") {
		t.Fatalf("cancellation not reported: %+v", m.entries)
	}
}

// hasEvent reports whether the session events end a line with text.
func hasEvent(m model, text string) bool {
	for _, e := range m.ctl.Events() {
		if strings.HasSuffix(e, " "+text) {
			return true
		}
	}
	return false
}

func TestSteeringQueueAndRecovery(t *testing.T) {
	m, release := busyModel(t)
	m.input.SetValue("follow up")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "" || len(m.ctl.Status().Queued) != 1 {
		t.Fatalf("message not queued: %q %+v", m.input.Value(), m.ctl.Status().Queued)
	}
	if !strings.Contains(ansi.Strip(m.workingBlock()), "queued · follow up") {
		t.Fatal("pending message invisible")
	}
	// Once the model's answer completes, the agent accepts the steering at its boundary.
	release()
	m = awaitIdle(t, m)
	users := 0
	for _, e := range m.entries {
		if e.Kind == "user" && e.Text == "follow up" {
			users++
		}
	}
	if users != 1 || len(m.ctl.Status().Queued) != 0 {
		t.Fatalf("accepted steering duplicated or not cleared: users=%d entries=%+v", users, m.entries)
	}
}

func TestCancelledTurnRestoresUnsentSteeringIntoComposer(t *testing.T) {
	m, _ := busyModel(t)
	m.input.SetValue("not accepted")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.requestCancel()
	m.input.SetValue("draft")
	m = awaitIdle(t, m)
	if m.input.Value() != "not accepted\n\ndraft" || len(m.ctl.Status().Queued) != 0 {
		t.Fatalf("pending text lost: %q", m.input.Value())
	}
}

func TestSteeringQueueFullKeepsInput(t *testing.T) {
	m, _ := busyModel(t)
	for i := 0; i < 64; i++ {
		if err := m.ctl.Input(agent.Input{Text: "first"}); err != nil {
			t.Fatal(err)
		}
	}
	m.input.SetValue("second")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "second" || len(m.ctl.Status().Queued) != 64 {
		t.Fatal("queue-full submission lost")
	}
	if !hasEvent(m, "Steering queue is full; your message remains in the input.") {
		t.Fatalf("queue-full notice missing: %+v", m.entries)
	}
}
