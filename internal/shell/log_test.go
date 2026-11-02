package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	line := strings.Repeat("a", 1000)
	for i := 0; i < 3000; i++ {
		AppendLog(path, "%s", line)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > MaxLogSize+2000 {
		t.Fatalf("log not bounded: %v %v", info, err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	AppendLog("", "ignored")
}
