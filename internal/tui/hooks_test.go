package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"dashy/internal/shell"
)

// TestInterfaceEvents covers the events only the interface can know about:
// idle (no follow-up turn starts), queued steering and pasted images.
func TestInterfaceEvents(t *testing.T) {
	out := filepath.Join(t.TempDir(), "events")
	hooks := "[hooks.test]\nevents = [\"idle\", \"steer_queued\", \"image_attached\"]\ncommand = 'echo \"$DASHY_EVENT|$DASHY_IMAGE_SOURCE|$(cat \"$DASHY_MESSAGE_FILE\" 2>/dev/null)\" >> " + out + "'\n"
	m, a, release := heldModel(t, hooks)
	next, _ := m.submitInput("busy")
	m = next.(model)
	m.input.SetValue("keep going")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	release()
	m = awaitIdle(t, m)
	cmd := m.acceptClipboard(clipboardResult{generation: m.pasteGeneration, image: shell.Image{URL: "data:image/png;base64,AA=="}})
	_ = cmd
	m.input.SetValue("/compact")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m = awaitIdle(t, m)
	a.Finish(0)
	data, _ := os.ReadFile(out)
	want := "steer_queued||keep going\nidle||\nimage_attached|paste|\nidle||\n"
	if string(data) != want || strings.Count(string(data), "idle") != 2 {
		t.Fatalf("got %q want %q", data, want)
	}
}

var _ tea.Msg = nil
