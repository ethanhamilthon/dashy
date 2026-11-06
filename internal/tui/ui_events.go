package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"dashy/internal/agent"
	"dashy/internal/control"
)

// Run runs the terminal interface until the user exits.
// The caller remains responsible for closing the agent and releasing session ownership.
func Run(ctx context.Context, a *agent.Agent, cwd, version string) error {
	keyboard := keyboardProtocol{}
	defer keyboard.close()
	m := newModel(a, cwd)
	m.version = version
	m.startCmd = m.onboard()
	termOut = os.Stdout
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithFilter(keyboard.filter))
	running, stop := context.WithCancel(ctx)
	defer stop()
	go forwardControl(running, m.events, program)
	go func() {
		<-running.Done()
		program.Quit()
	}()
	final, err := program.Run()
	keyboard.close()
	stop()
	if m, ok := final.(model); ok {
		m.term.reset()
		m.ctl.Close()
		if m.menuCancel != nil {
			m.menuCancel()
		}
		if m.fileScanCancel != nil {
			m.fileScanCancel()
		}
	}
	return err
}

func forwardControl(ctx context.Context, events *eventQueue, program *tea.Program) {
	for {
		event, ok := events.Pop(ctx)
		if !ok {
			return
		}
		program.Send(event)
	}
}

type workingTick struct{ started time.Time }

func workingPulse(started time.Time) tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return workingTick{started} })
}

func (m model) workingBlock() string {
	status := m.ctl.Status()
	var block strings.Builder
	for _, input := range status.Queued {
		row := "  queued · " + strings.ReplaceAll(ansi.Strip(input.Display()), "\n", " ")
		block.WriteString(lipgloss.NewStyle().Foreground(muted).Render(ansi.Truncate(row, max(1, m.width), "…")) + "\n")
	}
	if status.Busy {
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		elapsed := max(time.Duration(0), time.Since(status.Started))
		label := "working"
		hint := " · esc to stop"
		if status.Cancelling {
			label, hint = "cancelling", ""
		}
		line := "  " + lipgloss.NewStyle().Foreground(inputBlue).Render(frames[int(elapsed/(80*time.Millisecond))%len(frames)]) +
			lipgloss.NewStyle().Foreground(muted).Render(" "+label+" · "+formatDuration(elapsed)+tasksRunning(status.Tasks, " · ")+hint)
		block.WriteString(ansi.Truncate(line, max(1, m.width), "…") + "\n\n")
	} else if status.Tasks > 0 {
		line := "  " + lipgloss.NewStyle().Foreground(inputBlue).Render("●") + lipgloss.NewStyle().Foreground(muted).Render(tasksRunning(status.Tasks, " "))
		block.WriteString(ansi.Truncate(line, max(1, m.width), "…") + "\n\n")
	}
	return block.String()
}

func tasksRunning(n int, sep string) string {
	if n == 0 {
		return ""
	}
	return sep + fmt.Sprint(n) + map[bool]string{true: " task", false: " tasks"}[n == 1] + " running"
}

func (m model) handleControl(e control.Event) (tea.Model, tea.Cmd) {
	follow := m.viewport.AtBottom()
	var cmd tea.Cmd
	switch e.Kind {
	case "started":
		m.term.start(e.Title)
	case "done":
		m.term.finish(e.Status, e.Title)
	case "session":
		m.term.state = "idle"
		fallthrough
	default:
		m.term.rename(e.Title)
	}
	switch e.Kind {
	case "started":
		m.refresh(true)
		cmd = workingPulse(m.ctl.Status().Started)
	case "agent":
		if time.Since(m.frameAt) >= frameInterval {
			m.refresh(follow)
		} else {
			m.dirty = true
		}
	case "notice", "error":
		m.setFlash(e.Kind, e.Text)
	case "session":
		m.cache = nil
		m.pasteGeneration++
		m.pasting = false
		m.dismissFileCompletion()
		m.fileIndexCache = nil
		m.refresh(true)
	case "done":
		if len(e.Unsent) > 0 {
			m.restoreUnsent(e.Unsent)
		}
		m.refresh(follow)
	}
	select {
	case <-m.ctl.Closed():
		return m, tea.Quit
	default:
	}
	return m, cmd
}

func (m *model) restoreUnsent(unsent []agent.Input) {
	var texts []string
	for _, in := range unsent {
		if in.Text != "" {
			texts = append(texts, in.Text)
		}
	}
	if draft := m.input.Value(); draft != "" {
		texts = append(texts, draft)
	}
	m.setComposer(strings.Join(texts, "\n\n"))
}

func (m *model) setComposer(text string) {
	m.input.SetValue(text)
	m.selected, m.hideCompletion = 0, false
	m.dismissFileCompletion()
}
