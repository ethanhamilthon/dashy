package tui

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	for _, kind := range []string{"image", "text", "failure"} {
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
				if err := os.WriteFile(args[len(args)-1], data, 0600); err != nil {
					t.Fatal(err)
				}
				return []byte("image\n"), nil
			}
			result, text, err := readClipboard(context.Background(), directory, "darwin", run)
			files, e := os.ReadDir(directory)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "image" {
				if err != nil || text != "" || filepath.Dir(result) != directory || len(files) != 1 {
					t.Fatalf("%q %q %v", result, text, err)
				}
				info, e := os.Stat(result)
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

// A pasted image goes into the draft as its path; the agent opens it with dashy view.
func TestPastedImageInsertsPath(t *testing.T) {
	m := bareModel(t, "/project")
	m.input.SetValue("look at ")
	m.input.CursorEnd()
	m.pasting = true
	next, _ := m.Update(clipboardResult{generation: m.pasteGeneration, image: "/tmp/paste-1.png"})
	if m = next.(model); m.input.Value() != "look at /tmp/paste-1.png " || m.pasting {
		t.Fatalf("draft %q", m.input.Value())
	}
}
