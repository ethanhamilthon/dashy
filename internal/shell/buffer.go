package shell

import (
	"fmt"
	"strings"
)

// outputBuffer captures UTF-8 tool output. With a positive limit it keeps the
// first and last limit/2 bytes; otherwise it keeps everything.
type outputBuffer struct {
	limit      int
	head, tail []byte
	size       int
}

// newOutputBuffer keeps the first and last limit/2 bytes, or everything when limit <= 0.
func newOutputBuffer(limit int) *outputBuffer { return &outputBuffer{limit: limit} }

func (b *outputBuffer) Write(p []byte) (int, error) {
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

func (b *outputBuffer) Text() string {
	if b.limit <= 0 || b.size <= b.limit {
		// Nothing was omitted: normalize the whole output so that a character
		// straddling the head/tail split is kept intact.
		return strings.ToValidUTF8(string(b.head)+string(b.tail), "")
	}
	head, tail := strings.ToValidUTF8(string(b.head), ""), strings.ToValidUTF8(string(b.tail), "")
	return head + fmt.Sprintf("\n[output truncated: %d of %d bytes omitted (shell_output=%d, first %d + last %d kept); "+
		"filter with rg, grep, head or tail]\n",
		b.size-len(b.head)-len(b.tail), b.size, b.limit, len(b.head), len(b.tail)) + tail
}
