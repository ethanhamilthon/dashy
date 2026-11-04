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
		!strings.Contains(text, "20 of 30 bytes omitted (shell_output=10, first 5 + last 5 kept)") {
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

func TestOutputBufferKeepsMultibyteStraddlingHeadTailSplit(t *testing.T) {
	for _, tc := range []struct {
		limit int
		text  string
	}{
		{4, "a€"},  // at the limit; € crosses byte 2
		{5, "ab€"}, // at the limit; € crosses byte 2
		{6, "ab€"}, // below the limit; € crosses byte 3
		{8192, strings.Repeat("x", 4095) + "€"}, // default limit; € crosses byte 4096
	} {
		whole := NewOutputBuffer(tc.limit)
		whole.Write([]byte(tc.text))
		chunked := NewOutputBuffer(tc.limit)
		for _, c := range []byte(tc.text) {
			chunked.Write([]byte{c})
		}
		if whole.Text() != tc.text || chunked.Text() != tc.text {
			t.Fatalf("limit %d: whole = %q, chunked = %q, want %q", tc.limit, whole.Text(), chunked.Text(), tc.text)
		}
	}
}
