package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func run(t *testing.T, r *Runner, command string, timeout time.Duration) Result {
	t.Helper()
	result, err := r.Execute(context.Background(), command, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestStatelessFreshVariablesFunctionsAndCWD(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Dir: dir}

	// 1. Fresh variables: variable assignment does not persist across calls.
	res := run(t, r, "x=40; echo $x", 5*time.Second)
	if res.Output != "40\n" || res.ExitCode != 0 {
		t.Fatalf("first call: %+v", res)
	}
	res = run(t, r, `echo "[$x]"`, 5*time.Second)
	if res.Output != "[]\n" || res.ExitCode != 0 {
		t.Fatalf("variable leaked across calls: %+v", res)
	}

	// 2. Fresh functions: defined functions do not persist across calls.
	res = run(t, r, "add() { echo $(($1 + 1)); }", 5*time.Second)
	if res.ExitCode != 0 {
		t.Fatalf("function definition failed: %+v", res)
	}
	res = run(t, r, "add 1", 5*time.Second)
	if res.ExitCode != 127 || !strings.Contains(res.Output, "not found") {
		t.Fatalf("function persisted across calls: %+v", res)
	}

	// 3. Fresh cwd: directory changes do not persist across calls.
	res = run(t, r, "mkdir subdir && cd subdir && basename \"$(pwd)\"", 5*time.Second)
	if res.Output != "subdir\n" || res.ExitCode != 0 {
		t.Fatalf("cd inside call failed: %+v", res)
	}
	res = run(t, r, "pwd", 5*time.Second)
	cleanOut := strings.TrimSpace(res.Output)
	realDir, _ := filepath.EvalSymlinks(dir)
	realClean, _ := filepath.EvalSymlinks(cleanOut)
	if realClean != realDir {
		t.Fatalf("cwd persisted: got %q want %q", cleanOut, dir)
	}

	// 4. Fresh shell options: options do not persist across calls.
	run(t, r, "shopt -s nullglob", 5*time.Second)
	res = run(t, r, "shopt nullglob", 5*time.Second)
	if !strings.Contains(res.Output, "off") {
		t.Fatalf("shopt leaked across calls: %+v", res)
	}
}

func TestBashSemantics(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Dir: dir}

	for _, tc := range []struct {
		name    string
		command string
		wantOut string
		code    int
	}{
		{"pipeline", "echo hello | tr a-z A-Z", "HELLO\n", 0},
		{"heredoc", "cat <<'EOF'\nheredoc line\nEOF", "heredoc line\n", 0},
		{"merged stdout/stderr", "echo stdout; echo stderr >&2", "stdout\nstderr\n", 0},
		{"exit code 7", "printf problem >&2; exit 7", "problem", 7},
		{"false", "false", "", 1},
		{"true", "true", "", 0},
		{"syntax error", "if then", "syntax error", 2},
		{"empty stdin cat", "cat; echo done", "done\n", 0},
		{"cat wc -c", "cat | wc -c", "0", 0},
		{"pagers", "echo $PAGER $GIT_PAGER", "cat cat\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, r, tc.command, 5*time.Second)
			if res.ExitCode != tc.code || res.Error != (tc.code != 0) || !strings.Contains(res.Output, tc.wantOut) {
				t.Fatalf("%s: %+v, want output containing %q, code %d", tc.name, res, tc.wantOut, tc.code)
			}
		})
	}

	// Dashy binary directory is first in PATH.
	res := run(t, r, `echo "$PATH"`, 5*time.Second)
	if execPath, err := os.Executable(); err == nil {
		execDir := filepath.Dir(execPath)
		firstPart := strings.Split(strings.TrimSpace(res.Output), string(os.PathListSeparator))[0]
		if firstPart != execDir {
			t.Fatalf("PATH does not start with executable dir %q: %q", execDir, res.Output)
		}
	}
}

func TestBASHENVExcluded(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "evil_marker.txt")
	badScript := filepath.Join(dir, "evil.sh")
	if err := os.WriteFile(badScript, []byte("echo EVIL > "+marker+"\n"), 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Inherited host BASH_ENV must be excluded.
	t.Setenv("BASH_ENV", badScript)
	r := &Runner{Dir: dir}
	res := run(t, r, `echo clean; echo "[$BASH_ENV]"`, 5*time.Second)
	if strings.Contains(res.Output, "EVIL") || !strings.Contains(res.Output, "clean\n[]") {
		t.Fatalf("host BASH_ENV executed or passed: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("host BASH_ENV script ran!")
	}

	// 2. Env callback BASH_ENV must also be excluded.
	rConfigured := &Runner{
		Dir: dir,
		Env: func() map[string]string {
			return map[string]string{
				"BASH_ENV": badScript,
				"SAFE_VAR": "safe_value",
			}
		},
	}
	res = run(t, rConfigured, `echo "$SAFE_VAR"; echo "[$BASH_ENV]"`, 5*time.Second)
	if !strings.Contains(res.Output, "safe_value\n[]") {
		t.Fatalf("configured BASH_ENV was not stripped: %+v", res)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("configured BASH_ENV script ran!")
	}
}

func TestEnvironmentAndMetadata(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Dir: dir}

	val := "first"
	r.Env = func() map[string]string {
		return map[string]string{
			"TEST_VAR": val,
		}
	}
	res := run(t, r, `echo "$TEST_VAR"`, 5*time.Second)
	if res.Output != "first\n" {
		t.Fatalf("env: %+v", res)
	}

	// Dynamic change on subsequent call
	val = "second"
	res = run(t, r, `echo "$TEST_VAR"`, 5*time.Second)
	if res.Output != "second\n" {
		t.Fatalf("env change: %+v", res)
	}
}

func TestTimeoutKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{Dir: dir}

	started := time.Now()
	result := run(t, r, "sh -c 'sleep 30 & echo $! > child.pid; wait'", 300*time.Millisecond)
	if result.ExitCode != 124 || !strings.Contains(result.Output, "timed out") || time.Since(started) > 5*time.Second {
		t.Fatalf("timeout: %+v after %s", result, time.Since(started))
	}

	data, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for _, c := range strings.TrimSpace(string(data)) {
		pid = pid*10 + int(c-'0')
	}
	// Verify that grandchild was killed
	for i := 0; syscall.Kill(pid, 0) == nil; i++ {
		if i == 40 {
			t.Fatalf("grandchild %d survived timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Timeout on built-in loop
	result = run(t, r, "while :; do :; done", 200*time.Millisecond)
	if result.ExitCode != 124 || !strings.Contains(result.Output, "timed out") {
		t.Fatalf("loop timeout: %+v", result)
	}
}

func TestCancellation(t *testing.T) {
	r := &Runner{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	result, err := r.Execute(ctx, "sleep 30", 10*time.Second)
	if err != nil || result.ExitCode != 130 || !strings.Contains(result.Output, "[dashy: cancelled]") {
		t.Fatalf("cancellation: %+v err=%v", result, err)
	}
}

func TestOutputLimit(t *testing.T) {
	r := &Runner{Dir: t.TempDir()}

	for _, command := range []string{
		"yes x | head -c 400000",
		"i=0; while [ $i -lt 40000 ]; do echo xxxxxxxxx; i=$((i+1)); done",
	} {
		result := run(t, r, command, 20*time.Second)
		if result.ExitCode != 0 || len(result.Output) > DefaultOutputLimit+300 || !strings.Contains(result.Output, "[output truncated") {
			t.Fatalf("%s: length=%d %+v", command, len(result.Output), result.ExitCode)
		}
	}

	// Custom limits
	for _, tc := range []struct{ limit, max int }{{1000, 1300}, {NoOutputLimit, 50000}} {
		result, err := r.ExecuteLimit(context.Background(), "yes x | head -c 50000", 20*time.Second, tc.limit)
		if err != nil || len(result.Output) > tc.max {
			t.Fatalf("limit %d: len=%d err=%v", tc.limit, len(result.Output), err)
		}
		if truncated := strings.Contains(result.Output, "[output truncated"); truncated != (tc.limit > 0) {
			t.Fatalf("limit %d: truncated=%v", tc.limit, truncated)
		}
	}
}

func TestMissingExecutableScenarios(t *testing.T) {
	r := &Runner{Dir: t.TempDir()}

	// Nonexistent command executed within bash returns 127
	result := run(t, r, "nonexistent-command-xyz-12345", 5*time.Second)
	if result.ExitCode != 127 || !result.Error || !strings.Contains(result.Output, "not found") {
		t.Fatalf("missing command: %+v", result)
	}

	// Nonexistent working directory
	rBad := &Runner{Dir: filepath.Join(t.TempDir(), "nonexistent-directory")}
	result, err := rBad.Execute(context.Background(), "echo hello", 5*time.Second)
	if err != nil || result.ExitCode == 0 || !result.Error {
		t.Fatalf("bad dir: %+v %v", result, err)
	}
}

func TestNoStuckWaitFromPipes(t *testing.T) {
	r := &Runner{Dir: t.TempDir()}

	// Background process holding pipes open is terminated by WaitDelay rather than hanging
	started := time.Now()
	result := run(t, r, "sleep 10 & echo finished", 5*time.Second)
	if result.ExitCode != 0 || !strings.Contains(result.Output, "finished") {
		t.Fatalf("pipe holding test: %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Wait took too long with open pipes: %s", elapsed)
	}

	// Detached background task with redirection returns immediately without delay
	started = time.Now()
	result = run(t, r, "(sleep 0.5; echo bg > job.txt) >/dev/null 2>&1 & echo immediate", 5*time.Second)
	if result.ExitCode != 0 || result.Output != "immediate\n" {
		t.Fatalf("redirected bg job: %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("redirected bg job delayed parent: %s", elapsed)
	}
}

func TestCloseTerminatesActive(t *testing.T) {
	r := &Runner{
		Dir: t.TempDir(),
	}

	// Close terminates any active managed processes
	started := make(chan struct{})
	done := make(chan Result, 1)
	go func() {
		close(started)
		res, _ := r.Execute(context.Background(), "sleep 30", 10*time.Second)
		done <- res
	}()
	<-started
	time.Sleep(100 * time.Millisecond)
	r.Close()

	select {
	case res := <-done:
		if res.ExitCode == 0 {
			t.Fatal("expected terminated process on Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not terminate active process")
	}

	// Runner is stateless: calls succeed after Close without reinitialization
	res := run(t, r, "echo still works", 5*time.Second)
	if res.Output != "still works\n" || res.ExitCode != 0 {
		t.Fatalf("Execute after Close: %+v", res)
	}
}

func TestConcurrentRace(t *testing.T) {
	r := &Runner{Dir: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			res := run(t, r, "echo start; echo err >&2; echo done", 5*time.Second)
			if res.ExitCode != 0 || !strings.Contains(res.Output, "done") {
				t.Errorf("worker %d: %+v", n, res)
			}
		}(i)
	}
	wg.Wait()
}

func TestCommandKillsProcessGroupOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	marker := filepath.Join(t.TempDir(), "survived")
	cmd := Command(ctx, "", []string{"BASH_ENV=/nonexistent"}, `(sleep 1; touch `+marker+`) & wait`)
	start := time.Now()
	_ = cmd.Run()
	if time.Since(start) > 900*time.Millisecond {
		t.Fatal("command was not cancelled")
	}
	time.Sleep(1300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("background child survived cancellation")
	}
}
