package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestCompactRunsInTheBackground guards the interface against a blocking
// summary request: /compact must return immediately while the summary runs.
func TestCompactRunsInTheBackground(t *testing.T) {
	m, release := compactableModel(t)
	m.input.SetValue("/compact")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if status := m.ctl.Status(); !status.Busy || !status.Compacting {
		t.Fatalf("compaction did not start in the background: %+v", status)
	}
	if len(m.ctl.Status().Queued) != 0 {
		t.Fatal("compaction must not claim steering state")
	}
	release()
	m = awaitIdle(t, m)
	if !strings.HasPrefix(m.flash.Text, "Compacted") {
		t.Fatalf("compaction result was not reported: %q", m.flash.Text)
	}
	if m.ctl.Status().Busy {
		t.Fatal("compaction state was not cleared")
	}
}

// TestSendDuringCompactionIsRefused keeps a queued message from being stranded.
func TestSendDuringCompactionIsRefused(t *testing.T) {
	m, release := compactableModel(t)
	defer release()
	m.input.SetValue("/compact")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.input.SetValue("while compacting")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.ctl.Status().Queued) != 0 {
		t.Fatal("a message was queued while the compaction request held the history")
	}
	if !hasEvent(m, "Compaction is running; wait before sending.") {
		t.Fatalf("expected a refusal notice: %v", m.ctl.Events())
	}
	if m.input.Value() != "while compacting" {
		t.Fatalf("refused message must stay in the composer: %q", m.input.Value())
	}
}

// TestCompactWithGuidanceRunsFromEnter: text after /compact is the guidance,
// not an ordinary chat message.
func TestCompactWithGuidanceRunsFromEnter(t *testing.T) {
	m, _ := testModel(t)
	m.input.SetValue("/compact keep the API decisions")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = awaitIdle(t, m)
	if len(m.ctl.Status().Queued) != 0 || m.input.Value() != "" {
		t.Fatalf("/compact with guidance was treated as chat: queued=%v input=%q", m.ctl.Status().Queued, m.input.Value())
	}
	if !hasEvent(m, "Nothing to compact.") {
		t.Fatalf("compaction did not run: %+v", m.entries)
	}
}
