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
	"sync"
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
	var extraFiles []*os.File
	if r := lifelineReader(); r != nil {
		// A watcher in the command's group waits on a pipe only this process writes to.
		// When this process dies, even by SIGKILL, the read ends and the group is killed.
		script = "{ read -r -u 3 _; kill -KILL 0; } >/dev/null 2>&1 & exec 3<&-; " + script
		extraFiles = []*os.File{r}
	}
	cmd := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-c", script)
	cmd.ExtraFiles = extraFiles
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
	result, _ := r.Run(ctx, command, timeout, limit, nil)
	return result
}

// Run is Execute that can let go of the command: when detach fires first, Run returns
// the output so far and the still running command, which the caller then owns.
func (r *Runner) Run(ctx context.Context, command string, timeout time.Duration, limit int, detach <-chan struct{}) (Result, *Background) {
	var env map[string]string
	if r.Env != nil {
		env = r.Env()
	}
	// Not tied to ctx: a detached command must outlive the call. Cancellation and the
	// timeout kill the group below.
	cmd := Command(context.Background(), r.Dir, env, command)
	cmd.Stdin = strings.NewReader("")
	buffer := NewOutputBuffer(limit)
	// Stdout and Stderr share one writer, so exec never calls Write concurrently.
	out := &switchWriter{w: buffer}
	cmd.Stdout, cmd.Stderr = out, out
	if ctx.Err() != nil {
		return Result{Output: "\n[dashy: cancelled]\n", ExitCode: 130, Error: true}, nil
	}
	if err := cmd.Start(); err != nil {
		io.WriteString(buffer, err.Error()+"\n")
		return Result{Output: buffer.Text(), ExitCode: 127, Error: true}, nil
	}
	done := make(chan error, 1)
	go func() { done <- Reap(cmd) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	result := Result{}
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = killGroup(cmd)
		err = <-done
		result.ExitCode = 130
	case <-timer.C:
		_ = killGroup(cmd)
		err = <-done
		result.ExitCode = 124
	case <-detach:
		out.mu.Lock()
		text := buffer.Text()
		out.mu.Unlock()
		return Result{Output: text}, &Background{Cmd: cmd, done: done, out: out, buffer: buffer}
	}
	var exit *exec.ExitError
	switch {
	case result.ExitCode == 130:
		io.WriteString(buffer, "\n[dashy: cancelled]\n")
	case result.ExitCode == 124:
		io.WriteString(buffer, fmt.Sprintf("\n[dashy: timed out after %s]\n", timeout))
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.ExitCode = 128 + int(status.Signal())
		}
	case err != nil:
		result.ExitCode = 1
		io.WriteString(buffer, fmt.Sprintf("\n[dashy: %v]\n", err))
	}
	result.Output = buffer.Text()
	result.Error = result.ExitCode != 0
	return result, nil
}

// switchWriter is the output of a command, which can move to a log file mid-run.
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Background is a command that Run let go of; it keeps running in its process group.
type Background struct {
	Cmd    *exec.Cmd
	done   <-chan error
	out    *switchWriter
	buffer *OutputBuffer
}

// Redirect sends the output from now on to w, after the output kept so far.
func (b *Background) Redirect(w io.Writer) {
	b.out.mu.Lock()
	defer b.out.mu.Unlock()
	io.WriteString(w, b.buffer.Text())
	b.out.w = w
}

// Wait waits for the command to end and its group to be killed, as Reap does.
func (b *Background) Wait() error { return <-b.done }

var lifeline struct {
	once sync.Once
	r, w *os.File // w stays open for the life of the process
}

// lifelineReader is the read end of a pipe whose write end this process holds
// until it exits; nil if the pipe could not be made.
func lifelineReader() *os.File {
	lifeline.once.Do(func() {
		if r, w, err := os.Pipe(); err == nil {
			lifeline.r, lifeline.w = r, w
		}
	})
	return lifeline.r
}
