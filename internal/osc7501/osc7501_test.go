package osc7501

import (
	"bytes"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"dashy/internal/agent"
	"dashy/internal/control"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// The protocol example, in dashy's app name.
func TestEncodeMatchesProtocolForm(t *testing.T) {
	got, err := Encode("working", "", "Installing updates")
	want := "\x1b]7501;state=working:app=dashy:msg=" + b64("Installing updates") + "\x1b\\"
	if err != nil || string(got) != want {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, _ := Encode("idle", "", ""); string(got) != "\x1b]7501;state=idle:app=dashy\x1b\\" {
		t.Fatalf("empty msg and title must be omitted: %q", got)
	}
	if got, _ := Encode("idle", "Work", ""); !strings.Contains(string(got), ":title="+b64("Work")) {
		t.Fatalf("title missing: %q", got)
	}
}

// Control characters and whitespace runs become single spaces; long text is cut on a rune boundary.
func TestTextIsSanitizedAndBounded(t *testing.T) {
	got, _ := Encode("done", "", "a\x07b\n\tc  \u0085 d")
	if !strings.HasSuffix(string(got), ":msg="+b64("a b c d")+"\x1b\\") {
		t.Fatalf("sanitized msg: %q", got)
	}
	long := strings.Repeat("é", 300)
	got, _ = Encode("done", strings.Repeat("ß", 300), long)
	msg := decodedField(t, got, "msg")
	title := decodedField(t, got, "title")
	if !utf8.ValidString(msg) || utf8.RuneCountInString(msg) != 120 || !strings.HasSuffix(msg, "…") || len(msg) >= 2048 {
		t.Fatalf("msg %d runes", utf8.RuneCountInString(msg))
	}
	if utf8.RuneCountInString(title) != 60 || len(title) >= 192 {
		t.Fatalf("title %d runes", utf8.RuneCountInString(title))
	}
	if len(got) > MaxSequence {
		t.Fatalf("sequence %d bytes", len(got))
	}
}

func decodedField(t *testing.T, seq []byte, key string) string {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimPrefix(string(seq), "\x1b]7501;"), "\x1b\\")
	for _, pair := range strings.Split(body, ":") {
		if v, ok := strings.CutPrefix(pair, key+"="); ok {
			data, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				t.Fatal(err)
			}
			return string(data)
		}
	}
	return ""
}

// syncBuffer records what the reporter writes, safely from its timer goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// lastState returns the state of the last sequence written, or "" if none.
func lastState(out string) string {
	i := strings.LastIndex(out, "state=")
	if i < 0 {
		return ""
	}
	state, _, _ := strings.Cut(out[i+len("state="):], ":")
	return strings.TrimSuffix(state, "\x1b\\")
}

// The state mapping, driven by the events the controller emits.
func TestReporterMapsControllerEvents(t *testing.T) {
	out := &syncBuffer{}
	r := New(out, true)
	r.HandleEvent(control.Event{Kind: "session", Title: "Chat"})
	r.HandleEvent(control.Event{Kind: "started", Operation: "turn", Text: "build it\nnow"})
	time.Sleep(interval + 20*time.Millisecond) // reports inside one interval are coalesced
	if got := lastState(out.String()); got != "working" || !strings.Contains(out.String(), b64("build it")) {
		t.Fatalf("started: %q", out.String())
	}
	r.HandleEvent(control.Event{Kind: "agent", Agent: &agent.Event{Kind: "assistant", Text: "All done.\nSee below."}})
	r.HandleEvent(control.Event{Kind: "done", Operation: "turn", Status: "ok"})
	time.Sleep(interval + 20*time.Millisecond)
	if !strings.HasSuffix(out.String(), b64("All done.")+"\x1b\\") || lastState(out.String()) != "done" {
		t.Fatalf("done: %q", out.String())
	}
	r.HandleEvent(control.Event{Kind: "started", Operation: "turn", Text: "again"})
	r.HandleEvent(control.Event{Kind: "done", Operation: "turn", Status: "error", Error: "provider failed"})
	time.Sleep(interval + 20*time.Millisecond)
	if lastState(out.String()) != "error" || !strings.Contains(out.String(), b64("provider failed")) {
		t.Fatalf("error: %q", out.String())
	}
	r.Close()
	if lastState(out.String()) != "idle" {
		t.Fatalf("close must deliver idle: %q", out.String())
	}
}

// A switch to another session reports idle with no message: the previous answer
// or error does not stay on the record.
func TestSessionSwitchForgetsPreviousResult(t *testing.T) {
	out := &syncBuffer{}
	r := New(out, true)
	r.HandleEvent(control.Event{Kind: "done", Operation: "turn", Status: "error", Error: "old failure"})
	time.Sleep(120 * time.Millisecond)
	r.HandleEvent(control.Event{Kind: "session", Title: "New one"})
	time.Sleep(120 * time.Millisecond)
	last := out.String()
	if lastState(last) != "idle" || decodedField(t, []byte(last[strings.LastIndex(last, "\x1b]7501"):]), "msg") != "" {
		t.Fatalf("after switch: %q", last)
	}
	if !strings.Contains(last, b64("New one")) {
		t.Fatal("new title not reported")
	}
}

// Idle (sent) -> working (queued) -> idle inside one interval: the latest state
// wins, and no working report is delivered later.
func TestBurstEndingIdleNeverDeliversStaleWorking(t *testing.T) {
	out := &syncBuffer{}
	r := New(out, true)
	r.HandleEvent(control.Event{Kind: "session"})
	r.HandleEvent(control.Event{Kind: "started", Operation: "turn", Text: "task"})
	r.HandleEvent(control.Event{Kind: "done", Operation: "turn", Status: "cancelled"})
	time.Sleep(250 * time.Millisecond)
	if strings.Contains(out.String(), "state=working") && lastState(out.String()) != "idle" {
		t.Fatalf("stale working delivered: %q", out.String())
	}
	if n := strings.Count(out.String(), "\x1b]7501;"); n != 1 {
		t.Fatalf("expected only the first idle report, got %d writes", n)
	}
}

// A burst ending in done reports done (the latest state), once.
func TestBurstDeliversLatestState(t *testing.T) {
	out := &syncBuffer{}
	r := New(out, true)
	r.HandleEvent(control.Event{Kind: "session"})
	r.HandleEvent(control.Event{Kind: "started", Operation: "turn", Text: "x"})
	r.HandleEvent(control.Event{Kind: "done", Operation: "turn", Status: "error", Error: "e"})
	time.Sleep(250 * time.Millisecond)
	if lastState(out.String()) != "error" {
		t.Fatalf("latest state not delivered: %q", out.String())
	}
}

// A disabled reporter (piped output, -p, TERM=dumb) writes nothing at all.
func TestDisabledReporterWritesNothing(t *testing.T) {
	out := &syncBuffer{}
	r := New(out, false)
	r.HandleEvent(control.Event{Kind: "started", Operation: "turn", Text: "x"})
	r.Close()
	if out.String() != "" {
		t.Fatalf("wrote %q", out.String())
	}
}
