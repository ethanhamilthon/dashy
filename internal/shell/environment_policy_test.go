package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandPrependsBinaryAfterConfiguredAndMetadataPath(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Dir(exe) + string(os.PathListSeparator) + "/metadata/bin"
	r := &Runner{Env: func() map[string]string { return map[string]string{"PATH": "/metadata/bin"} }}
	result, err := r.Execute(context.Background(), `printf '%s' "$PATH"`, time.Second)
	if err != nil || result.Output != want {
		t.Fatalf("tool PATH = %q, want %q: %v", result.Output, want, err)
	}
	cmd := Command(context.Background(), "", []string{"PATH=/configured/bin"}, `printf '%s' "$PATH"`)
	out, err := cmd.Output()
	if err != nil || !strings.HasPrefix(string(out), filepath.Dir(exe)+string(os.PathListSeparator)) {
		t.Fatalf("direct command PATH = %q: %v", out, err)
	}
}
