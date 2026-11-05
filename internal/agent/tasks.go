package agent

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"dashy/internal/shell"
)

// Task is one background command of a session, as stored.
type Task struct {
	ID, Command, Status, Log string // Status: running, done, failed, stopped or lost
	ExitCode                 *int
	Started, Ended           time.Time
}

const (
	taskResultLines = 40   // output lines a task result carries
	taskResultBytes = 8192 // and at most this many bytes of them
)

// tasks runs the background tasks of one session. A finished task queues its
// result as session input; closing the session stops the tasks still running.
type tasks struct {
	db                *sql.DB
	session, dir, log string
	env               map[string]string // set again once the session socket is open
	queue             *Steering
	mu                sync.Mutex
	running           map[string]*exec.Cmd
	stopped           map[string]bool
	closed            bool
	wg                sync.WaitGroup
}

func (a *Agent) newTasks() *tasks {
	// Tasks of an earlier run of this session died with its process.
	_, _ = a.db.Exec(`UPDATE tasks SET status = 'lost', ended = ? WHERE session = ? AND status = 'running'`, time.Now().UnixNano(), a.id)
	return &tasks{db: a.db, session: a.id, dir: a.Shell.Dir, log: filepath.Join(a.home, "tasks"), env: a.shellEnv(),
		queue: a.queue, running: map[string]*exec.Cmd{}, stopped: map[string]bool{}}
}

// run starts command in the project directory and returns the task ID.
func (t *tasks) run(command string) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("tasks run requires a command")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return "", errors.New("session has ended")
	}
	if err := os.MkdirAll(t.log, 0700); err != nil {
		return "", err
	}
	var n int
	if err := t.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE session = ?`, t.session).Scan(&n); err != nil {
		return "", err
	}
	id := fmt.Sprintf("t%d", n+1)
	logPath := filepath.Join(t.log, t.session+"-"+id+".log")
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	cmd := shell.Command(context.Background(), t.dir, t.env, command)
	cmd.Stdout, cmd.Stderr = file, file
	started := time.Now()
	if _, err := t.db.Exec(`INSERT INTO tasks (session, id, command, status, started, log) VALUES (?, ?, ?, 'running', ?, ?)`,
		t.session, id, command, started.UnixNano(), logPath); err != nil {
		file.Close()
		return "", err
	}
	if err := cmd.Start(); err != nil {
		file.Close()
		t.finish(id, "failed", nil)
		return "", err
	}
	t.running[id] = cmd
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		err := shell.Reap(cmd)
		file.Close()
		t.mu.Lock()
		stopped := t.stopped[id]
		t.mu.Unlock()
		status, code := "done", cmd.ProcessState.ExitCode()
		switch {
		case stopped:
			status = "stopped"
		case err != nil || code != 0:
			status = "failed"
		}
		exit := &code
		if code < 0 {
			exit = nil
		}
		t.finish(id, status, exit)
		result := Input{Text: taskResult(id, command, status, exit, time.Since(started), logPath), Notice: true}
		// The result is queued before the task stops counting as running, so a waiting
		// dashy -p never sees neither.
		t.mu.Lock()
		if !t.closed {
			t.queue.PushNotice(result)
		}
		delete(t.running, id)
		t.mu.Unlock()
	}()
	return id, nil
}

func (t *tasks) finish(id, status string, exit *int) {
	_, _ = t.db.Exec(`UPDATE tasks SET status = ?, exit_code = ?, ended = ? WHERE session = ? AND id = ?`,
		status, exit, time.Now().UnixNano(), t.session, id)
}

// stop kills a running task; its result still arrives, with status stopped.
func (t *tasks) stop(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	cmd, ok := t.running[id]
	if !ok {
		return fmt.Errorf("task %s is not running", id)
	}
	t.stopped[id] = true
	return cmd.Cancel()
}

// Running reports how many tasks are running.
func (t *tasks) Running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.running)
}

// close stops every running task without reporting results and waits for them.
func (t *tasks) close() {
	t.mu.Lock()
	t.closed = true
	for id, cmd := range t.running {
		t.stopped[id] = true
		_ = cmd.Cancel()
	}
	t.mu.Unlock()
	t.wg.Wait()
}

// taskResult is the session input that reports a finished task.
func taskResult(id, command, status string, exit *int, took time.Duration, logPath string) string {
	attrs := fmt.Sprintf(`id=%q status=%q`, id, status)
	if exit != nil {
		attrs += fmt.Sprintf(` exit_code="%d"`, *exit)
	}
	attrs += fmt.Sprintf(` duration=%q`, took.Round(time.Second).String())
	body := xmlText.Replace("$ " + command + "\n" + tail(logPath))
	return "<task-result " + attrs + ">\n" + strings.TrimRight(body, "\n") + "\n[full output: " + logPath + "]\n</task-result>"
}

// xmlText escapes text inside a tag; unlike xml.EscapeText it keeps newlines and quotes.
var xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// tail is the end of a log: its last taskResultLines lines, at most taskResultBytes.
// Only the end of the file is read, however large the log is.
func tail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "(no output: " + err.Error() + ")"
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "(no output: " + err.Error() + ")"
	}
	start := max(0, info.Size()-taskResultBytes-1)
	data := make([]byte, info.Size()-start)
	n, err := file.ReadAt(data, start)
	if err != nil && n < len(data) {
		data = data[:n]
	}
	if start > 0 {
		// Drop the partial first line (and with it any cut UTF-8 sequence).
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return "(no output)"
	}
	lines := strings.Split(text, "\n")
	switch {
	case len(lines) > taskResultLines:
		lines = append([]string{"... earlier lines"}, lines[len(lines)-taskResultLines:]...)
	case start > 0:
		lines = append([]string{"... earlier output"}, lines...)
	}
	return strings.Join(lines, "\n")
}

// IsNotice reports whether text is session input from dashy itself (a task result or
// a dashy view image) rather than from the user.
func IsNotice(text string) bool {
	return strings.HasPrefix(text, "<task-result ") || strings.HasPrefix(text, "<view>")
}

var taskHeader = regexp.MustCompile(`^<task-result id="([^"]*)" status="([^"]*)"(?: exit_code="([^"]*)")? duration="([^"]*)">\n\$ ([^\n]*)`)

// NoticeSummary is the one-line form of a notice for the timeline: "task t1 done · exit 0 · 3s · $ make".
func NoticeSummary(text string) string {
	m := taskHeader.FindStringSubmatch(text)
	if m == nil {
		return strings.Join(strings.Fields(strings.NewReplacer("<view>", "", "</view>", "").Replace(text)), " ")
	}
	parts := []string{"task " + m[1] + " " + m[2]}
	if m[3] != "" {
		parts = append(parts, "exit "+m[3])
	}
	command := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(m[5])
	return strings.Join(append(parts, m[4], "$ "+command), " · ")
}

// RunningTasks is how many background tasks of the session are running.
func (a *Agent) RunningTasks() int {
	if a.tasks == nil {
		return 0
	}
	return a.tasks.Running()
}

// SessionTasks lists the tasks of a session, oldest first.
func SessionTasks(home, session string) ([]Task, error) {
	db, err := openDB(home)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return sessionTasks(db, session)
}

// Tasks lists the tasks of this session, oldest first.
func (a *Agent) Tasks() ([]Task, error) { return sessionTasks(a.db, a.id) }

// StopTask kills a running task of this session; its result reports stopped.
func (a *Agent) StopTask(id string) error {
	if a.tasks == nil {
		return fmt.Errorf("task %s is not running", id)
	}
	return a.tasks.stop(id)
}

func sessionTasks(db *sql.DB, session string) ([]Task, error) {
	rows, err := db.Query(`SELECT id, command, status, log, exit_code, started, ended FROM tasks WHERE session = ? ORDER BY started`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Task
	for rows.Next() {
		var task Task
		var code, ended sql.NullInt64
		var started int64
		if err := rows.Scan(&task.ID, &task.Command, &task.Status, &task.Log, &code, &started, &ended); err != nil {
			return nil, err
		}
		if code.Valid {
			c := int(code.Int64)
			task.ExitCode = &c
		}
		task.Started = time.Unix(0, started)
		if ended.Valid {
			task.Ended = time.Unix(0, ended.Int64)
		}
		list = append(list, task)
	}
	return list, rows.Err()
}

// TaskOutput is the end of a task's output, as its result shows it.
func TaskOutput(task Task) string { return tail(task.Log) }
