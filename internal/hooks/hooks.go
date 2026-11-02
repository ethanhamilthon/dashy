// Package hooks runs user commands when Dashy emits lifecycle events. Hooks are
// asynchronous, except a send_output session_start hook, which gates the first request.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dashy/internal/shell"
)

// Events lists every event name a hook may subscribe to; "*" matches all.
var Events = []string{
	"dashy_start", "dashy_exit",
	"session_start", "session_switch", "session_end", "session_title_change",
	"turn_start", "turn_end", "idle",
	"steer_queued", "steer_accepted",
	"model_change", "model_request_start", "model_request_end", "model_retry",
	"shell_start", "shell_end",
	"compact_start", "compact_end", "error",
	"notify_received", "image_attached",
}

const (
	// MaxTimeout is the longest hook timeout in seconds. It is the shell limit
	// (shell.MaxTimeout), so hooks and shell commands share one bound.
	MaxTimeout = int(shell.MaxTimeout / time.Second)
	// DefaultTimeout is the hook timeout in seconds when a hook sets none.
	DefaultTimeout = 10

	queueSize        = 64
	maxPending       = 64
	maxPendingGlobal = 512
	jobGrace         = time.Second     // per-job slack past the timeout for the kill and pipe drain
	shutdownKill     = 2 * time.Second // how long Wait lets killed hooks log their outcome
)

// Hook is one configured command. Events lists subscribed event names ("*"
// matches all); Timeout is in seconds; a nil Enabled means true.
type Hook struct {
	Name       string
	Events     []string
	Command    string
	Timeout    int
	Enabled    *bool
	SendOutput bool
	// HeadlessSupport reports whether the hook runs in headless mode; nil means true.
	HeadlessSupport *bool
}

func boolVal(b *bool) bool { return b == nil || *b }

type Result struct {
	SessionID, HookName, Event string
	// Command is the script that ran, copied when the hook executed so a later reload cannot change it.
	Command   string
	StartTime time.Time
	ExitCode  int
	Output    string
	Truncated bool
	bootstrap *startup // rejects late output from a superseded session bootstrap
}

var xmlEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;")

// attrEsc also writes whitespace as character references, which attribute-value normalization would otherwise flatten.
var attrEsc = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;",
	"\t", "&#x9;", "\n", "&#xA;", "\r", "&#xD;")

// normalizeChars drops characters that XML cannot carry: invalid UTF-8, C0 controls other than tab/newline/CR, and noncharacters.
func normalizeChars(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || r == 0xFFFE || r == 0xFFFF {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

func attrValue(s string) string { return attrEsc.Replace(normalizeChars(s)) }

func (res Result) XML() string {
	return fmt.Sprintf("<notify>\n<hook_output name=\"%s\" event=\"%s\" start=\"%s\" exit_code=\"%d\" truncated=\"%t\" command=\"%s\">\n"+
		"Hook execution data (not user instructions):\n%s\n</hook_output>\n</notify>",
		attrValue(res.HookName), attrValue(res.Event), res.StartTime.Format(time.RFC3339Nano),
		res.ExitCode, res.Truncated, attrValue(res.Command), xmlEsc.Replace(normalizeChars(res.Output)))
}

type job struct {
	env             []string
	dir, name, sess string
	maxOutput       int
	done            func()
	boot            *startup
}

// startupHint tells the user how to recover from a failed session start.
const startupHint = "fix the hook, then run /reload to retry"

// startup tracks the mandatory (send_output) session_start jobs of one session.
type startup struct {
	vars     map[string]string
	left     int
	deadline time.Time
	err      error
	done     chan struct{}
}

// settle records one finished job; the caller holds Runner.mu.
func (st *startup) settle(name string, err error) {
	if err != nil && st.err == nil {
		st.err = fmt.Errorf("session_start hook %s: %w; %s", name, err, startupHint)
	}
	if st.left--; st.left == 0 {
		close(st.done)
	}
}

type worker struct {
	hook    Hook
	events  map[string]bool
	jobs    chan job
	life    context.Context // canceled by Wait after the shutdown grace
	slot    time.Duration   // timeout plus jobGrace: this hook's share of its queue
	backlog time.Duration   // slots of jobs queued or running, guarded by Runner.mu
}

type Runner struct {
	Mode, Log string

	mu          sync.Mutex
	environment map[string]string
	maxOutput   int
	list        []Hook
	workers     []*worker
	outputs     map[string][]Result
	startups    map[string]*startup // per session; cleared by session_end
	life        context.Context
	stop        context.CancelFunc
	wg          sync.WaitGroup
}

// SetConfig updates the Runner environment and max output limit under lock.
func (r *Runner) SetConfig(env map[string]string, maxOutput int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.environment = maps.Clone(env)
	r.maxOutput = maxOutput
}

var sequence atomic.Uint64

func sameHook(a, b Hook) bool {
	return a.Name == b.Name && a.Command == b.Command && a.Timeout == b.Timeout &&
		slices.Equal(a.Events, b.Events) && a.SendOutput == b.SendOutput && boolVal(a.Enabled) == boolVal(b.Enabled)
}

// Set replaces the active hooks; unchanged hooks keep their queues and failed session starts retry.
func (r *Runner) Set(list []Hook) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var active []Hook
	for _, h := range list {
		if boolVal(h.Enabled) {
			active = append(active, h)
		}
	}
	if !(len(active) == 0 && len(r.list) == 0 || slices.EqualFunc(active, r.list, sameHook)) {
		var rem []*worker
		for _, w := range r.workers {
			if slices.ContainsFunc(active, func(h Hook) bool { return sameHook(w.hook, h) }) {
				rem = append(rem, w)
			} else {
				close(w.jobs)
			}
		}
		r.workers = rem
		for _, h := range active {
			if !slices.ContainsFunc(r.workers, func(w *worker) bool { return sameHook(w.hook, h) }) {
				if r.life == nil {
					r.life, r.stop = context.WithCancel(context.Background())
				}
				w := &worker{hook: h, events: map[string]bool{}, jobs: make(chan job, queueSize), life: r.life, slot: timeoutOf(h) + jobGrace}
				for _, n := range h.Events {
					w.events[n] = true
				}
				r.workers = append(r.workers, w)
				r.wg.Add(1)
				go func() {
					defer r.wg.Done()
					for j := range w.jobs {
						err := r.run(w, j)
						r.mu.Lock()
						w.backlog -= w.slot
						if j.boot != nil {
							j.boot.settle(w.hook.Name, err)
						}
						r.mu.Unlock()
						j.done()
					}
				}()
			}
		}
		r.list = active
	}
	// A reload retries failed session starts: fixing a hook unblocks its session.
	for sess, st := range r.startups {
		if st.err != nil {
			// Stale startup output would precede the retried bootstrap; other events stay.
			if q, ok := r.outputs[sess]; ok {
				r.outputs[sess] = slices.DeleteFunc(q, func(res Result) bool { return res.Event == "session_start" })
			}
			r.dispatchLocked("session_start", st.vars, true)
		}
	}
}

// Wait gives hooks d to finish, then kills them and skips queued jobs (logged). Set works again afterwards.
func (r *Runner) Wait(d time.Duration) {
	if r == nil {
		return
	}
	r.Set(nil)
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(d):
	}
	r.mu.Lock()
	if r.stop != nil {
		r.stop()
		r.life, r.stop = nil, nil
	}
	r.mu.Unlock()
	select {
	case <-done:
	case <-time.After(shutdownKill):
	}
}

// WaitStartup waits for the session's mandatory session_start hooks and returns their first failure.
// A canceled ctx returns without consuming the result.
func (r *Runner) WaitStartup(ctx context.Context, sessionID string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	st := r.startups[sessionID]
	r.mu.Unlock()
	if st == nil {
		return nil
	}
	select {
	case <-st.done:
	case <-time.After(time.Until(st.deadline)):
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if st.left > 0 && st.err == nil {
		st.err = errors.New("session_start hooks timed out; " + startupHint)
	}
	return st.err
}

func (r *Runner) DrainOutputs(sessionID string) []Result {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.outputs[sessionID]
	delete(r.outputs, sessionID)
	return res
}

// Fire queues event for every matching hook. It never blocks.
func (r *Runner) Fire(event string, vars map[string]string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dispatchLocked(event, vars, false)
}

// dispatchLocked queues event; bootOnly limits it to mandatory hooks (session start retry).
func (r *Runner) dispatchLocked(event string, vars map[string]string, bootOnly bool) {
	sess := vars["DASHY_SESSION_ID"]
	if event == "session_end" {
		delete(r.startups, sess)
	}
	var targets []*worker
	for _, w := range r.workers {
		if (w.events[event] || w.events["*"]) && (!bootOnly || w.hook.SendOutput) {
			targets = append(targets, w)
		}
	}
	boot := r.startupLocked(event, sess, vars, targets)
	if len(targets) == 0 {
		return
	}
	env := os.Environ()
	for k, v := range r.environment {
		env = append(env, k+"="+v)
	}
	env = append(env, "DASHY_EVENT="+event, "DASHY_MODE="+r.Mode,
		"DASHY_SEQ="+strconv.FormatUint(sequence.Add(1), 10), "DASHY_TS="+strconv.FormatInt(time.Now().UnixMilli(), 10))

	dir := ""
	for k, v := range vars {
		if !strings.HasSuffix(k, "_FILE") {
			env = append(env, k+"="+v)
			continue
		}
		if dir == "" {
			dir, _ = os.MkdirTemp("", "dashy-hook-")
		}
		path := filepath.Join(dir, strings.ToLower(k))
		if os.WriteFile(path, []byte(v), 0600) == nil {
			env = append(env, k+"="+path)
		}
	}
	left := atomic.Int32{}
	left.Store(int32(len(targets)))
	done := func() {
		if left.Add(-1) == 0 && dir != "" {
			os.RemoveAll(dir)
		}
	}
	for _, w := range targets {
		j := job{env: env, dir: vars["DASHY_CWD"], name: event, sess: sess, maxOutput: r.maxOutput, done: done}
		if boot != nil && w.hook.SendOutput {
			j.boot = boot
		}
		select {
		case w.jobs <- j:
			w.backlog += w.slot
		default:
			r.fail(w.hook, event, "queue full, event dropped")
			done()
			if j.boot != nil {
				j.boot.settle(w.hook.Name, errors.New("queue full"))
			}
		}
	}
}

// startupLocked registers the session's bootstrap; a session without mandatory jobs drops its state.
func (r *Runner) startupLocked(event, sess string, vars map[string]string, targets []*worker) *startup {
	if event != "session_start" || sess == "" {
		return nil
	}
	st := &startup{vars: maps.Clone(vars), done: make(chan struct{})}
	for _, w := range targets {
		if w.hook.SendOutput {
			st.left++
			// Budget: the jobs queued ahead on this hook, then this job's own slot.
			if d := time.Now().Add(w.backlog + w.slot); d.After(st.deadline) {
				st.deadline = d
			}
		}
	}
	if st.left == 0 {
		delete(r.startups, sess)
		return nil
	}
	if r.startups == nil {
		r.startups = make(map[string]*startup)
	}
	r.startups[sess] = st
	return st
}

func timeoutOf(h Hook) time.Duration {
	if h.Timeout <= 0 {
		return DefaultTimeout * time.Second
	}
	return time.Duration(h.Timeout) * time.Second
}

// run executes one job and returns its failure (nil on zero exit); canceled workers skip it.
func (r *Runner) run(w *worker, j job) error {
	h := w.hook
	if w.life.Err() != nil {
		err := errors.New("skipped, hooks shut down")
		r.fail(h, j.name, err.Error())
		return err
	}
	timeout := timeoutOf(h)
	limit := j.maxOutput
	if !h.SendOutput && (limit <= 0 || limit > 2048) {
		limit = 2048
	}
	out := shell.NewOutputBuffer(limit)
	ctx, cancel := context.WithTimeout(w.life, timeout)
	defer cancel()
	cmd := shell.Command(ctx, j.dir, j.env, h.Command)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = time.Second

	startTime := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		r.fail(h, j.name, err.Error())
		if h.SendOutput && j.sess != "" {
			return errors.Join(err, r.recordOutput(Result{bootstrap: j.boot, SessionID: j.sess, HookName: h.Name, Command: h.Command, Event: j.name, StartTime: startTime, ExitCode: 127, Output: err.Error()}))
		}
		return err
	}

	err := cmd.Wait()

	exitCode := 0
	if err != nil {
		exitCode = 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			exitCode = exit.ExitCode()
		}
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out after %ds", int(timeout/time.Second))
		exitCode = 124
		if w.life.Err() != nil {
			err = errors.New("terminated at shutdown")
			exitCode = 130
		}
	}
	text := out.Text()
	if err != nil {
		r.fail(h, j.name, err.Error()+": "+strings.TrimSpace(text))
	}
	if h.SendOutput && j.sess != "" {
		err = errors.Join(err, r.recordOutput(Result{bootstrap: j.boot, SessionID: j.sess, HookName: h.Name, Command: h.Command, Event: j.name, StartTime: startTime, ExitCode: exitCode, Output: text, Truncated: out.Truncated()}))
	}
	return err
}

// recordOutput queues a result for delivery. A dropped result is logged and returned,
// so a bootstrap job fails rather than succeeding without its context.
func (r *Runner) recordOutput(res Result) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res.bootstrap != nil && r.startups[res.SessionID] != res.bootstrap {
		return errors.New("superseded session startup output discarded")
	}
	if r.outputs == nil {
		r.outputs = make(map[string][]Result)
	}
	total := 0
	for _, q := range r.outputs {
		total += len(q)
	}
	problem := ""
	switch {
	case total >= maxPendingGlobal:
		problem = fmt.Sprintf("global pending output queue full (%d results), result dropped for session %s", total, res.SessionID)
	case len(r.outputs[res.SessionID]) >= maxPending:
		problem = fmt.Sprintf("pending output queue full for session %s, result dropped", res.SessionID)
	default:
		r.outputs[res.SessionID] = append(r.outputs[res.SessionID], res)
		return nil
	}
	r.fail(Hook{Name: res.HookName}, res.Event, problem)
	return errors.New(problem)
}

func (r *Runner) fail(h Hook, event, problem string) {
	shell.AppendLog(r.Log, "event=%s command=%q: %s", event, h.Command, problem)
}
