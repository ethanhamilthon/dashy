package tui

import (
	"context"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"dashy/internal/agent"
	"dashy/internal/control"
	"dashy/internal/osc7501"
	"dashy/internal/shell"
)

// Version is shown on the start screen; main sets it from the VERSION file.
var Version = "dev"

// Run runs the terminal interface until the user exits.
// The caller remains responsible for closing the agent and releasing session ownership.
func Run(ctx context.Context, a *agent.Agent, cwd string) error {
	keyboard := keyboardProtocol{}
	defer keyboard.close()
	reporter := osc7501.New(os.Stdout, osc7501.Enabled(os.Stdout))
	defer reporter.Close()
	m := newModel(a, cwd, reporter)
	program := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithFilter(keyboard.filter))
	running, stop := context.WithCancel(ctx)
	defer stop()
	go forwardControl(running, m.events, program)
	// A signal ends the interface through the program's own exit path.
	go func() {
		<-running.Done()
		program.Quit()
	}()
	final, err := program.Run()
	keyboard.close()
	stop()
	if m, ok := final.(model); ok {
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

// forwardControl delivers controller events to the program in the order emitted.
func forwardControl(ctx context.Context, events *control.Queue[control.Event], program *tea.Program) {
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

// workingBlock renders queued steering and the spinner for the active operation.
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
		if status.Compacting {
			label = "compacting"
		}
		hint := " · esc to stop"
		if status.Cancelling {
			label, hint = "cancelling", ""
		}
		line := "  " + lipgloss.NewStyle().Foreground(inputBlue).Render(frames[int(elapsed/(80*time.Millisecond))%len(frames)]) +
			lipgloss.NewStyle().Foreground(muted).Render(" "+label+" · "+formatDuration(elapsed)+hint)
		block.WriteString(ansi.Truncate(line, max(1, m.width), "…") + "\n\n")
	}
	return block.String()
}

// handleControl applies one controller event to the interface.
func (m model) handleControl(e control.Event) (tea.Model, tea.Cmd) {
	m.reporter.HandleEvent(e)
	follow := m.viewport.AtBottom()
	var cmd tea.Cmd
	switch e.Kind {
	case "started":
		m.refresh(true)
		cmd = workingPulse(m.ctl.Status().Started)
	case "agent":
		m.applyAgentEvent(*e.Agent)
		m.refresh(follow)
	case "notice", "error":
		m.add(e.Kind, e.Text)
		m.refresh(true)
	case "session":
		m.setEntries(e.Records)
		if e.Operation != "compact" {
			// A session change starts a fresh composer context, as /new and /resume always have.
			m.pasteGeneration++
			m.pasting = false
			m.dismissFileCompletion()
			m.fileIndexCache = nil
		}
		m.refresh(true)
	case "done":
		if e.Operation == "compact" && e.Message != "" {
			m.add("notice", e.Message)
		}
		if len(e.Unsent) > 0 {
			m.restoreUnsent(e.Unsent)
		}
		m.refresh(follow)
	}
	if m.ctl.Closed() != nil {
		select {
		case <-m.ctl.Closed():
			return m, tea.Quit
		default:
		}
	}
	return m, cmd
}

// restoreUnsent puts instructions the model did not accept back into the composer,
// ahead of the text already typed there.
func (m *model) restoreUnsent(unsent []agent.Input) {
	var texts []string
	var images []shell.Image
	for _, in := range unsent {
		if in.Text != "" {
			texts = append(texts, in.Text)
		}
		images = append(images, in.Images...)
	}
	if draft := m.input.Value(); draft != "" {
		texts = append(texts, draft)
	}
	images = append(images, m.images...)
	m.images = images
	m.input.SetValue(strings.Join(texts, "\n\n"))
}

func (m *model) applyAgentEvent(e agent.Event) {
	m.entries = agent.ApplyEvent(m.entries, e)
}
