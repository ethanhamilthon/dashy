package shell

import (
	"strings"
	"testing"
)

func TestOutputBufferKeepsHeadAndTail(t *testing.T) {
	b := NewOutputBuffer(10)
	for _, chunk := range []string{"0123", "456789", "abcdefghij", "klmnopqrst"} {
		if _, err := b.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	text := b.Text()
	if !b.Truncated() || !strings.HasPrefix(text, "01234") || !strings.HasSuffix(text, "pqrst") {
		t.Fatalf("text = %q, truncated = %v", text, b.Truncated())
	}
	if !strings.Contains(text, "[output truncated: 20 of 30 bytes omitted (max_output=10, first 5 + last 5 kept)") {
		t.Fatalf("missing truncation notice: %q", text)
	}
}

func TestOutputBufferWithinLimitIsExact(t *testing.T) {
	for _, limit := range []int{0, 64} {
		b := NewOutputBuffer(limit)
		b.Write([]byte("short output"))
		if b.Truncated() || b.Text() != "short output" {
			t.Fatalf("limit %d: text = %q, truncated = %v", limit, b.Text(), b.Truncated())
		}
	}
}

func TestOutputBufferUnlimitedKeepsEverything(t *testing.T) {
	b := NewOutputBuffer(0)
	long := strings.Repeat("x", 5000)
	b.Write([]byte(long))
	if b.Truncated() || b.Text() != long {
		t.Fatal("unlimited buffer dropped output")
	}
}

func TestOutputBufferCutsOnlyValidUTF8(t *testing.T) {
	b := NewOutputBuffer(4)
	b.Write([]byte(strings.Repeat("日", 10))) // 3 bytes per rune
	text := b.Text()
	if !b.Truncated() || !strings.Contains(text, "truncated") {
		t.Fatalf("text = %q", text)
	}
	head, _, _ := strings.Cut(text, "\n[output truncated")
	tail := text[strings.LastIndex(text, "]\n")+2:]
	if strings.ContainsRune(head+tail, '\uFFFD') || strings.ToValidUTF8(head+tail, "") != head+tail {
		t.Fatalf("invalid UTF-8 in kept bytes: %q", text)
	}
}
