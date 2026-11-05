package agent

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"dashy/internal/shell"
)

type Input struct {
	Text   string
	Images []shell.Image
	sentAt time.Time
	Notice bool
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

func (a *Agent) addInput(input Input, emit func(Event)) {
	a.commitInput(input)
	e := inputEvent(input)
	e.Meta = a.itemMeta[len(a.History)-1]
	emit(e)
}

func (a *Agent) commitInput(input Input) {
	if input.sentAt.IsZero() {
		input.sentAt = time.Now()
	}
	defer func() { a.setItemMeta(len(a.History)-1, Metadata{At: input.sentAt}) }()
	a.slots.commit(len(input.Images))
	text := input.Display()
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

const originalImage = "\nOriginal image (for crops): "

func imageOriginals(images []shell.Image) string {
	var text strings.Builder
	for _, img := range images {
		if img.Path != "" {
			text.WriteString(originalImage + strconv.Quote(img.Path))
		}
	}
	return text.String()
}

func inputEvent(input Input) Event {
	kind := "user"
	if input.Notice || IsNotice(input.Text) {
		kind = "notice"
	}
	return Event{Kind: kind, Text: input.Display()}
}

type Steering struct {
	mu      sync.Mutex
	pending []Input
	wake    func()
	typed   chan struct{}
}

func (s *Steering) typedSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.typed == nil {
		s.typed = make(chan struct{}, 1)
	}
	return s.typed
}

func (s *Steering) drainTyped() {
	select {
	case <-s.typedSignal():
	default:
	}
}

func (s *Steering) hasTyped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.pending, func(input Input) bool { return !input.Notice })
}

func (s *Steering) OnPush(wake func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wake = wake
}

const maxQueued = 64

func (a *Agent) Queue() *Steering { return a.queue }

func (s *Steering) PushNotice(input Input) {
	input.sentAt = time.Now()
	s.mu.Lock()
	s.pending = append(s.pending, input)
	wake := s.wake
	s.mu.Unlock()
	if wake != nil {
		wake()
	}
}

func (s *Steering) Push(input Input) bool {
	input.sentAt = time.Now()
	s.mu.Lock()
	if len(s.pending) >= maxQueued {
		s.mu.Unlock()
		return false
	}
	s.pending = append(s.pending, input)
	wake := s.wake
	if s.typed != nil && !input.Notice {
		select {
		case s.typed <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	if wake != nil {
		wake()
	}
	return true
}

func (s *Steering) Take() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	s.pending = nil
	return pending
}

func (s *Steering) Pop() (Input, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return Input{}, false
	}
	input := s.pending[0]
	s.pending = s.pending[1:]
	return input, true
}

func (s *Steering) RemoveComposer() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	var composer []Input
	s.pending = slices.DeleteFunc(s.pending, func(input Input) bool {
		if !input.Notice {
			composer = append(composer, input)
		}
		return !input.Notice
	})
	return composer
}

func (s *Steering) Recall() (Input, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.pending) - 1; i >= 0; i-- {
		if input := s.pending[i]; !input.Notice {
			s.pending = slices.Delete(s.pending, i, i+1)
			return input, true
		}
	}
	return Input{}, false
}

func (s *Steering) Pending() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.pending), func(input Input) bool { return input.Notice })
}
