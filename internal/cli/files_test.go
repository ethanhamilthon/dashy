package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
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

// Malformed or empty blocks change nothing; untouched line endings stay as they were.
func TestPatchRejectsMalformedAndKeepsLineEndings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	patch := func(file, blocks string) (string, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(file), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := runTool(t, blocks, "patch", path)
		data, _ := os.ReadFile(path)
		return string(data), err
	}
	block := func(search, replace string) string {
		return "<<<<<<< SEARCH\n" + search + "\n=======\n" + replace + "\n>>>>>>> REPLACE\n"
	}
	for name, blocks := range map[string]string{
		"truncated second block": block("one", "ONE") + "<<<<<<< SEARCH\ntwo\n=======\nTWO\n",
		"replace without search": block("one", "ONE") + ">>>>>>> REPLACE\n",
		"empty search":           "<<<<<<< SEARCH\n=======\n>>>>>>> REPLACE\n",
		"nested search":          "<<<<<<< SEARCH\none\n<<<<<<< SEARCH\n=======\nx\n>>>>>>> REPLACE\n",
	} {
		if got, err := patch("one\ntwo\n", blocks); err == nil || got != "one\ntwo\n" {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
	for name, c := range map[string]struct{ file, blocks, want string }{
		"mixed endings":       {"a\r\nb\nc\r\n", block("b", "B"), "a\r\nB\nc\r\n"},
		"multi-line in CRLF":  {"a\r\nb\r\nc\n", block("a\nb", "A\nB"), "A\r\nB\r\nc\n"},
		"multi-line in LF":    {"a\r\nb\nc\n", block("b\nc", "x"), "a\r\nx\n"},
		"delete a CRLF line":  {"a\r\nb\r\nc\n", block("b", ""), "a\r\nc\n"},
		"insert in CRLF line": {"a\r\nb\r\n", block("a", "a\nz"), "a\r\nz\r\nb\r\n"},
	} {
		if got, err := patch(c.file, c.blocks); err != nil || got != c.want {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A result that cannot be written fails the command.
func TestOutputFailureIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(path, []byte("x\n"), 0600)
	err := Run(context.Background(), []string{"read", path}, strings.NewReader(""), brokenWriter{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v", err)
	}
}

// After -- every argument is positional, even dash-prefixed ones.
func TestGrepDoubleDash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	os.WriteFile(path, []byte("-l here\nother\n"), 0600)
	out, err := runTool(t, "", "grep", "--", "-l", path)
	if err != nil || !strings.Contains(out, ":1:-l here") {
		t.Fatalf("grep -- -l: %q %v", out, err)
	}
}

// Special and huge files are refused; unreadable files make grep fail instead of looking like no match.
func TestReadRefusesSpecialFilesAndGrepReportsFailures(t *testing.T) {
	if _, err := runTool(t, "", "read", "/dev/zero"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("read /dev/zero: %v", err)
	}
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("match\n"), 0600)
	os.WriteFile(filepath.Join(dir, "locked.txt"), []byte("match\n"), 0000)
	var out, diag bytes.Buffer
	err := Run(context.Background(), []string{"grep", "match", dir}, strings.NewReader(""), &out, &diag)
	if ExitCode(err) != 2 || !strings.Contains(out.String(), "ok.txt:1:match") || !strings.Contains(diag.String(), "locked.txt") {
		t.Fatalf("grep: %v %q %q", err, out.String(), diag.String())
	}
}
