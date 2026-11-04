package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Result is the outcome of one bash call as the model sees it.
type Result struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
	Error    bool   `json:"error,omitempty"`
}

// Runner executes stateless bash calls in Dir. Env supplies variables exported
// before every call.
type Runner struct {
	Dir string
	Env func() map[string]string
}

// Command builds `bash -c script` in its own process group in dir. extra
// overrides the inherited environment; BASH_ENV and the session variables DASHY_MODE,
// DASHY_SOURCE and DASHY_ENDPOINT are never inherited, pagers are disabled and the running
// binary's directory leads PATH. Cancelling ctx kills the whole group. Start it with cmd.Start and finish it with Reap.
func Command(ctx context.Context, dir string, extra map[string]string, script string) *exec.Cmd {
	bash := "/bin/bash"
	if _, err := os.Stat(bash); err != nil {
		if p, err := exec.LookPath("bash"); err == nil {
			bash = p
		}
	}
	cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-c", script)
	cmd.Dir = dir
	path := os.Getenv("PATH")
	if exe, err := os.Executable(); err == nil {
		path = filepath.Dir(exe) + string(os.PathListSeparator) + path
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		// Session variables belong to this session: a sub-agent must not inherit its parent's endpoint.
		if _, overridden := extra[key]; overridden || key == "BASH_ENV" || key == "PATH" || key == "PAGER" || key == "GIT_PAGER" || key == "DASHY_MODE" || key == "DASHY_SOURCE" || key == "DASHY_ENDPOINT" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	for key, value := range extra {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Env = append(cmd.Env, "PATH="+path, "PAGER=cat", "GIT_PAGER=cat")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	// Pipes held open by orphaned descendants are abandoned after this delay.
	cmd.WaitDelay = 250 * time.Millisecond
	return cmd
}

// Reap waits for a started command and then kills its process group, so no
// descendant can outlive the call, whether it exited, timed out or was cancelled.
// exec.ErrWaitDelay (pipes held by descendants) is reported like any exit.
func Reap(cmd *exec.Cmd) error {
	err := cmd.Wait()
	_ = killGroup(cmd)
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil || cmd.Process.Pid <= 0 {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// Execute runs command with a timeout, keeping at most limit output bytes
// (first and last halves; limit <= 0 keeps everything).
func (r *Runner) Execute(ctx context.Context, command string, timeout time.Duration, limit int) Result {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var env map[string]string
	if r.Env != nil {
		env = r.Env()
	}
	cmd := Command(callCtx, r.Dir, env, command)
	cmd.Stdin = strings.NewReader("")
	// Stdout and Stderr share one writer, so exec never calls Write concurrently.
	out := NewOutputBuffer(limit)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return Result{Output: "\n[dashy: cancelled]\n", ExitCode: 130, Error: true}
		}
		io.WriteString(out, err.Error()+"\n")
		return Result{Output: out.Text(), ExitCode: 127, Error: true}
	}
	err := Reap(cmd)

	result := Result{}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		result.ExitCode = 130
		io.WriteString(out, "\n[dashy: cancelled]\n")
	case callCtx.Err() != nil:
		result.ExitCode = 124
		io.WriteString(out, fmt.Sprintf("\n[dashy: timed out after %s]\n", timeout))
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.ExitCode = 128 + int(status.Signal())
		}
	case err != nil:
		result.ExitCode = 1
		io.WriteString(out, fmt.Sprintf("\n[dashy: %v]\n", err))
	}
	result.Output = out.Text()
	result.Error = result.ExitCode != 0
	return result
}
