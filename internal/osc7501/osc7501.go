// Package osc7501 reports dashy's state to the terminal with the Program Status
// Protocol (OSC 7501). The encoder is pure; Reporter maps controller events to
// states, deduplicates them and coalesces bursts.
package osc7501

import (
	"bytes"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mattn/go-isatty"

	"dashy/internal/agent"
	"dashy/internal/control"
)

// Protocol limits. The text limits keep every report far below MaxSequence bytes.
const (
	MaxSequence   = 4096
	maxTitleRunes = 60
	maxTitleBytes = 192 // decoded
	maxMsgRunes   = 120
	maxMsgBytes   = 2048 // decoded
	interval      = 100 * time.Millisecond
)

// clean makes s a single line: control characters and whitespace runs become one
// space, the text is trimmed, and it is cut on a rune boundary to fit both limits.
func clean(s string, maxRunes, maxBytes int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	runes := []rune(s)
	if len(runes) <= maxRunes && len(s) < maxBytes {
		return s
	}
	keep := min(len(runes), maxRunes-1)
	for keep > 0 && len(string(runes[:keep]))+len("…") >= maxBytes {
		keep--
	}
	return string(runes[:keep]) + "…"
}

// Encode returns the sequence for one report: ESC ] 7501 ; state=S:app=dashy[:title=B64][:msg=B64] ESC \.
// Empty title or msg are omitted.
func Encode(state, title, msg string) []byte {
	var b strings.Builder
	b.WriteString("\x1b]7501;state=" + state + ":app=dashy")
	if t := clean(title, maxTitleRunes, maxTitleBytes); t != "" {
		b.WriteString(":title=" + base64.StdEncoding.EncodeToString([]byte(t)))
	}
	if m := clean(msg, maxMsgRunes, maxMsgBytes); m != "" {
		b.WriteString(":msg=" + base64.StdEncoding.EncodeToString([]byte(m)))
	}
	b.WriteString("\x1b\\")
	return []byte(b.String())
}

// Enabled reports whether reports may be written to out: out and stdin must be
// terminals and TERM must not be "dumb". Piped output never gets sequences.
func Enabled(out io.Writer) bool {
	f, ok := out.(*os.File)
	if !ok || os.Getenv("TERM") == "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return isatty.IsTerminal(f.Fd()) && isatty.IsTerminal(os.Stdin.Fd())
}

// Reporter tracks the state of the session and writes changed reports to out.
// Each write is one call, so a sequence never interleaves with a frame.
type Reporter struct {
	mu      sync.Mutex
	out     io.Writer
	enabled bool
	state   string
	msg     string
	title   string
	last    []byte // last transmitted report
	pending []byte // latest report not yet transmitted; nil when none
	lastAt  time.Time
	timer   *time.Timer
}

// New returns a reporter for out. A disabled reporter accepts events and writes nothing.
func New(out io.Writer, enabled bool) *Reporter {
	return &Reporter{out: out, enabled: enabled, state: "idle"}
}

// HandleEvent maps one controller event to a state and reports it when it changed.
func (r *Reporter) HandleEvent(e control.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch e.Kind {
	case "started":
		r.state = "working"
		if e.Operation == "compact" {
			r.msg = "Compacting"
		} else {
			r.msg = agent.FirstLine(e.Text)
		}
	case "agent":
		if e.Agent != nil && e.Agent.Kind == "notice" && strings.HasPrefix(e.Agent.Text, "Retrying") {
			r.state = "working"
		}
	case "error":
		r.state, r.msg = "error", e.Text
	case "done":
		switch {
		case e.Status == "error":
			r.state, r.msg = "error", e.Error
		case e.Status == "cancelled" || e.Operation == "compact":
			r.state, r.msg = "idle", ""
		default:
			r.state, r.msg = "done", agent.FirstLine(e.Text) // e.Text: final assistant text
		}
	case "session":
		r.state, r.msg = "idle", ""
	}
	r.title = e.Title
	r.reportLocked()
}

// reportLocked makes the latest state the pending report. A pending report equal to
// the last transmitted one is dropped, so a superseded state is never sent later.
func (r *Reporter) reportLocked() {
	if !r.enabled {
		return
	}
	data := Encode(r.state, r.title, r.msg)
	if bytes.Equal(data, r.last) {
		r.pending = nil
		return
	}
	r.pending = data
	if r.timer != nil {
		return // the running timer sends the latest pending report
	}
	if wait := interval - time.Since(r.lastAt); wait > 0 {
		r.timer = time.AfterFunc(wait, r.onTimer)
		return
	}
	r.flushLocked()
}

func (r *Reporter) onTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timer = nil
	r.flushLocked()
}

func (r *Reporter) flushLocked() {
	if r.pending == nil {
		return
	}
	data := r.pending
	r.pending = nil
	_, _ = r.out.Write(data)
	r.last, r.lastAt = data, time.Now()
}

// Close delivers the final idle state and stops the reporter.
func (r *Reporter) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if !r.enabled {
		return
	}
	r.state, r.msg = "idle", ""
	r.pending = nil // the final state replaces whatever was queued, even when it is not sent
	if data := Encode(r.state, r.title, r.msg); !bytes.Equal(data, r.last) {
		r.pending = data
		// The final report obeys the same rate limit: wait out the rest of the interval.
		if wait := interval - time.Since(r.lastAt); wait > 0 {
			time.Sleep(wait)
		}
	}
	r.flushLocked()
	r.enabled = false
}
