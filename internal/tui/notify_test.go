package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func captureTerm(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	termOut = &out
	t.Cleanup(func() { termOut = nil })
	return &out
}

func TestTermStateTurn(t *testing.T) {
	out := captureTerm(t)
	var ts termState
	ts.start("")
	if got := out.String(); got != "\x1b]9;4;3\x07\x1b]2;⠋ dashy\x07" {
		t.Fatalf("start = %q", got)
	}
	out.Reset()
	ts.rename("fix\nthe; bug")
	ts.rename("fix\nthe; bug")
	ts.tick()
	if got := out.String(); got != "\x1b]2;⠋ fix the bug\x07\x1b]2;⠙ fix the bug\x07" {
		t.Fatalf("rename/tick = %q", got)
	}
	out.Reset()
	ts.finish("ok", "fix the bug")
	if got := out.String(); got != "\x1b]9;4;0\x07\x1b]2;✓ fix the bug\x07" {
		t.Fatalf("quick done should not notify: %q", got)
	}
	out.Reset()
	ts.start("fix the bug")
	ts.started = time.Now().Add(-notifyAfter)
	out.Reset()
	ts.finish("error", "fix the bug")
	got := out.String()
	for _, want := range []string{"\x1b]9;4;2;100\x07", "✗ fix the bug", "\x1b]9;fix the bug: failed\x07", "\x1b]777;notify;fix the bug;failed\x07", "\x07\x07"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error finish missing %q in %q", want, got)
		}
	}
	out.Reset()
	ts.reset()
	if got := out.String(); got != "\x1b]9;4;0\x07\x1b]2;\x07" {
		t.Fatalf("reset = %q", got)
	}
}

func TestTermStateCancelAndUnused(t *testing.T) {
	out := captureTerm(t)
	var unused termState
	unused.reset()
	if out.Len() != 0 {
		t.Fatalf("unused reset wrote %q", out.String())
	}
	var ts termState
	ts.start("x")
	ts.started = time.Now().Add(-time.Hour)
	out.Reset()
	ts.finish("cancelled", "x")
	if got := out.String(); got != "\x1b]9;4;0\x07\x1b]2;x\x07" {
		t.Fatalf("cancel = %q", got)
	}
}

func TestTermTitleTruncates(t *testing.T) {
	if got := termTitle(strings.Repeat("я", 50)); len([]rune(got)) != 40 || !strings.HasSuffix(got, "…") {
		t.Fatalf("termTitle = %q", got)
	}
}
