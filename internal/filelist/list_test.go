package filelist

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func put(t *testing.T, root, p string) {
	t.Helper()
	path := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestListFallbackOptionsAndCancellation(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"a.go", "dir/b.go", ".hidden/c.go", "node_modules/pkg/x.go", "skip/x.go"} {
		put(t, root, p)
	}
	paths, failures, err := List(context.Background(), root, Options{Ignore: func(p string) bool { return strings.HasPrefix(p, "skip") }})
	if err != nil || len(failures) != 0 || !slices.Equal(paths, []string{"a.go", "dir/b.go"}) {
		t.Fatalf("%v %v %v", paths, failures, err)
	}
	paths, _, err = List(context.Background(), root, Options{Hidden: true, Directories: true, Max: 3})
	if err != nil || len(paths) != 3 || !slices.Contains(paths, ".hidden/") {
		t.Fatalf("%v %v", paths, err)
	}
	paths, _, err = List(context.Background(), root, Options{Depth: 1, Hidden: true})
	if err != nil || !slices.Contains(paths, "dir/b.go") {
		t.Fatalf("%v %v", paths, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := List(ctx, root, Options{}); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestGitDiscoveryRespectsEmptyAndHidden(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Skipf("git unavailable: %s %v", out, err)
	}
	put(t, root, ".gitignore")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	put(t, root, "ignored")
	paths, _, err := List(context.Background(), root, Options{})
	if err != nil || len(paths) != 0 {
		t.Fatalf("empty git: %v %v", paths, err)
	}
	put(t, root, ".hidden")
	if out, err := exec.Command("git", "-C", root, "add", "-f", ".hidden").CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	paths, _, err = List(context.Background(), root, Options{})
	if err != nil || !slices.Equal(paths, []string{".hidden"}) {
		t.Fatalf("hidden git: %v %v", paths, err)
	}
}
