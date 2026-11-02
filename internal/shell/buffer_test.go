package shell

import (
	"strings"
	"testing"
)

func TestOutputBufferKeepsHeadAndTail(t *testing.T) {
	b := NewOutputBuffer(10)
	for _, chunk := range []string{"0123", "456789", "abcdefghij", "klmnopqrst"} {
		b.Write([]byte(chunk))
	}
	text := b.Text()
	if !strings.HasPrefix(text, "01234") || !strings.HasSuffix(text, "pqrst") ||
		!strings.Contains(text, "20 of 30 bytes omitted (max_output=10, first 5 + last 5 kept)") {
		t.Fatalf("text = %q", text)
	}
}

func TestOutputBufferWithinLimitAndUnlimitedAreExact(t *testing.T) {
	short := NewOutputBuffer(64)
	short.Write([]byte("short output"))
	unlimited := NewOutputBuffer(0)
	long := strings.Repeat("x", 5000)
	unlimited.Write([]byte(long))
	if short.Text() != "short output" || unlimited.Text() != long {
		t.Fatal("output within the limit or unlimited output was changed")
	}
}
