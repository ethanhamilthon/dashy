package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadTextPrefixDoesNotBlockOnSpecialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	// Unblock the reader on regression, so a failed assertion cannot leak a goroutine.
	t.Cleanup(func() {
		if f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	done := make(chan error, 1)
	go func() {
		_, _, _, err := readTextPrefix(path, 64)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("special file accepted as text")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("special file blocked attachment preparation")
	}
}
