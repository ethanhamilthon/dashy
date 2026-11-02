package tui

import (
	"bytes"
	"context"
	"dashy/internal/shell"
	"errors"
	tea "github.com/charmbracelet/bubbletea"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func TestClipboardReaderImageTextAndFailures(t *testing.T) {
	for _, kind := range []string{"image", "text", "invalid", "failure"} {
		t.Run(kind, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "images")
			data := pngBytes(t)
			run := func(_ context.Context, _ int, name string, args ...string) ([]byte, error) {
				if kind == "failure" {
					return nil, errors.New("clipboard unavailable")
				}
				if name == "/usr/bin/pbpaste" {
					return []byte("ordinary text"), nil
				}
				if kind == "text" {
					return []byte("text\n"), nil
				}
				if kind == "invalid" {
					data = []byte("not an image")
				}
				if err := os.WriteFile(args[len(args)-1], data, 0600); err != nil {
					t.Fatal(err)
				}
				return []byte("image\n"), nil
			}
			result, text, err := readClipboardWith(context.Background(), directory, "darwin", run)
			files, e := os.ReadDir(directory)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "image" {
				if err != nil || text != "" || !strings.HasPrefix(result.URL, "data:image/png;base64,") || len(files) != 1 {
					t.Fatalf("%+v %q %v", result, text, err)
				}
				info, e := os.Stat(result.Path)
				if e != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("image permissions: %v %v", info, e)
				}
			} else {
				if len(files) != 0 {
					t.Fatal("abandoned clipboard file left behind")
				}
				if kind == "text" && (err != nil || text != "ordinary text") {
					t.Fatalf("text fallback: %q %v", text, err)
				}
				if kind != "text" && err == nil {
					t.Fatal("failed capture accepted")
				}
			}
		})
	}
}
func TestAttachmentQueueRecallAndCancellationRecovery(t *testing.T) {
	m, release := busyModel(t)
	defer release()
	m.images = []shell.Image{{URL: "data:image/png;base64,one", Path: "one.png"}}
	m.input.SetValue("look")
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.images) != 0 || len(m.ctl.Status().Queued) != 1 || len(m.ctl.Status().Queued[0].Images) != 1 {
		t.Fatal("image not queued with text")
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyUp})
	if len(m.images) != 1 || m.input.Value() != "look" || len(m.ctl.Status().Queued) != 0 {
		t.Fatalf("recall lost/duplicated image: %q %v", m.input.Value(), m.images)
	}
	m = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	m.input.SetValue("draft")
	m.requestCancel()
	m = awaitIdle(t, m)
	if len(m.images) != 1 || m.input.Value() != "look\n\ndraft" {
		t.Fatalf("cancellation recovery: %q %v", m.input.Value(), m.images)
	}
}

type csiTestMessage string

func (m csiTestMessage) String() string { return string(m) }
func TestClipboardShortcutDecodesFromTerminalBytes(t *testing.T) {
	for _, sequence := range []string{"\x16", "\x1b[118;9u", "\x1b[1084;5u", "\x1b[1084::118;9u"} {
		m := bareModel(t, "/project")
		seen := false
		program := tea.NewProgram(m, tea.WithInput(strings.NewReader(sequence)), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			if key, ok := normalizeTerminalKey(msg).(tea.KeyMsg); ok && key.Type == tea.KeyCtrlV {
				seen = true
				return tea.QuitMsg{}
			}
			return msg
		}))
		if _, err := program.Run(); err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Fatalf("terminal sequence not recognized: %q", sequence)
		}
	}
}

func TestPendingImagesPreventDestructiveSessionSwitch(t *testing.T) {
	m := bareModel(t, "/project")
	m.input.SetValue("caption")
	m.images = []shell.Image{{URL: "data", Path: "draft.png"}}
	next, cmd := m.runCommand("/new")
	m = next.(model)
	if cmd != nil || m.input.Value() != "caption" || len(m.images) != 1 {
		t.Fatal("session switch discarded unsent attachment")
	}
}
