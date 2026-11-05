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

// Input keeps attachments with their instruction through acceptance and recall.
type Input struct {
	Text   string
	Images []shell.Image
	sentAt time.Time
	Notice bool // from dashy itself (task result, view image): queued in order with composer input, never recalled
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

// commitInput appends the input to API history without displaying it.
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

func inputEvent(input Input) Event {
	kind := "user"
	if input.Notice || IsNotice(input.Text) {
		kind = "notice"
	}
	return Event{Kind: kind, Text: input.Display()}
}

// Steering is the session's one ordered queue of input waiting for the running turn: composer
// input, task results and dashy view images, in admission order. Taken input cannot be recalled.
type Steering struct {
	mu      sync.Mutex
	pending []Input
	wake    func()        // called after every push; a new session's queue keeps it
	typed   chan struct{} // signalled when composer input is pushed
}

// typedSignal fires when composer input is pushed (not on notices).
func (s *Steering) typedSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.typed == nil {
		s.typed = make(chan struct{}, 1)
	}
	return s.typed
}

// drainTyped forgets earlier signals; then the signal means input pushed from now on.
func (s *Steering) drainTyped() {
	select {
	case <-s.typedSignal():
	default:
	}
}

// hasTyped reports whether composer input is waiting.
func (s *Steering) hasTyped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.ContainsFunc(s.pending, func(input Input) bool { return !input.Notice })
}

// OnPush sets the function called after every push, so an idle session can start a turn at once.
func (s *Steering) OnPush(wake func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wake = wake
}

// maxQueued bounds the steering queue.
const maxQueued = 64

// Queue returns the session's steering queue.
func (a *Agent) Queue() *Steering { return a.queue }

// PushNotice queues input from dashy itself, such as a task result, even when the queue is full.
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

// Push queues input behind everything waiting; false when the queue is full.
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

// Take removes all queued input, in order, for acceptance by the running turn.
func (s *Steering) Take() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.pending
	s.pending = nil
	return pending
}

// Pop removes the oldest queued input, which starts the next turn.
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

// RemoveComposer removes the composer input a turn did not accept and returns it; notices stay queued.
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

// Recall removes the newest composer input that is still queued.
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

// Pending returns the composer input still queued.
func (s *Steering) Pending() []Input {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(s.pending), func(input Input) bool { return input.Notice })
}
