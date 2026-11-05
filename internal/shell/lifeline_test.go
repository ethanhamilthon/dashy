package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestMain lets the test binary act as an owner process that starts a command and hangs.
func TestMain(m *testing.M) {
	if dir := os.Getenv("DASHY_TEST_OWNER"); dir != "" {
		cmd := Command(context.Background(), dir, nil, "touch ready; sleep 1; touch survived")
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A command dies with its owner, even when the owner is killed with SIGKILL.
func TestCommandDiesWithOwner(t *testing.T) {
	dir := t.TempDir()
	owner := exec.Command(os.Args[0], "-test.run=^$")
	owner.Env = append(os.Environ(), "DASHY_TEST_OWNER="+dir)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			owner.Process.Kill()
			t.Fatal("command did not start")
		}
	}
	owner.Process.Kill()
	owner.Wait()
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "survived")); err == nil {
		t.Fatal("command outlived its killed owner")
	}
}
