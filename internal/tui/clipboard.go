package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"dashy/internal/agent"
	"dashy/internal/shell"
	tea "github.com/charmbracelet/bubbletea"
)

type clipboardResult struct {
	generation int
	image      shell.Image
	text       string
	err        error
}

// Bounded command output prevents clipboard data from growing host memory indefinitely.
type clipboardBuffer struct {
	data  []byte
	limit int
}

func (b *clipboardBuffer) Write(data []byte) (int, error) {
	if len(b.data)+len(data) > b.limit {
		return 0, fmt.Errorf("clipboard output exceeds size limit")
	}
	b.data = append(b.data, data...)
	return len(data), nil
}
func clipboardCommand(ctx context.Context, limit int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, diagnostic := &clipboardBuffer{limit: limit}, &clipboardBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, diagnostic
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("clipboard %s: %w: %s", name, err, diagnostic.data)
	}
	return out.data, nil
}

// AppKit bridges native pasteboard image formats to PNG; policy and validation stay in Go.
const clipboardPNG = `ObjC.import("AppKit");
function run(argv) {
 var image = $.NSImage.alloc.initWithPasteboard($.NSPasteboard.generalPasteboard);
 if (image.isNil()) return "text";
 var bitmap = $.NSBitmapImageRep.imageRepWithData(image.TIFFRepresentation);
 if (bitmap.isNil()) throw new Error("Cannot decode clipboard image");
 var png = bitmap.representationUsingTypeProperties($.NSBitmapImageFileTypePNG, $.NSDictionary.dictionary);
 if (png.isNil()) throw new Error("Cannot convert clipboard image to PNG");
 if (!png.writeToFileAtomically($(argv[0]), false)) throw new Error("Cannot save clipboard image");
 return "image";
}`

type clipboardRunner func(context.Context, int, string, ...string) ([]byte, error)

func readClipboard(ctx context.Context, directory string) (shell.Image, string, error) {
	return readClipboardWith(ctx, directory, runtime.GOOS, clipboardCommand)
}
func readClipboardWith(ctx context.Context, directory, platform string, run clipboardRunner) (shell.Image, string, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return shell.Image{}, "", err
	}
	file, err := os.CreateTemp(directory, "paste-*.png")
	if err != nil {
		return shell.Image{}, "", err
	}
	path := file.Name()
	if err = file.Close(); err != nil {
		os.Remove(path)
		return shell.Image{}, "", err
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(path)
		}
	}()
	var data []byte
	var textCommand string
	var textArgs []string
	if platform == "darwin" {
		result, e := run(ctx, 4096, "/usr/bin/osascript", "-l", "JavaScript", "-e", clipboardPNG, path)
		if e != nil {
			return shell.Image{}, "", e
		}
		if strings.TrimSpace(string(result)) == "image" {
			image, e := shell.CaptureImage(path)
			if e != nil {
				return shell.Image{}, "", e
			}
			image.Path, keep = path, true
			return image, "", nil
		}
		textCommand = "/usr/bin/pbpaste"
	} else if _, e := exec.LookPath("wl-paste"); e == nil && os.Getenv("WAYLAND_DISPLAY") != "" {
		types, e := run(ctx, 16384, "wl-paste", "--list-types")
		if e != nil {
			return shell.Image{}, "", e
		}
		if strings.Contains(string(types), "image/png") {
			data, err = run(ctx, shell.MaxImageInputBytes, "wl-paste", "--type", "image/png")
		}
		textCommand, textArgs = "wl-paste", []string{"--no-newline"}
	} else {
		types, e := run(ctx, 16384, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
		if e != nil {
			return shell.Image{}, "", e
		}
		if strings.Contains(string(types), "image/png") {
			data, err = run(ctx, shell.MaxImageInputBytes, "xclip", "-selection", "clipboard", "-t", "image/png", "-o")
		}
		textCommand, textArgs = "xclip", []string{"-selection", "clipboard", "-o"}
	}
	if err != nil {
		return shell.Image{}, "", err
	}
	if len(data) > 0 {
		if err = os.WriteFile(path, data, 0600); err != nil {
			return shell.Image{}, "", err
		}
		image, e := shell.CaptureImage(path)
		if e != nil {
			return shell.Image{}, "", e
		}
		image.Path, keep = path, true
		return image, "", nil
	}
	text, err := run(ctx, 256*1024, textCommand, textArgs...)
	return shell.Image{}, string(text), err
}
func (m model) pasteClipboard() (tea.Model, tea.Cmd) {
	if m.pasting {
		return m, nil
	}
	directory := m.ctl.AttachmentsDir()
	m.pasting = true
	generation := m.pasteGeneration
	return m, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		image, text, err := readClipboard(ctx, directory)
		return clipboardResult{generation, image, text, err}
	}
}
func (m *model) acceptClipboard(result clipboardResult) tea.Cmd {
	if result.generation != m.pasteGeneration {
		if result.image.Path != "" {
			_ = os.Remove(result.image.Path)
		}
		return nil
	}
	m.pasting = false
	if result.err != nil {
		m.add("error", result.err.Error())
		return nil
	}
	if result.image.URL != "" {
		if m.menuKind != "" {
			_ = os.Remove(result.image.Path)
			m.add("notice", "Images can only be pasted into a message draft.")
			return nil
		}
		if len(m.images)+m.ctl.Status().Images >= agent.MaxPendingImages {
			_ = os.Remove(result.image.Path)
			m.add("error", fmt.Sprintf("At most %d new images per request. Remove or submit images first.", agent.MaxPendingImages))
		} else {
			m.images = append(m.images, result.image)
		}
		return nil
	}
	// Clipboard text replaces an active composer selection.
	if _, _, ok := m.activeComposerSelection(); ok {
		return m.replaceComposerSelection(result.text)
	}
	m.input.InsertString(result.text)
	return nil
}
