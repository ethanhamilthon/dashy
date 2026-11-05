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
	return execute(r, context.Background(), command, timeout, limit)
}

func TestFreshStateEnvironmentAndBASHENV(t *testing.T) {
	t.Setenv("BASH_ENV", filepath.Join(t.TempDir(), "rc"))
	if err := os.WriteFile(os.Getenv("BASH_ENV"), []byte("echo rc-ran\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Dir: t.TempDir(), Env: func() map[string]string { return map[string]string{"DASHY_SESSION_ID": "s1"} }}
	first := execute(r, context.Background(), "x=1; cd /; echo $DASHY_SESSION_ID ${PAGER}", 5*time.Second, 0)
	second := execute(r, context.Background(), "echo \"[$x]\" $PWD", 5*time.Second, 0)
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
	result := execute(r, ctx, "sleep 30", 10*time.Second, 0)
	if result.ExitCode != 130 {
		t.Fatalf("cancel: %+v", result)
	}
}

func TestBackgroundDescendantsDoNotSurviveCall(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	r := &Runner{Dir: dir}
	result := execute(r, context.Background(), "(sleep 0.5; touch marker) >/dev/null 2>&1 &", 5*time.Second, 0)
	if result.ExitCode != 0 {
		t.Fatalf("call: %+v", result)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background descendant survived the call")
	}
}

func TestPipeHoldingDescendantIsKilled(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	r := &Runner{Dir: dir}
	start := time.Now()
	result := execute(r, context.Background(), "(sleep 1; touch marker) &", 5*time.Second, 0)
	if result.ExitCode != 0 || time.Since(start) > 4*time.Second {
		t.Fatalf("call: %+v after %s", result, time.Since(start))
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("pipe-holding descendant survived the call")
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

func execute(r *Runner, ctx context.Context, command string, timeout time.Duration, limit int) Result {
	result, _ := r.Run(ctx, command, timeout, limit, nil)
	return result
}
