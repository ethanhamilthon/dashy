package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, command string, timeout time.Duration, limit int) Result {
	t.Helper()
	r := &Runner{Dir: t.TempDir(), Env: func() map[string]string { return map[string]string{"DASHY_SESSION_ID": "s1"} }}
	result, err := r.Execute(context.Background(), command, timeout, limit)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestFreshStateEnvironmentAndBASHENV(t *testing.T) {
	t.Setenv("BASH_ENV", filepath.Join(t.TempDir(), "rc"))
	if err := os.WriteFile(os.Getenv("BASH_ENV"), []byte("echo rc-ran\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Dir: t.TempDir(), Env: func() map[string]string { return map[string]string{"DASHY_SESSION_ID": "s1"} }}
	first, _ := r.Execute(context.Background(), "x=1; cd /; echo $DASHY_SESSION_ID ${PAGER}", 5*time.Second, 0)
	second, _ := r.Execute(context.Background(), "echo \"[$x]\" $PWD", 5*time.Second, 0)
	if first.Output != "s1 cat\n" || strings.Contains(first.Output, "rc-ran") {
		t.Fatalf("first: %q", first.Output)
	}
	if strings.Contains(second.Output, "[1]") || !strings.HasPrefix(second.Output, "[] ") {
		t.Fatalf("state leaked: %q", second.Output)
	}
}

func TestExitCodeAndTimeoutKillsGroup(t *testing.T) {
	if got := run(t, "echo hi; exit 3", 5*time.Second, 0); got.ExitCode != 3 || got.Output != "hi\n" || !got.Error {
		t.Fatalf("exit: %+v", got)
	}
	start := time.Now()
	got := run(t, "sleep 30 & sleep 30", time.Second, 0)
	if got.ExitCode != 124 || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %+v after %s", got, time.Since(start))
	}
}

func TestCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	r := &Runner{Dir: t.TempDir()}
	result, err := r.Execute(ctx, "sleep 30", 10*time.Second, 0)
	if err != nil || result.ExitCode != 130 {
		t.Fatalf("cancel: %+v %v", result, err)
	}
}

// A background child with redirected output must not outlive a finished call.
func TestBackgroundDescendantsDoNotSurviveCall(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	r := &Runner{Dir: dir}
	result, err := r.Execute(context.Background(), "(sleep 0.5; touch marker) >/dev/null 2>&1 &", 5*time.Second, 0)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("call: %+v %v", result, err)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background descendant survived the call")
	}
}

// A background child that keeps the output pipe open is cut off after the wait delay.
func TestPipeHoldingDescendantIsKilled(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	r := &Runner{Dir: dir}
	start := time.Now()
	result, err := r.Execute(context.Background(), "(sleep 1; touch marker) &", 5*time.Second, 0)
	if err != nil || result.ExitCode != 0 || time.Since(start) > 4*time.Second {
		t.Fatalf("call: %+v %v after %s", result, err, time.Since(start))
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("pipe-holding descendant survived the call")
	}
}

func TestOutputLimitKeepsFirstAndLastHalves(t *testing.T) {
	got := run(t, "head -c 3000 /dev/zero | tr '\\0' a; printf END", 5*time.Second, 1000)
	if !strings.HasPrefix(got.Output, "aaaa") || !strings.HasSuffix(got.Output, "END") || !strings.Contains(got.Output, "[output truncated") {
		t.Fatalf("truncated output: %q", got.Output)
	}
	if full := run(t, "head -c 3000 /dev/zero | tr '\\0' a", 5*time.Second, 0); len(full.Output) != 3000 {
		t.Fatalf("limit 0 must keep everything, got %d bytes", len(full.Output))
	}
}

func TestBlockOnlyVariablesAreNotInherited(t *testing.T) {
	t.Setenv("DASHY_MODE", "tui")
	t.Setenv("DASHY_SOURCE", "startup")
	t.Setenv("DASHY_ENDPOINT", "/run/inherited.sock")
	const script = `echo "mode=$DASHY_MODE source=$DASHY_SOURCE endpoint=$DASHY_ENDPOINT"`
	dir := t.TempDir()
	got, err := Command(context.Background(), dir, nil, script).Output()
	if err != nil || string(got) != "mode= source= endpoint=\n" {
		t.Fatalf("inherited: %q %v", got, err)
	}
	got, err = Command(context.Background(), dir, map[string]string{"DASHY_MODE": "print", "DASHY_SOURCE": "new", "DASHY_ENDPOINT": "/run/own.sock"}, script).Output()
	if err != nil || string(got) != "mode=print source=new endpoint=/run/own.sock\n" {
		t.Fatalf("block env: %q %v", got, err)
	}
}

func TestSetsProcessGroup(t *testing.T) {
	cmd := Command(context.Background(), t.TempDir(), nil, "true")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("command must run in its own process group")
	}
}
