package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"dashy/internal/agent"
	"dashy/internal/control"
	"dashy/internal/shell"
)

type entry = agent.Record
type renderedEntry struct {
	source   entry
	width    int
	maxLines int
	Text     string
}
type model struct {
	ctl                      *control.Controller
	events                   *control.Queue[control.Event]
	input                    textarea.Model
	viewport                 viewport.Model
	entries                  []entry
	images                   []shell.Image
	pasteGeneration          int
	pasting                  bool
	width, height            int
	cwd                      string
	selected                 int
	hideCompletion           bool
	cache                    []renderedEntry
	inputValue               string
	inputWidth, inputHeight  int
	contextInput             *int64
	sel                      *selection
	inputSel                 *composerSelection
	inputDrag                bool
	inputTop                 int
	layoutValue              string
	layoutWidth              int
	layout                   []composerRow
	menuKind                 string
	menu                     []command
	effort, menuGeneration   int
	menuCancel               context.CancelFunc
	pickerModels             []control.ModelInfo
	setup                    []string
	sessions                 []control.SessionInfo
	fileGen, fileDebounceGen int
	fileLoading              bool
	fileCompletions          []string
	fileSelected             int
	fileTokenStart           int
	fileTokenEnd             int
	fileIndexCache           []string
	fileIndexCacheTime       time.Time
	fileIndexCwd             string
	fileScanCancel           context.CancelFunc
	fileDismissed            bool
	fileDismissedValue       string
	fileDismissedCursor      int
}

func newModel(a *agent.Agent, cwd string) model {
	input := textarea.New()
	input.Placeholder = "Ask anything, or type / for commands"
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.CharLimit = 0
	input.MaxHeight = 10000
	input.MaxWidth = 10000
	input.SetHeight(1)
	input.Focus()
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	base := lipgloss.NewStyle().Foreground(foreground)
	style := textarea.Style{Base: base, CursorLine: base, Text: base, Placeholder: base.Foreground(muted), EndOfBuffer: base}
	input.FocusedStyle = style
	input.BlurredStyle = style
	events := control.NewQueue[control.Event](0)
	ctl := control.New(a, func(e control.Event) { events.Push(e) })
	m := model{ctl: ctl, events: events, input: input, viewport: viewport.New(80, 24), width: 80, height: 24, cwd: cwd}
	if a != nil {
	}
	m.setEntries(ctl.History())
	m.refresh(true)
	return m
}

// setEntries replaces the timeline with the controller's display journal.
func (m *model) setEntries(records []entry) {
	m.entries, m.cache, m.contextInput = records, nil, nil
	for _, r := range m.entries {
		if r.Meta != nil && r.Meta.RequestID != "" {
			m.contextInput = r.Meta.ContextInput
		}
	}
}

type followerTick struct{}

func followerPulse() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return followerTick{} })
}
func (m model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		followerPulse(),
	)
}
func (m *model) resizeInput() {
	value, width, height := m.input.Value(), max(1, m.width-2), max(1, m.height/2)
	if m.inputWidth == width && m.inputHeight == height && m.inputValue == value {
		return
	}
	m.inputWidth, m.inputHeight, m.inputValue = width, height, value
	m.input.SetWidth(width)
	lines := 0
	// A textarea copy shares its viewport pointer; probing it resets live scroll.
	probe := textarea.New()
	probe.CharLimit = 0
	probe.MaxWidth = m.input.MaxWidth
	probe.Prompt = ""
	probe.ShowLineNumbers = false
	probe.SetWidth(m.input.Width())
	for _, line := range strings.Split(value, "\n") {
		probe.SetValue(line)
		lines += probe.LineInfo().Height
	}
	m.input.SetHeight(min(height, max(1, lines)))
}

// updateInput runs the textarea update with its maximum height. The textarea
// scrolls its viewport to the cursor during Update using its current height, so
// with a stale smaller height an inserted newline would scroll earlier lines out
// of view before the height is grown.
func (m *model) updateInput(msg tea.Msg) tea.Cmd {
	previous := m.input.Height()
	m.input.SetHeight(max(1, m.height/2))
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.input.SetHeight(previous)
	m.inputValue, m.inputWidth = "\x00", 0 // force resizeInput to recompute height
	return cmd
}
func (m *model) refresh(follow bool) {
	if text := m.input.Value(); !m.hideCompletion && (text == "/resume" || strings.HasPrefix(text, "/resume ")) {
		if m.sessions == nil {
			m.sessions, _ = m.ctl.Sessions()
		}
	} else {
		m.sessions = nil
	}
	m.resizeInput() // Constant-time unless input text or dimensions changed.
	m.viewport.Width = max(1, m.width)
	m.viewport.Height = max(1, m.height)
	m.viewport.SetContent(m.timeline())
	if follow {
		m.viewport.GotoBottom()
	}
}

func (m *model) add(kind, text string) { m.entries = append(m.entries, entry{Kind: kind, Text: text}) }

// stop adds a diagnostic, refreshes the view and ends the update cycle.
func (m *model) stop(kind, text string) (tea.Model, tea.Cmd) {
	m.add(kind, text)
	m.refresh(true)
	return *m, nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	msg = normalizeTerminalKey(msg)
	follow := m.viewport.AtBottom()
	switch v := msg.(type) {
	case fileDebounceMsg:
		return m.handleFileDebounce(v)
	case fileScanMsg:
		return m.handleFileScan(v)
	case clipboardResult:
		valBefore := m.input.Value()
		curBefore := m.composerCursorOffset()
		cmd := m.acceptClipboard(v)
		valAfter := m.input.Value()
		curAfter := m.composerCursorOffset()
		changed := valAfter != valBefore
		moved := curAfter != curBefore
		if changed {
			m.selected = 0
			m.hideCompletion = false
		}
		var fileCmd tea.Cmd
		if changed || moved {
			m.fileDismissed = false
			fileCmd = m.checkFileCompletion()
		}
		m.refresh(true)
		return m, tea.Batch(cmd, fileCmd)
	case followerTick:
		// Notifications that arrive while idle start a turn; busy turns accept them as steering.
		m.ctl.Deliver()
		return m, followerPulse()
	case modelChoices:
		if (m.menuKind != "model" && m.menuKind != "settings") || v.generation != m.menuGeneration {
			return m, nil
		}
		if v.err != nil {
			m.closePicker()
			return m.reject(v.err)
		}
		var failed []string
		for name, err := range v.errs {
			failed = append(failed, name+": "+err)
		}
		sort.Strings(failed)
		if m.menuKind == "settings" {
			m.pickerModels = v.models
			m.rebuildSettingsMenu()
			if len(v.models) == 0 {
				if len(failed) > 0 {
					m.add("error", "Could not list models.\n"+strings.Join(failed, "\n"))
				} else if len(m.ctl.Settings().Providers) > 0 {
					m.add("notice", "No models returned by providers.")
				}
			} else if len(failed) > 0 {
				m.add("notice", "Some providers failed to list models:\n"+strings.Join(failed, "\n"))
			}
		} else {
			if len(v.models) == 0 {
				m.closePicker()
				if len(failed) > 0 {
					m.add("error", "Could not list models.\n"+strings.Join(failed, "\n"))
				} else {
					m.add("notice", "No models returned by providers.")
				}
			} else {
				current := m.ctl.Settings()
				for i, c := range v.models {
					m.menu = append(m.menu, command{name: c.Model, description: c.Provider})
					if c.Provider == current.Provider && c.Model == current.Model {
						m.selected = i
					}
				}
				if len(failed) > 0 {
					m.add("notice", "Some providers failed to list models:\n"+strings.Join(failed, "\n"))
				}
			}
		}
		m.refresh(true)
		return m, nil
	case tea.QuitMsg:
		return m, nil
	case workingTick:
		status := m.ctl.Status()
		if !status.Busy || !v.started.Equal(status.Started) {
			return m, nil
		}
		m.refresh(follow)
		return m, workingPulse(status.Started)

	case tea.WindowSizeMsg:
		m.width, m.height = max(1, v.Width), max(1, v.Height)
		m.refresh(follow)
		return m, nil
	case tea.MouseMsg:
		if cmd, handled := m.mouseSelection(v); handled {
			m.refresh(false)
			return m, cmd
		}
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	case composerShortcut:
		key := tea.KeyMsg{Type: tea.KeyCtrlC}
		if v.cut {
			key = tea.KeyMsg{Type: tea.KeyCtrlX}
		}
		if cmd, handled := m.composerKey(key); handled {
			return m, cmd
		}
		return m, nil
	case clipboardCopied:
		if v.err != nil {
			return m.stop("error", "Copy failed: "+v.err.Error())
		}
		return m.stop("notice", fmt.Sprintf("Copied %d lines to the clipboard.", v.lines))
	case control.Event:
		return m.handleControl(v)
	case tea.KeyMsg:
		if next, cmd, handled := m.pickerKey(v); handled {
			return next, cmd
		}
		if cmd, handled := m.composerKey(v); handled {
			return m, cmd
		}
		switch v.String() {
		case "ctrl+v":
			return m.pasteClipboard()
		case "ctrl+x":
			if len(m.images) > 0 {
				image := m.images[len(m.images)-1]
				if image.Path != "" {
					_ = os.Remove(image.Path)
				}
				m.images = m.images[:len(m.images)-1]
				m.refresh(true)
				return m, nil
			}
		case "ctrl+c":
			if m.ctl.Status().Busy {
				m.requestCancel()
				return m, nil
			}
			return m.executeCommand("/quit")
		case "ctrl+d":
			if !m.ctl.Status().Busy && m.input.Value() == "" {
				return m.executeCommand("/quit")
			}
		case "esc":
			if m.fileLoading || m.hasFileCompletions() {
				m.dismissFileCompletion()
				m.refresh(true)
				return m, nil
			}
			if len(m.completions()) == 0 && m.ctl.Status().Busy {
				m.requestCancel()
			}
			m.hideCompletion = true
			m.refresh(true)
			return m, nil
		case "pgup", "pgdown", "ctrl+up", "ctrl+down", "ctrl+home", "ctrl+end":
			switch v.String() {
			case "ctrl+home":
				m.viewport.GotoTop()
			case "ctrl+end":
				m.viewport.GotoBottom()
			case "ctrl+up":
				m.viewport.ScrollUp(1)
			case "ctrl+down":
				m.viewport.ScrollDown(1)
			default:
				m.viewport, _ = m.viewport.Update(msg)
			}
			return m, nil
		case "tab":
			if m.hasFileCompletions() {
				cmd := m.acceptFileCompletion()
				m.refresh(true)
				return m, cmd
			}
			if m.complete() {
				m.refresh(true)
				return m, nil
			}
		case "up", "down":
			delta := 1
			if v.String() == "up" {
				delta = -1
			}
			if m.hasFileCompletions() {
				n := len(m.fileCompletions)
				m.fileSelected = (m.fileSelected + delta + n) % n
				m.refresh(true)
				return m, nil
			}
			if matches := m.completions(); len(matches) > 0 {
				m.selected = (m.selected + delta + len(matches)) % len(matches)
				m.refresh(true)
				return m, nil
			}
			if v.String() == "up" && m.input.Value() == "" && len(m.images) == 0 && len(m.ctl.Status().Queued) > 0 {
				in, err := m.ctl.Recall()
				if err != nil {
					return m.reject(err)
				}
				if in != nil {
					m.input.SetValue(in.Text)
					m.images = in.Images
					m.input.CursorEnd()
					m.refresh(true)
					return m, nil
				}
			}
		case "enter":
			if m.pasting {
				return m.stop("notice", "Clipboard read in progress; wait before submitting.")
			}

			if m.hasFileCompletions() {
				cmd := m.acceptFileCompletion()
				m.refresh(true)
				return m, cmd
			}

			text := strings.TrimSpace(m.input.Value())
			if text == "" && len(m.images) == 0 {
				return m, nil
			}
			if matches := m.completions(); len(matches) > 0 && len(m.images) == 0 {
				return m.executeCommand(matches[m.selected%len(matches)].name)
			}
			return m.submitInput(text)

		}
		valBefore := m.input.Value()
		curBefore := m.composerCursorOffset()
		cmd := m.updateInput(msg)
		valAfter := m.input.Value()
		curAfter := m.composerCursorOffset()
		changed := valAfter != valBefore
		moved := curAfter != curBefore
		if changed {
			m.selected = 0
			m.hideCompletion = false
		}
		var fileCmd tea.Cmd
		if changed || moved {
			m.fileDismissed = false
			fileCmd = m.checkFileCompletion()
		}
		m.refresh(follow || changed)
		return m, tea.Batch(cmd, fileCmd)
	}
	valBefore := m.input.Value()
	curBefore := m.composerCursorOffset()
	cmd := m.updateInput(msg)
	valAfter := m.input.Value()
	curAfter := m.composerCursorOffset()
	changed := valAfter != valBefore
	moved := curAfter != curBefore
	var fileCmd tea.Cmd
	if changed || moved {
		m.fileDismissed = false
		fileCmd = m.checkFileCompletion()
	}
	m.refresh(follow || changed)
	return m, tea.Batch(cmd, fileCmd)
}
func (m model) View() string {
	lines := strings.Split(m.viewport.View(), "\n")
	if m.sel != nil {
		lines = m.sel.highlight(lines, m.viewport.Width)
	}
	return strings.Join(m.composerHighlight(lines), "\n")
}

// submitInput sends the composer as input.
func (m model) submitInput(text string) (tea.Model, tea.Cmd) {
	return m.sendInput(agent.Input{Text: text, Images: m.images})
}

// sendInput hands a complete input to the controller. Idle input starts a turn;
// input during a turn is queued as steering. The composer clears only once accepted.
func (m model) sendInput(input agent.Input) (tea.Model, tea.Cmd) {
	if err := m.ctl.Input(input); err != nil {
		return m.reject(err)
	}
	m.input.Reset()
	m.images = nil
	m.hideCompletion = false
	m.dismissFileCompletion()
	m.refresh(true)
	return m, nil
}

// requestCancel stops the active operation; the cancellation completes later as a done event.
func (m *model) requestCancel() {
	m.ctl.Stop()
	m.refresh(false)
}

func (m model) executeCommand(name string) (tea.Model, tea.Cmd) {
	return m.runCommand(name)
}
