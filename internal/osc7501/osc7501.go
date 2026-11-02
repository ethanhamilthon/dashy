package osc7501

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/term"

	"dashy/internal/control"
)

// Protocol limits from OSC 7501 specification.
const (
	MaxSequenceLen = 4096
	MaxAppLen      = 32
	MaxTitleRunes  = 60
	MaxTitleDec    = 192 // decoded bytes must be < 192
	MaxTitleEnc    = 256
	MaxMsgRunes    = 120
	MaxMsgDec      = 2048 // decoded bytes must be < 2048
	MaxMsgEnc      = 2732
)

// Allowed states according to OSC 7501.
func isValidState(s string) bool {
	switch s {
	case "idle", "working", "done", "blocked", "error", "clear":
		return true
	default:
		return false
	}
}

func isValidApp(app string) bool {
	if len(app) == 0 || len(app) > MaxAppLen {
		return false
	}
	for i := 0; i < len(app); i++ {
		c := app[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '+' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// Report holds the parameters of an OSC 7501 report.
type Report struct {
	State string
	App   string
	Title string
	Msg   string
}

func isControl(r rune) bool {
	return (r >= 0 && r <= 0x1f) || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// SanitizeText replaces control characters and whitespace runs with a single
// space, trims the result, and truncates to at most maxRunes and maxBytes,
// appending an ellipsis ('…') if truncated.
func SanitizeText(s string, maxRunes, maxBytes int) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if isControl(r) || unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
		} else {
			b.WriteRune(r)
			inSpace = false
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return ""
	}
	runes := []rune(cleaned)
	cut := false
	if maxRunes > 0 && len(runes) > maxRunes {
		cut = true
		runes = runes[:maxRunes-1]
	}
	if maxBytes > 0 {
		for len(runes) > 0 {
			targetLen := len(string(runes))
			if cut {
				targetLen += len("…")
			}
			if targetLen < maxBytes {
				break
			}
			cut = true
			runes = runes[:len(runes)-1]
		}
	}
	if cut {
		return string(runes) + "…"
	}
	return string(runes)
}

// SanitizeTitle sanitizes and bounds a session title: at most 60 runes,
// decoded < 192 bytes, encoded <= 256 bytes.
func SanitizeTitle(title string) string {
	return SanitizeText(title, MaxTitleRunes, MaxTitleDec)
}

// SanitizeMsg sanitizes and bounds a status message: at most 120 runes,
// decoded < 2048 bytes.
func SanitizeMsg(msg string) string {
	return SanitizeText(msg, MaxMsgRunes, MaxMsgDec)
}

// Encode produces the raw escape sequence bytes for r.
// Wire form: ESC ] 7501 ; state=<s>:app=<app>[:title=<b64>][:msg=<b64>] ESC \
func Encode(r Report) ([]byte, error) {
	if !isValidState(r.State) {
		return nil, fmt.Errorf("osc7501: invalid or missing state %q", r.State)
	}
	app := r.App
	if app == "" {
		app = "dashy"
	}
	if !isValidApp(app) {
		return nil, fmt.Errorf("osc7501: invalid app identifier %q", app)
	}

	var buf bytes.Buffer
	buf.WriteString("\x1b]7501;state=")
	buf.WriteString(r.State)
	buf.WriteString(":app=")
	buf.WriteString(app)

	if r.Title != "" {
		title := SanitizeTitle(r.Title)
		if title != "" {
			if len(title) >= MaxTitleDec {
				return nil, errors.New("osc7501: title decoded exceeds limit")
			}
			b64 := base64.StdEncoding.EncodeToString([]byte(title))
			if len(b64) > MaxTitleEnc {
				return nil, errors.New("osc7501: title encoded exceeds limit")
			}
			buf.WriteString(":title=")
			buf.WriteString(b64)
		}
	}

	if r.Msg != "" {
		msg := SanitizeMsg(r.Msg)
		if msg != "" {
			if len(msg) >= MaxMsgDec {
				return nil, errors.New("osc7501: msg decoded exceeds limit")
			}
			b64 := base64.StdEncoding.EncodeToString([]byte(msg))
			if len(b64) > MaxMsgEnc {
				return nil, errors.New("osc7501: msg encoded exceeds limit")
			}
			buf.WriteString(":msg=")
			buf.WriteString(b64)
		}
	}

	buf.WriteString("\x1b\\")
	res := buf.Bytes()
	if len(res) > MaxSequenceLen {
		return nil, errors.New("osc7501: report exceeds maximum sequence length")
	}
	return res, nil
}

type fd interface {
	Fd() uintptr
}

func isTerminal(w io.Writer) bool {
	termEnv := os.Getenv("TERM")
	if termEnv == "" || termEnv == "dumb" {
		return false
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	f, ok := w.(fd)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// Option configures Reporter.
type Option func(*Reporter)

// WithEnabled forces reporting enabled or disabled regardless of TTY detection.
func WithEnabled(enabled bool) Option {
	return func(r *Reporter) {
		r.enabled = enabled
	}
}

// WithInterval configures the rate-limiting interval between reports.
func WithInterval(d time.Duration) Option {
	return func(r *Reporter) {
		r.interval = d
	}
}

// WithApp configures the application identifier.
func WithApp(app string) Option {
	return func(r *Reporter) {
		r.app = app
	}
}

// Reporter manages OSC 7501 emission for a terminal session.
type Reporter struct {
	mu           sync.Mutex
	out          io.Writer
	enabled      bool
	closed       bool
	interval     time.Duration
	timer        *time.Timer
	lastSent     []byte
	lastSentTime time.Time
	pending      []byte

	state            string
	app              string
	title            string
	firstUserMsg     string
	titleCustom      bool
	msg              string
	currentAssistant string
}

// New returns a Reporter for out.
func New(out io.Writer, opts ...Option) *Reporter {
	r := &Reporter{
		out:      out,
		interval: 100 * time.Millisecond,
		app:      "dashy",
		state:    "idle",
		enabled:  isTerminal(out),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func firstNonEmptyLine(s string) string {
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			return line
		}
	}
	return strings.TrimSpace(s)
}

// HandleEvent updates the reporter state based on controller events.
func (r *Reporter) HandleEvent(e control.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch e.Kind {
	case "started":
		r.currentAssistant = ""
		if e.Operation == "compact" {
			r.state = "working"
			r.msg = "Compacting"
		} else {
			r.state = "working"
			firstLine := firstNonEmptyLine(e.Text)
			r.msg = firstLine
			if r.title == "" && firstLine != "" && !r.titleCustom {
				r.title = firstLine
				r.firstUserMsg = firstLine
			}
		}
		r.emitLocked()

	case "agent":
		if e.Agent == nil {
			return
		}
		switch e.Agent.Kind {
		case "user":
			firstLine := firstNonEmptyLine(e.Agent.Text)
			if r.firstUserMsg == "" && firstLine != "" {
				r.firstUserMsg = firstLine
			}
			if r.title == "" && firstLine != "" && !r.titleCustom {
				r.title = firstLine
			}
			if r.state == "working" && r.msg == "" {
				r.msg = firstLine
				r.emitLocked()
			}
		case "notice":
			if strings.HasPrefix(e.Agent.Text, "Retrying") {
				r.state = "working"
				r.emitLocked()
			}
		case "delta":
			r.currentAssistant += e.Agent.Text
		case "assistant":
			r.currentAssistant = e.Agent.Text
		}

	case "notice":
		if strings.HasPrefix(e.Text, "Session renamed: ") {
			name := strings.TrimPrefix(e.Text, "Session renamed: ")
			r.title = name
			r.titleCustom = true
			r.emitLocked()
		} else if e.Text == "Session title reset to the first message." {
			r.titleCustom = false
			r.title = r.firstUserMsg
			r.emitLocked()
		}

	case "error":
		r.state = "error"
		r.msg = e.Text
		r.emitLocked()

	case "done":
		switch e.Status {
		case "cancelled":
			r.state = "idle"
			r.msg = ""
			r.emitLocked()
		case "error":
			r.state = "error"
			errText := e.Error
			if errText == "" {
				errText = e.Message
			}
			r.msg = errText
			r.emitLocked()
		case "ok", "":
			if e.Operation == "compact" {
				r.state = "idle"
				r.msg = ""
			} else {
				r.state = "done"
				r.msg = firstNonEmptyLine(r.currentAssistant)
			}
			r.emitLocked()
		}

	case "session":
		if t, ok := e.Session["title"].(string); ok && t != "" {
			r.title = t
			r.firstUserMsg = t
			r.titleCustom = true
		} else {
			r.title = ""
			r.firstUserMsg = ""
			r.titleCustom = false
			for _, rec := range e.Records {
				if rec.Kind == "user" && strings.TrimSpace(rec.Text) != "" {
					first := firstNonEmptyLine(rec.Text)
					r.title = first
					r.firstUserMsg = first
					break
				}
			}
		}
		if r.state == "" {
			r.state = "idle"
			r.msg = ""
		}
		r.emitLocked()
	}
}

// Report emits an explicit report, applying dedup and rate limits.
func (r *Reporter) Report(rep Report) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	if rep.State != "" {
		r.state = rep.State
	}
	if rep.App != "" {
		r.app = rep.App
	}
	if rep.Title != "" {
		r.title = rep.Title
	}
	r.msg = rep.Msg
	r.emitLocked()
	return nil
}

// Idle sets and emits the idle state.
func (r *Reporter) Idle() {
	_ = r.Report(Report{State: "idle", Msg: ""})
}

// Working sets and emits the working state with msg.
func (r *Reporter) Working(msg string) {
	_ = r.Report(Report{State: "working", Msg: msg})
}

// Done sets and emits the done state with msg.
func (r *Reporter) Done(msg string) {
	_ = r.Report(Report{State: "done", Msg: msg})
}

// Error sets and emits the error state with msg.
func (r *Reporter) Error(msg string) {
	_ = r.Report(Report{State: "error", Msg: msg})
}

// SetTitle updates the session title and emits a report if changed.
func (r *Reporter) SetTitle(title string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.title == title {
		return
	}
	r.title = title
	r.titleCustom = true
	r.emitLocked()
}

// Title returns the current session title.
func (r *Reporter) Title() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.title
}

// State returns the current reporter state.
func (r *Reporter) State() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *Reporter) emitLocked() {
	if r.closed || !r.enabled {
		return
	}
	rep := Report{
		State: r.state,
		App:   r.app,
		Title: r.title,
		Msg:   r.msg,
	}
	data, err := Encode(rep)
	if err != nil {
		return
	}
	r.sendLocked(data)
}

func (r *Reporter) writeLocked(data []byte) {
	if r.out != nil && len(data) > 0 {
		_, _ = r.out.Write(data)
	}
}

func (r *Reporter) sendLocked(data []byte) {
	if r.closed || !r.enabled {
		return
	}
	if bytes.Equal(data, r.lastSent) {
		return
	}
	now := time.Now()
	elapsed := now.Sub(r.lastSentTime)
	if elapsed >= r.interval && r.timer == nil {
		r.writeLocked(data)
		r.lastSent = data
		r.lastSentTime = now
		r.pending = nil
		return
	}
	r.pending = data
	if r.timer == nil {
		delay := r.interval - elapsed
		if delay < 0 {
			delay = 0
		}
		r.timer = time.AfterFunc(delay, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.timer = nil
			if r.closed || !r.enabled || r.pending == nil {
				return
			}
			if bytes.Equal(r.pending, r.lastSent) {
				r.pending = nil
				return
			}
			toSend := r.pending
			r.pending = nil
			r.writeLocked(toSend)
			r.lastSent = toSend
			r.lastSentTime = time.Now()
		})
	}
}

// Close flushes final idle state and terminates the reporter.
func (r *Reporter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if !r.enabled {
		return nil
	}
	r.state = "idle"
	r.msg = ""
	idleRep := Report{
		State: "idle",
		App:   r.app,
		Title: r.title,
		Msg:   "",
	}
	data, err := Encode(idleRep)
	if err == nil && !bytes.Equal(data, r.lastSent) {
		r.writeLocked(data)
		r.lastSent = data
		r.lastSentTime = time.Now()
	}
	return nil
}
