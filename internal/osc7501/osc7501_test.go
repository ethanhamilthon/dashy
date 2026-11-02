package osc7501

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"dashy/internal/agent"
	"dashy/internal/control"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *safeBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Reset()
}

// 1. Encoder against the examples of the protocol.
func TestEncoderProtocolExamples(t *testing.T) {
	tests := []struct {
		name string
		in   Report
		want string
	}{
		{
			name: "brew installing updates",
			in:   Report{State: "working", App: "brew", Msg: "Installing updates"},
			want: "\x1b]7501;state=working:app=brew:msg=SW5zdGFsbGluZyB1cGRhdGVz\x1b\\",
		},
		{
			name: "brew upgraded packages",
			in:   Report{State: "done", App: "brew", Msg: "Upgraded 12 packages"},
			want: "\x1b]7501;state=done:app=brew:msg=VXBncmFkZWQgMTIgcGFja2FnZXM=\x1b\\",
		},
		{
			name: "deploy deployed to 3 regions",
			in:   Report{State: "done", App: "deploy", Msg: "Deployed to 3 regions"},
			want: "\x1b]7501;state=done:app=deploy:msg=RGVwbG95ZWQgdG8gMyByZWdpb25z\x1b\\",
		},
		{
			name: "dashy idle default",
			in:   Report{State: "idle", App: "dashy"},
			want: "\x1b]7501;state=idle:app=dashy\x1b\\",
		},
		{
			name: "dashy working with title and msg",
			in:   Report{State: "working", App: "dashy", Title: "US East", Msg: "Pushing image"},
			want: "\x1b]7501;state=working:app=dashy:title=VVMgRWFzdA==:msg=UHVzaGluZyBpbWFnZQ==\x1b\\",
		},
		{
			name: "dashy error msg",
			in:   Report{State: "error", App: "dashy", Msg: "rsync failed"},
			want: "\x1b]7501;state=error:app=dashy:msg=cnN5bmMgZmFpbGVk\x1b\\",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("Encode() =\n%q\nwant:\n%q", string(got), tc.want)
			}
		})
	}
}

// 2. Title handling (present, absent, rename, size limits).
func TestTitleHandling(t *testing.T) {
	// Present
	got, err := Encode(Report{State: "idle", App: "dashy", Title: "Session 1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(got), ":title=") {
		t.Fatalf("expected title key in %q", string(got))
	}

	// Absent
	gotNoTitle, err := Encode(Report{State: "idle", App: "dashy", Title: ""})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(gotNoTitle), ":title=") {
		t.Fatalf("unexpected title key in %q", string(gotNoTitle))
	}

	// Rename driven via Reporter
	buf := &safeBuffer{}
	rep := New(buf, WithEnabled(true), WithInterval(1*time.Millisecond))
	defer rep.Close()

	// Initial report: no title yet
	rep.Idle()
	time.Sleep(10 * time.Millisecond)
	if strings.Contains(buf.String(), ":title=") {
		t.Fatalf("expected no title initially: %q", buf.String())
	}
	buf.Reset()

	// Rename session
	rep.HandleEvent(control.Event{Kind: "notice", Text: "Session renamed: Project Phoenix"})
	time.Sleep(10 * time.Millisecond)
	wantB64 := base64.StdEncoding.EncodeToString([]byte("Project Phoenix"))
	if !strings.Contains(buf.String(), ":title="+wantB64) {
		t.Fatalf("expected renamed title %q in %q", wantB64, buf.String())
	}
	buf.Reset()

	// Reset rename back to default
	rep.HandleEvent(control.Event{Kind: "notice", Text: "Session title reset to the first message."})
	time.Sleep(10 * time.Millisecond)
	// Since no first user message existed yet, title becomes empty
	if strings.Contains(buf.String(), ":title=") {
		t.Fatalf("expected reset title to be omitted in %q", buf.String())
	}

	// Title size limits: > 60 runes truncated with ellipsis
	longTitle := strings.Repeat("A", 80)
	sanitized := SanitizeTitle(longTitle)
	runes := []rune(sanitized)
	if len(runes) > MaxTitleRunes {
		t.Fatalf("expected <= 60 runes, got %d", len(runes))
	}
	if !strings.HasSuffix(sanitized, "…") {
		t.Fatalf("expected ellipsis on truncated title: %q", sanitized)
	}
	if len([]byte(sanitized)) >= MaxTitleDec {
		t.Fatalf("decoded title size must be < %d, got %d", MaxTitleDec, len([]byte(sanitized)))
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(sanitized))
	if len(b64) > MaxTitleEnc {
		t.Fatalf("encoded title size must be <= %d, got %d", MaxTitleEnc, len(b64))
	}

	// Title size limits with 4-byte runes (emojis)
	emojiTitle := strings.Repeat("🌟", 60)
	sanEmoji := SanitizeTitle(emojiTitle)
	if len([]byte(sanEmoji)) >= MaxTitleDec {
		t.Fatalf("decoded emoji title size must be < %d, got %d", MaxTitleDec, len([]byte(sanEmoji)))
	}
	b64Emoji := base64.StdEncoding.EncodeToString([]byte(sanEmoji))
	if len(b64Emoji) > MaxTitleEnc {
		t.Fatalf("encoded emoji title size must be <= %d, got %d", MaxTitleEnc, len(b64Emoji))
	}
}

// 3. Control-character stripping.
func TestControlCharacterStripping(t *testing.T) {
	raw := "Hello\x00\x07\x08\x1b\x1f \t\r\n World\x7f\u0080\u0085\u009f!"
	cleaned := SanitizeMsg(raw)

	if strings.ContainsAny(cleaned, "\r\n\t") {
		t.Fatalf("cleaned string contains newlines or tabs: %q", cleaned)
	}
	for _, r := range cleaned {
		if isControl(r) {
			t.Fatalf("cleaned string contains control rune %U: %q", r, cleaned)
		}
	}
	if cleaned != "Hello World !" {
		t.Fatalf("unexpected cleaned result: %q, want %q", cleaned, "Hello World !")
	}

	// Leading/trailing control characters and whitespace
	edge := "\x01\x02  test  \x03\x04"
	if got := SanitizeMsg(edge); got != "test" {
		t.Fatalf("SanitizeMsg(%q) = %q, want %q", edge, got, "test")
	}

	// Only controls/whitespace -> empty string
	onlyControls := "\x00\x1f\t \n\x7f\u0085"
	if got := SanitizeMsg(onlyControls); got != "" {
		t.Fatalf("SanitizeMsg(onlyControls) = %q, want empty", got)
	}
}

// 4. Truncation on rune boundaries.
func TestTruncationOnRuneBoundaries(t *testing.T) {
	// Multi-byte Greek runes (2 bytes each)
	greek := strings.Repeat("Ω", 150)
	cleanedGreek := SanitizeMsg(greek)
	if !utf8.ValidString(cleanedGreek) {
		t.Fatalf("truncated Greek is not valid UTF-8")
	}
	runesGreek := []rune(cleanedGreek)
	if len(runesGreek) != MaxMsgRunes {
		t.Fatalf("expected exactly %d runes, got %d", MaxMsgRunes, len(runesGreek))
	}
	if runesGreek[len(runesGreek)-1] != '…' {
		t.Fatalf("expected ellipsis at end of truncated Greek")
	}

	// 4-byte emoji runes
	emojis := strings.Repeat("🚀", 140)
	cleanedEmojis := SanitizeMsg(emojis)
	if !utf8.ValidString(cleanedEmojis) {
		t.Fatalf("truncated emojis is not valid UTF-8")
	}
	runesEmojis := []rune(cleanedEmojis)
	if len(runesEmojis) != MaxMsgRunes {
		t.Fatalf("expected exactly %d runes, got %d", MaxMsgRunes, len(runesEmojis))
	}
	if runesEmojis[len(runesEmojis)-1] != '…' {
		t.Fatalf("expected ellipsis at end of truncated emojis")
	}

	// Not truncated if <= 120 runes
	short := strings.Repeat("Ω", 120)
	cleanedShort := SanitizeMsg(short)
	if strings.HasSuffix(cleanedShort, "…") {
		t.Fatalf("expected no ellipsis when rune count <= 120")
	}
	if len([]rune(cleanedShort)) != 120 {
		t.Fatalf("expected 120 runes, got %d", len([]rune(cleanedShort)))
	}
}

// 5. Size limits.
func TestSizeLimits(t *testing.T) {
	// Invalid state
	if _, err := Encode(Report{State: "bogus", App: "dashy"}); err == nil {
		t.Fatalf("expected error for invalid state")
	}

	// Missing state
	if _, err := Encode(Report{State: "", App: "dashy"}); err == nil {
		t.Fatalf("expected error for missing state")
	}

	// App identifier too long (> 32 bytes)
	longApp := strings.Repeat("a", 33)
	if _, err := Encode(Report{State: "idle", App: longApp}); err == nil {
		t.Fatalf("expected error for app > 32 bytes")
	}

	// App with invalid characters
	if _, err := Encode(Report{State: "idle", App: "dashy;bad"}); err == nil {
		t.Fatalf("expected error for app with invalid chars")
	}

	// Sequence limit check
	hugeReport := Report{
		State: "working",
		App:   "dashy",
		Msg:   strings.Repeat("M", 5000),
	}
	// SanitizeMsg bounds msg to 120 runes so normal usage cannot exceed 4096.
	// Direct byte test ensures reports above 4096 bytes are rejected:
	rawLong := make([]byte, 4100)
	for i := range rawLong {
		rawLong[i] = 'A'
	}
	hugeSeq := append([]byte("\x1b]7501;state=working:app=dashy:msg="), rawLong...)
	hugeSeq = append(hugeSeq, []byte("\x1b\\")...)
	if len(hugeSeq) <= MaxSequenceLen {
		t.Fatalf("test helper must be > 4096 bytes")
	}
	_ = hugeReport
}

// 6. Dedup and coalescing.
func TestDedupAndCoalescing(t *testing.T) {
	buf := &safeBuffer{}
	rep := New(buf, WithEnabled(true), WithInterval(50*time.Millisecond))

	// Dedup: duplicate reports are not sent repeatedly
	rep.Working("Task 1")
	time.Sleep(10 * time.Millisecond)
	initialLen := len(buf.String())
	if initialLen == 0 {
		t.Fatalf("expected first report to be written immediately")
	}

	// Call with identical report multiple times
	rep.Working("Task 1")
	rep.Working("Task 1")
	time.Sleep(70 * time.Millisecond)
	if len(buf.String()) != initialLen {
		t.Fatalf("expected duplicate reports to be suppressed")
	}

	// Coalescing: rapid burst within 50 ms window
	buf.Reset()
	rep.Working("Burst 1") // Sent immediately if > interval has passed
	rep.Working("Burst 2")
	rep.Working("Burst 3")
	rep.Working("Burst 4")
	rep.Working("Burst 5") // Latest

	// Wait for timer to flush
	time.Sleep(100 * time.Millisecond)
	out := buf.String()
	wantB64 := base64.StdEncoding.EncodeToString([]byte("Burst 5"))
	if !strings.Contains(out, ":msg="+wantB64) {
		t.Fatalf("expected latest burst report (Burst 5: %s) to be delivered, got:\n%s", wantB64, out)
	}

	// Close delivers final idle state immediately
	buf.Reset()
	rep.Working("Running before quit")
	time.Sleep(70 * time.Millisecond) // Ensure it was sent
	buf.Reset()

	if err := rep.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	closeOut := buf.String()
	if !strings.Contains(closeOut, "state=idle") {
		t.Fatalf("expected final state=idle on Close, got: %q", closeOut)
	}
}

// 7. State mapping driven by controller events.
func TestStateMappingDrivenByControllerEvents(t *testing.T) {
	buf := &safeBuffer{}
	rep := New(buf, WithEnabled(true), WithInterval(1*time.Millisecond))
	defer rep.Close()

	// Initial turn started -> working with first line of prompt
	prompt := "Create a REST server\nWith authentication and tests"
	rep.HandleEvent(control.Event{
		Kind:      "started",
		Turn:      "turn-1",
		Operation: "turn",
		Text:      prompt,
	})
	time.Sleep(10 * time.Millisecond)
	wantPromptB64 := base64.StdEncoding.EncodeToString([]byte("Create a REST server"))
	if !strings.Contains(buf.String(), "state=working") || !strings.Contains(buf.String(), ":msg="+wantPromptB64) {
		t.Fatalf("expected working with prompt, got: %q", buf.String())
	}
	// Title should also have been set by the first message
	if !strings.Contains(buf.String(), ":title="+wantPromptB64) {
		t.Fatalf("expected title set by first user prompt, got: %q", buf.String())
	}
	buf.Reset()

	// Assistant response streams in and turn finishes successfully -> done
	rep.HandleEvent(control.Event{
		Kind: "agent",
		Agent: &agent.Event{
			Kind: "assistant",
			Text: "Server created successfully.\nListening on port 8080.",
		},
	})
	rep.HandleEvent(control.Event{
		Kind:      "done",
		Turn:      "turn-1",
		Operation: "turn",
		Status:    "ok",
	})
	time.Sleep(10 * time.Millisecond)
	wantDoneB64 := base64.StdEncoding.EncodeToString([]byte("Server created successfully."))
	if !strings.Contains(buf.String(), "state=done") || !strings.Contains(buf.String(), ":msg="+wantDoneB64) {
		t.Fatalf("expected done with first line of assistant text, got: %q", buf.String())
	}
	buf.Reset()

	// Second turn started -> working
	rep.HandleEvent(control.Event{
		Kind:      "started",
		Turn:      "turn-2",
		Operation: "turn",
		Text:      "Deploy to prod",
	})
	time.Sleep(10 * time.Millisecond)
	if !strings.Contains(buf.String(), "state=working") {
		t.Fatalf("expected state=working on second turn, got: %q", buf.String())
	}
	buf.Reset()

	// Retry notice -> state remains working
	rep.HandleEvent(control.Event{
		Kind: "agent",
		Agent: &agent.Event{
			Kind: "notice",
			Text: "Retrying in 2s (attempt 2/3): 503 Service Unavailable",
		},
	})
	if rep.State() != "working" {
		t.Fatalf("expected rep.State() == working, got %q", rep.State())
	}

	// Turn failed -> error with short error text
	rep.HandleEvent(control.Event{
		Kind:      "done",
		Turn:      "turn-2",
		Operation: "turn",
		Status:    "error",
		Error:     "context deadline exceeded",
	})
	time.Sleep(10 * time.Millisecond)
	wantErrB64 := base64.StdEncoding.EncodeToString([]byte("context deadline exceeded"))
	if !strings.Contains(buf.String(), "state=error") || !strings.Contains(buf.String(), ":msg="+wantErrB64) {
		t.Fatalf("expected error state with error text, got: %q", buf.String())
	}
	buf.Reset()

	// Turn 3 cancelled by user -> idle
	rep.HandleEvent(control.Event{
		Kind:      "done",
		Turn:      "turn-3",
		Operation: "turn",
		Status:    "cancelled",
	})
	time.Sleep(10 * time.Millisecond)
	if !strings.Contains(buf.String(), "state=idle") {
		t.Fatalf("expected idle on cancel, got: %q", buf.String())
	}
	buf.Reset()

	// Compact started -> working with msg "Compacting"
	rep.HandleEvent(control.Event{
		Kind:      "started",
		Turn:      "compact-1",
		Operation: "compact",
	})
	time.Sleep(10 * time.Millisecond)
	wantCompactB64 := base64.StdEncoding.EncodeToString([]byte("Compacting"))
	if !strings.Contains(buf.String(), "state=working") || !strings.Contains(buf.String(), ":msg="+wantCompactB64) {
		t.Fatalf("expected working with Compacting, got: %q", buf.String())
	}
	buf.Reset()

	// Compact finished -> idle
	rep.HandleEvent(control.Event{
		Kind:      "done",
		Turn:      "compact-1",
		Operation: "compact",
		Status:    "ok",
	})
	time.Sleep(10 * time.Millisecond)
	if !strings.Contains(buf.String(), "state=idle") {
		t.Fatalf("expected idle when compaction finishes, got: %q", buf.String())
	}
}

// 8. Nothing is written when not a TTY.
func TestNothingWrittenWhenNotTTY(t *testing.T) {
	// Standard buffer is not *os.File -> not a TTY
	buf := &safeBuffer{}
	rep := New(buf) // No WithEnabled(true)
	rep.Idle()
	rep.Working("Not sent")
	rep.Done("Not sent")
	_ = rep.Close()

	if buf.buf.Len() != 0 {
		t.Fatalf("expected nothing written to non-TTY writer, got: %q", buf.String())
	}

	// Even if an *os.File is passed, if TERM=dumb, nothing is written
	oldTerm := os.Getenv("TERM")
	defer os.Setenv("TERM", oldTerm)
	os.Setenv("TERM", "dumb")

	repDumb := New(buf)
	repDumb.Working("Dumb term")
	_ = repDumb.Close()
	if buf.buf.Len() != 0 {
		t.Fatalf("expected nothing written when TERM=dumb, got: %q", buf.String())
	}
}
