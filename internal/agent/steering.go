package agent

import (
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"dashy/internal/shell"
)

// Input keeps attachments with their instruction through acceptance and recall.
type Input struct {
	Text   string
	Images []shell.Image
	Files  []FileAttachment // snapshot taken when the prompt was submitted
	ID     string           // controller identity of queued steering; empty when the agent queued it itself
}

func (input Input) Display() string {
	if len(input.Images) == 0 {
		return input.Text
	}
	text := input.Text
	for _, image := range input.Images {
		text += "\n[Image: " + filepath.Base(image.Path) + "]"
	}
	return strings.TrimSpace(text)
}
func (a *Agent) addInput(input Input, emit func(Event), steering bool) {
	a.commitInput(input)
	e := inputEvent(input, steering)
	e.InputID = input.ID // lets the controller drop exactly the instruction it accepted
	emit(e)
}

// commitInput appends the input to API history without displaying it.
func (a *Agent) commitInput(input Input) {
	text := input.Display() + fileBlocks(input.Files)
	if len(input.Images) == 0 {
		a.message("user", text)
		return
	}
	parts := []any{map[string]any{"type": "input_text", "text": text + imageOriginals(input.Images)}}
	for _, img := range input.Images {
		parts = append(parts, map[string]any{"type": "input_image", "image_url": img.URL, "detail": "auto"})
	}
	a.History = append(a.History, raw(map[string]any{"role": "user", "content": parts}))
}

// Keep original paths in model context without cluttering the terminal labels.
func imageOriginals(images []shell.Image) string {
	var text strings.Builder
	for _, img := range images {
		if img.Path != "" {
			text.WriteString("\nOriginal image (for crops): " + strconv.Quote(img.Path))
		}
	}
	return text.String()
}

func inputEvent(input Input, steering bool) Event {
	kind := "user"
	if len(input.Images) == 0 && strings.HasPrefix(input.Text, "<notify>") {
		kind = "notice"
	}
	return Event{Kind: kind, Text: input.Display(), Steering: steering}
}

// Steering serializes acceptance and recall; taken instructions cannot be recalled.
type Steering struct {
	mu      sync.Mutex
	pending []Input
}

func (s *Steering) Push(text string) bool { return s.PushInput(Input{Text: text}) }
func (s *Steering) PushInput(input Input) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= 64 {
		return false
	}
	s.pending = append(s.pending, input)
	return true
}
func (s *Steering) TakeInputs() []Input {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	s.pending = nil
	return pending
}
func (s *Steering) RecallInput() (Input, bool) {
	if s == nil {
		return Input{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return Input{}, false
	}
	i := len(s.pending) - 1
	text := s.pending[i]
	s.pending = s.pending[:i]
	return text, true
}
