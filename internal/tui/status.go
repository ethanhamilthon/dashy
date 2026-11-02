package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"dashy/internal/agent"
	"dashy/internal/shell"
)

type StatusSnapshot struct {
	Gen         uint64
	Cwd         string
	Home        string
	LeftCmds    []string
	RightCmds   []string
	Environment map[string]string
	Metadata    map[string]string
}

type statusEventMsg map[string]string
type statusInitMsg struct{}
type statusResultMsg struct {
	gen   uint64
	left  string
	right string
}

func listenForStatusEvents(ch <-chan map[string]string) tea.Cmd {
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return statusEventMsg(ev)
	}
}

var (
	statusLogMu   sync.Mutex
	lastStatusLog time.Time
)

func logStatusFailure(home string, cmd string, err error) {
	if home == "" {
		return
	}
	statusLogMu.Lock()
	defer statusLogMu.Unlock()
	now := time.Now()
	if now.Sub(lastStatusLog) < time.Second {
		return
	}
	lastStatusLog = now
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	shell.AppendLog(filepath.Join(home, "status.log"), "cmd=%q: %s", cmd, msg)
}

type limitWriter struct {
	w       io.Writer
	max     int
	written int
}

func (l *limitWriter) Write(p []byte) (n int, err error) {
	if l.written >= l.max {
		return len(p), nil
	}
	avail := l.max - l.written
	if len(p) <= avail {
		n, err = l.w.Write(p)
		l.written += n
		return n, err
	}
	n, err = l.w.Write(p[:avail])
	l.written += n
	return len(p), err
}

func sanitizeStatusSegment(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func runSingleStatusCommand(ctx context.Context, cmdStr string, snap StatusSnapshot, sem chan struct{}) (string, error) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	dir := ""
	if fi, err := os.Stat(snap.Cwd); snap.Cwd != "" && err == nil && fi.IsDir() {
		dir = snap.Cwd
	}
	env := os.Environ()
	for k, v := range snap.Environment {
		if !strings.HasSuffix(k, "_FILE") {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range snap.Metadata {
		if !strings.HasSuffix(k, "_FILE") && agent.IsAllowedStatusMetadataKey(k) {
			env = append(env, k+"="+v)
		}
	}
	cmd := shell.Command(cmdCtx, dir, env, cmdStr)
	cmd.WaitDelay = 100 * time.Millisecond

	var buf bytes.Buffer
	cmd.Stdout = &limitWriter{w: &buf, max: 1024}
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		if ctx.Err() == nil && cmdCtx.Err() != context.Canceled && !errors.Is(err, context.Canceled) {
			logStatusFailure(snap.Home, cmdStr, err)
		}
		return "", err
	}
	return sanitizeStatusSegment(buf.String()), nil
}

func evalStatusSnapshot(ctx context.Context, snap StatusSnapshot, sem chan struct{}) (string, string) {
	evalSide := func(cmds []string) string {
		if len(cmds) == 0 {
			return ""
		}
		type res struct {
			idx int
			val string
		}
		ch := make(chan res, len(cmds))
		var wg sync.WaitGroup
		for i, cmdStr := range cmds {
			wg.Add(1)
			go func(i int, cmd string) {
				defer wg.Done()
				val, err := runSingleStatusCommand(ctx, cmd, snap, sem)
				if err != nil || val == "" {
					return
				}
				ch <- res{idx: i, val: val}
			}(i, cmdStr)
		}
		wg.Wait()
		close(ch)
		outputs := make([]string, len(cmds))
		for r := range ch {
			outputs[r.idx] = r.val
		}
		var segments []string
		for _, out := range outputs {
			if out != "" {
				segments = append(segments, out)
			}
		}
		return strings.Join(segments, " · ")
	}

	left := evalSide(snap.LeftCmds)
	right := evalSide(snap.RightCmds)
	return left, right
}

func renderBottomBar(width int, left, right string) string {
	if width <= 0 {
		return ""
	}
	if width < 5 {
		return strings.Repeat(" ", width)
	}
	avail := width - 4
	lw := ansi.StringWidth(left)
	rw := ansi.StringWidth(right)
	var content string
	switch {
	case lw == 0 && rw == 0:
		content = strings.Repeat(" ", avail)
	case rw == 0:
		if lw > avail {
			left = ansi.Truncate(left, avail, "…")
		}
		gap := max(0, avail-ansi.StringWidth(left))
		content = ansi.Truncate(left+strings.Repeat(" ", gap), avail, "")
	case lw == 0:
		if rw > avail {
			right = ansi.TruncateLeft(right, avail, "…")
		}
		gap := max(0, avail-ansi.StringWidth(right))
		content = ansi.Truncate(strings.Repeat(" ", gap)+right, avail, "")
	default:
		if lw+rw+1 > avail {
			right = ansi.TruncateLeft(right, max(0, avail/3), "…")
			availLeft := max(0, avail-ansi.StringWidth(right)-1)
			left = ansi.Truncate(left, availLeft, "…")
		}
		gap := max(0, avail-ansi.StringWidth(left)-ansi.StringWidth(right))
		content = ansi.Truncate(left+strings.Repeat(" ", gap)+right, avail, "")
	}
	return "  " + content + "  "
}

func (m model) fallbackStatus() (string, string) {
	var provider, model, effort string
	if m.ctl != nil {
		s := m.ctl.Settings()
		provider, model, effort = s.Provider, s.Model, s.Effort
	}
	mdl := model
	if effort != "" {
		mdl += "/" + effort
	}
	ctx := "ctx ?"
	if m.contextInput != nil {
		ctx = "ctx ~" + formatContext(*m.contextInput)
	}
	left := provider + " · " + mdl + " · " + ctx
	if provider == "" || model == "" {
		left = "no model selected · " + ctx
	}
	right := shortPath(m.cwd)
	return sanitizeStatusSegment(left), sanitizeStatusSegment(right)
}

func (m model) bottomBar() string {
	left := m.statusLeft
	right := m.statusRight
	if !m.statusInit && left == "" && right == "" {
		left, right = m.fallbackStatus()
	}
	return renderBottomBar(m.width, left, right)
}

func (m *model) triggerStatusRefresh(eventVars map[string]string) tea.Cmd {
	if m.statusTornDown {
		return nil
	}
	m.statusGen++
	if m.statusCancel != nil {
		m.statusCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.statusCancel = cancel

	var settings agent.Settings
	var historyPath string
	if m.ctl != nil {
		settings = m.ctl.Settings()
		historyPath = m.ctl.HistoryPath()
	}

	leftCmds, rightCmds := settings.StatusLeft, settings.StatusRight

	meta := make(map[string]string, len(eventVars)+10)
	for k, v := range eventVars {
		if strings.HasSuffix(k, "_FILE") || !agent.IsAllowedStatusMetadataKey(k) {
			continue
		}
		meta[k] = v
	}
	meta["DASHY_CWD"] = m.cwd
	meta["DASHY_VERSION"] = Version
	meta["DASHY_PROVIDER"] = settings.Provider
	meta["DASHY_MODEL"] = settings.Model
	meta["DASHY_EFFORT"] = settings.Effort
	if m.contextInput != nil {
		meta["DASHY_CTX_SIZE"] = strconv.FormatInt(*m.contextInput, 10)
	} else if _, ok := meta["DASHY_CTX_SIZE"]; !ok {
		meta["DASHY_CTX_SIZE"] = ""
	}
	opState := m.ctl.Status()
	if opState.Cancelling {
		meta["DASHY_STATE"] = "cancelling"
	} else if opState.Busy {
		meta["DASHY_STATE"] = "busy"
	} else {
		meta["DASHY_STATE"] = "idle"
	}
	if _, ok := meta["DASHY_SESSION_ID"]; !ok {
		meta["DASHY_SESSION_ID"] = strings.TrimSuffix(filepath.Base(historyPath), ".jsonl")
	}

	envCopy := make(map[string]string, len(settings.Environment))
	for k, v := range settings.Environment {
		envCopy[k] = v
	}

	home := os.Getenv("DASHY_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".dashy")
		}
	}

	snap := StatusSnapshot{
		Gen:         m.statusGen,
		Cwd:         m.cwd,
		Home:        home,
		LeftCmds:    slices.Clone(leftCmds),
		RightCmds:   slices.Clone(rightCmds),
		Environment: envCopy,
		Metadata:    meta,
	}

	if m.statusSem == nil {
		m.statusSem = make(chan struct{}, 4)
	}
	sem := m.statusSem
	gen := m.statusGen

	return func() tea.Msg {
		left, right := evalStatusSnapshot(ctx, snap, sem)
		if ctx.Err() != nil {
			return nil
		}
		return statusResultMsg{gen: gen, left: left, right: right}
	}
}
