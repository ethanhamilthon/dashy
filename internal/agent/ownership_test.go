package agent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestOwnershipChild(t *testing.T) {
	mode := os.Getenv("DASHY_OWNERSHIP_CHILD")
	if mode == "" {
		return
	}
	a, err := New(os.Getenv("DASHY_TEST_CWD"), ConfigFiles{}, os.Getenv("DASHY_TEST_SESSION"))
	if mode == "reader" {
		if err == nil || !strings.Contains(err.Error(), "in use") {
			t.Fatalf("second process opened an owned session: %v", err)
		}
		fmt.Println("REJECTED")
		return
	}
	if err != nil {
		t.Fatalf("writer could not acquire released ownership: %v", err)
	}
	defer a.Close()
	fmt.Println("OWNER")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
func TestOwnershipAcrossProcessesAndCrashRelease(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	id := a.SessionInfo()["id"].(string)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(mode string) *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=^TestOwnershipChild$")
		cmd.Env = append(os.Environ(), "DASHY_OWNERSHIP_CHILD="+mode, "DASHY_TEST_CWD="+cwd, "DASHY_TEST_SESSION="+id)
		return cmd
	}
	output, err := child("reader").CombinedOutput()
	if err != nil || !strings.Contains(string(output), "REJECTED") {
		t.Fatalf("second client: %s %v", output, err)
	}
	a.Close()
	cmd := child("writer")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	outputPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	scanner := bufio.NewScanner(outputPipe)
	if !scanner.Scan() || scanner.Text() != "OWNER" {
		t.Fatal("child owner did not initialize")
	}
	if second, err := New(cwd, ConfigFiles{}, id); err == nil {
		second.Close()
		t.Fatal("second client stole ownership")
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("expected killed child exit")
	}
	replacement, err := New(cwd, ConfigFiles{}, id)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
}
