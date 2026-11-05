package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTool(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), args, strings.NewReader(stdin), &out, &out)
	return out.String(), err
}

func TestReadWritePatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "a.txt")
	if _, err := runTool(t, "one\ntwo\nthree\nfour\n", "write", path); err != nil {
		t.Fatal(err)
	}
	if out, err := runTool(t, "", "read", path, "-o", "2", "-n", "2"); err != nil || out != "     2\ttwo\n     3\tthree\n... 1 more lines; continue with -o 4\n" {
		t.Fatalf("read: %q %v", out, err)
	}
	blocks := "<<<<<<< SEARCH\ntwo\n=======\n2\n>>>>>>> REPLACE\n<<<<<<< SEARCH\nfour\n=======\n>>>>>>> REPLACE\n"
	if _, err := runTool(t, blocks, "patch", path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "one\n2\nthree\n" {
		t.Fatalf("patched: %q", data)
	}
	// A failing block leaves the file as it was, earlier blocks included.
	if _, err := runTool(t, "<<<<<<< SEARCH\none\n=======\n1\n>>>>>>> REPLACE\n<<<<<<< SEARCH\nmissing\n=======\nx\n>>>>>>> REPLACE\n", "patch", path); err == nil || !strings.Contains(err.Error(), "block 2") {
		t.Fatalf("missing search: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "one\n2\nthree\n" {
		t.Fatalf("failed patch changed the file: %q", data)
	}
	os.WriteFile(path, []byte("x\r\ny\r\nx\r\n"), 0600)
	if _, err := runTool(t, "<<<<<<< SEARCH\nx\n=======\nz\n>>>>>>> REPLACE\n", "patch", path); err == nil || !strings.Contains(err.Error(), "found 2 times") {
		t.Fatalf("ambiguous search: %v", err)
	}
	if _, err := runTool(t, "<<<<<<< SEARCH\ny\n=======\nY\n>>>>>>> REPLACE\n", "patch", path); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "x\r\nY\r\nx\r\n" {
		t.Fatalf("CRLF patch: %q", data)
	}
	if _, err := runTool(t, "", "read", "/nonexistent"); err == nil {
		t.Fatal("read of a missing file succeeded")
	}
}

func TestGrep(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0700)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc Hello() {}\n"), 0600)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello world\n"), 0600)
	os.WriteFile(filepath.Join(dir, ".hidden", "c.go"), []byte("func Hello()\n"), 0600)
	os.WriteFile(filepath.Join(dir, "bin"), []byte("Hello\x00"), 0600)
	out, err := runTool(t, "", "grep", "-i", "hello", dir)
	if err != nil || out != filepath.Join(dir, "a.go")+":2:func Hello() {}\n"+filepath.Join(dir, "b.txt")+":1:hello world\n" {
		t.Fatalf("grep: %q %v", out, err)
	}
	if out, _ = runTool(t, "", "grep", "-l", "-g", "*.go", "Hello", dir); out != filepath.Join(dir, "a.go")+"\n" {
		t.Fatalf("grep -l -g: %q", out)
	}
	if out, _ = runTool(t, "", "grep", "--files", "-g", "*.go", dir); out != filepath.Join(dir, "a.go")+"\n" {
		t.Fatalf("grep --files: %q", out)
	}
	if out, _ = runTool(t, "", "grep", "--files", dir); strings.Count(out, "\n") != 3 {
		t.Fatalf("grep --files lists every visible file: %q", out)
	}
	if _, err = runTool(t, "", "grep", "nothing", dir); ExitCode(err) != 1 || !IsReported(err) {
		t.Fatalf("no match: %v", err)
	}
	if _, err = runTool(t, "", "grep", "x", filepath.Join(dir, "missing")); err == nil || IsReported(err) {
		t.Fatalf("missing path: %v", err)
	}
}
