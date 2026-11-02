package shell

import (
	"fmt"
	"strings"
)

const (
	// DefaultOutputLimit caps a tool call result when the agent does not choose a limit.
	DefaultOutputLimit = 8 * 1024
	// NoOutputLimit keeps the complete output (used for the prefix and max_output 0).
	NoOutputLimit = 0
)

// cappedBuffer captures UTF-8 tool output. With a positive limit it keeps the
// first and last limit/2 bytes; with NoOutputLimit it keeps everything.
type cappedBuffer struct {
	limit      int
	head, tail []byte
	size       int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	total := len(p)
	b.size += total
	if b.limit <= 0 {
		b.head = append(b.head, p...)
		return total, nil
	}
	half := b.limit / 2
	if room := half - len(b.head); room > 0 {
		n := min(len(p), room)
		b.head, p = append(b.head, p[:n]...), p[n:]
	}
	b.tail = append(b.tail, p...)
	if keep := b.limit - half; len(b.tail) > keep {
		copy(b.tail[:keep], b.tail[len(b.tail)-keep:])
		b.tail = b.tail[:keep]
	}
	return total, nil
}

// OutputBuffer is a capped capture of command output, shared with hooks.
type OutputBuffer = cappedBuffer

// NewOutputBuffer keeps the first and last limit/2 bytes, or everything when limit <= 0.
func NewOutputBuffer(limit int) *OutputBuffer { return &cappedBuffer{limit: limit} }

// Truncated reports whether any bytes were omitted.
func (b *cappedBuffer) Truncated() bool { return b.limit > 0 && b.size > b.limit }

func (b *cappedBuffer) Text() string {
	head, tail := strings.ToValidUTF8(string(b.head), ""), strings.ToValidUTF8(string(b.tail), "")
	if b.limit <= 0 || b.size <= b.limit {
		return head + tail
	}
	return head + fmt.Sprintf("\n[output truncated: %d of %d bytes omitted (max_output=%d, first %d + last %d kept); "+
		"filter with rg, grep, head, tail or pass a larger max_output]\n",
		b.size-len(b.head)-len(b.tail), b.size, b.limit, len(b.head), len(b.tail)) + tail
}
