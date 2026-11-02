package shell

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// MaxLogSize bounds a diagnostic log; the previous generation is kept as path+".1".
const MaxLogSize = 1 << 20

// AppendLog writes one timestamped line to path. When the file exceeds
// MaxLogSize it is rotated first, so dashy's logs never grow without bound.
func AppendLog(path, format string, args ...any) {
	if path == "" {
		return
	}
	if info, err := os.Stat(path); err == nil && info.Size() >= MaxLogSize {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	line := strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
}
