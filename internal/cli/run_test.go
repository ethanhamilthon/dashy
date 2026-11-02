package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/agent"
	"dashy/internal/tui"
)

func TestVersion(t *testing.T) {
	tui.Version = "0.1.0"
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"version"}, strings.NewReader(""), &out, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "0.1.0" {
		t.Fatalf("version output: %q", out.String())
	}
}

func TestShellCommandsUseEndpoint(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := agent.New(cwd, agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	t.Setenv("DASHY_ENDPOINT", "")
	if err = Run(context.Background(), []string{"notify", "hi"}, strings.NewReader(""), ioDiscard{}, ioDiscard{}); err == nil || !strings.Contains(err.Error(), "-s") {
		t.Fatalf("notify outside a shell: %v", err)
	}
	result, err := a.Shell.Execute(context.Background(), `echo "$DASHY_ENDPOINT"`, time.Second)
	if err != nil || result.Error {
		t.Fatal(result, err)
	}
	t.Setenv("DASHY_ENDPOINT", strings.TrimSpace(result.Output))
	if err = Run(context.Background(), []string{"notify"}, strings.NewReader("from stdin"), ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if messages := a.Notifications(); len(messages) != 1 || !strings.Contains(messages[0], "from stdin") {
		t.Fatalf("%v", messages)
	}
	path := filepath.Join(cwd, "image.png")
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = Run(context.Background(), []string{"view", path, path, path, path}, strings.NewReader(""), ioDiscard{}, ioDiscard{}); err != nil {
		t.Fatal(err)
	}
	if err = Run(context.Background(), []string{"view", path}, strings.NewReader(""), ioDiscard{}, ioDiscard{}); err == nil || !strings.Contains(err.Error(), "four images") {
		t.Fatalf("view limit: %v", err)
	}
	if err = Run(context.Background(), []string{"view", filepath.Join(cwd, "missing.png")}, strings.NewReader(""), ioDiscard{}, ioDiscard{}); err == nil {
		t.Fatal("missing image accepted")
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
