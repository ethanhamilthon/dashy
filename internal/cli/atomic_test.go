package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAtomicWritePreservesModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("old"), 0750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("file", link); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(link, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatalf("mode %v %v", info, err)
	}
	if target, err := os.Readlink(link); err != nil || target != "file" {
		t.Fatalf("link %s %v", target, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
		t.Fatalf("data %s %v", data, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("temporary files leaked: %v", entries)
	}
	if err := writeFile(dir, []byte("no")); err == nil {
		t.Fatal("directory replaced")
	}
}

func TestAtomicWriteReadersNeverSeePartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	old, newData := bytes.Repeat([]byte("a"), 65536), bytes.Repeat([]byte("b"), 65536)
	if err := writeFile(path, old); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	errors := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil || (!bytes.Equal(data, old) && !bytes.Equal(data, newData)) {
				select {
				case errors <- "partial or missing file":
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 20; i++ {
		data := old
		if i%2 == 0 {
			data = newData
		}
		if err := writeFile(path, data); err != nil {
			t.Error(err)
			break
		}
	}
	close(stop)
	wg.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
}
