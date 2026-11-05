package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"dashy/internal/agent"
	"dashy/internal/control"
	"dashy/internal/osc7501"
)

type entry = agent.Record
type renderedEntry struct {
	source   entry
	width    int
	maxLines int
	draft    bool // plain text standing in for Markdown not rendered yet
	Text     string
	rows     []string
}
type model struct {
	ctl                      *control.Controller
	events                   *eventQueue
	input                    textarea.Model
	viewport                 pager
	entries                  []entry // copy of the controller's timeline, refreshed by refresh
	pasteGeneration          int
	pasting                  bool
	width, height            int
	cwd                      string
	selected                 int
	hideCompletion           bool
	cache                    []renderedEntry
	inputValue               string
	inputWidth, inputHeight  int
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
	pickerModels             []agent.ModelChoice
	setup                    []string
	sessions                 []agent.SessionSummary
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
	reporter                 *osc7501.Reporter
	flash                    entry // the latest notice or error, shown in the status bar
	flashAt                  time.Time
	frameAt                  time.Time // the last refresh
	dirty                    bool      // agent events arrived since frameAt
	renderPending            bool      // some Markdown is still shown as plain text
	redrawScheduled          bool
}

// Streaming events refresh at most once per frameInterval; renderBudget bounds the
// Markdown rendered by one refresh so a resize or a resumed session never freezes.
const (
	frameInterval = 33 * time.Millisecond
	renderBudget  = 25 * time.Millisecond
)

type redraw struct{}

func newModel(a *agent.Agent, cwd string, reporter *osc7501.Reporter) model {
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
	input.FocusedStyle, input.BlurredStyle = style, style
	events := newEventQueue()
	ctl := control.New(a, func(e control.Event) { events.Push(e) })
	m := model{ctl: ctl, events: events, input: input, viewport: pager{Width: 80, Height: 24}, width: 80, height: 24, cwd: cwd, reporter: reporter}
	reporter.HandleEvent(control.Event{Kind: "session", Title: ctl.Title()})
	m.refresh(true)
	return m
}

func (m model) Init() tea.Cmd { return textarea.Blink }
func (m *model) resizeInput() {
	value, width, height := m.input.Value(), max(1, m.width-2), max(1, m.height/2)
	if m.inputWidth == width && m.inputHeight == height && m.inputValue == value {
		return
	}
	m.inputWidth, m.inputHeight, m.inputValue = width, height, value
	m.input.SetWidth(width)
	lines := 0
	for _, line := range strings.Split(value, "\n") {
		lines += len(wrapRows([]rune(line), m.input.Width()))
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
	m.entries = m.ctl.History() // the controller's timeline is the only one; this is a view of it
	m.resizeInput()             // Constant-time unless input text or dimensions changed.
	m.viewport.Width = max(1, m.width)
	m.viewport.Height = max(1, m.height)
	m.viewport.SetLines(m.timelineRows(renderBudget))
	m.frameAt, m.dirty = time.Now(), false
	if follow {
		m.viewport.GotoBottom()
	}
}

// add records a message of the interface: info goes to the timeline, notices and errors to the status bar.
func (m *model) add(kind, text string) {
	m.ctl.Note(agent.Event{Kind: kind, Text: text})
	if kind != "info" {
		m.setFlash(kind, text)
	}
}

func (m *model) setFlash(kind, text string) {
	m.flash, m.flashAt = entry{Kind: kind, Text: shortEvent(kind, text)}, time.Now()
}

// flashLabels maps the start of an event to its status bar label; /events keeps the full text.
var flashLabels = [][2]string{
	{"Copied", "copied"}, {"Copy failed", "copy failed"}, {"Selected ", "model changed"},
	{"Provider created", "provider added"}, {"Session renamed", "renamed"}, {"Session title reset", "renamed"},
	{"Turn cancelled", "cancelled"}, {"Retrying", "retrying"}, {"Settings and DASHY.md reloaded", "reloaded"},
	{"Instructions will reload", "reload queued"}, {"No provider", "no provider"}, {"No model selected", "no model"},
	{"No models", "no models"}, {"Some providers failed", "models failed"}, {"Could not list models", "models failed"},
	{"No saved sessions", "no sessions"}, {"Unknown command", "unknown command"}, {"Unexpected command", "bad arguments"},
	{"Clipboard read", "pasting"}, {"Steering queue is full", "queue full"}, {"Session is changing", "busy"},
	{"Wait for", "busy"}, {"That instruction was already", "already sent"}, {"Stored settings changed", "settings error"},
	{"session not found", "not found"}, {"clipboard", "paste failed"},
}

// shortEvent is the one or two word label of a notice or error for the status bar.
func shortEvent(kind, text string) string {
	for _, l := range flashLabels {
		if strings.HasPrefix(text, l[0]) {
			return l[1]
		}
	}
	return map[bool]string{true: "error", false: "notice"}[kind == "error"] + " · /events"
}

type flashExpired struct{ at time.Time }

// Update hides the status bar event 10 seconds after the latest one and schedules
// the next frame while streamed events or Markdown rendering are pending.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	n, ok := next.(model)
	if !ok {
		return next, cmd
	}
	if !n.flashAt.Equal(m.flashAt) {
		cmd = tea.Batch(cmd, tea.Tick(10*time.Second, func(time.Time) tea.Msg { return flashExpired{n.flashAt} }))
	}
	if (n.dirty || n.renderPending) && !n.redrawScheduled {
		n.redrawScheduled = true
		wait := max(time.Millisecond, frameInterval-time.Since(n.frameAt))
		cmd = tea.Batch(cmd, tea.Tick(wait, func(time.Time) tea.Msg { return redraw{} }))
	}
	return n, cmd
}

// stop adds a diagnostic, refreshes the view and ends the update cycle.
func (m *model) stop(kind, text string) (tea.Model, tea.Cmd) {
	m.add(kind, text)
	m.refresh(true)
	return *m, nil
}

func (m model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	msg = normalizeTerminalKey(msg)
	follow := m.viewport.AtBottom()
	switch v := msg.(type) {
	case flashExpired:
		if v.at.Equal(m.flashAt) {
			m.flash = entry{}
		}
		return m, nil
	case fileDebounceMsg:
		return m.handleFileDebounce(v)
	case fileScanMsg:
		return m.handleFileScan(v)
	case clipboardResult:
		return m, m.edit(true, func() tea.Cmd { return m.acceptClipboard(v) })
	case modelChoices:
		if m.menuKind != pickerModels || v.generation != m.menuGeneration {
			return m, nil
		}
		if v.err != nil {
			m.closePicker()
			return m.reject(v.err)
		}
		m.pickerModels = v.models
		m.rebuildModelsMenu()
		var failed []string
		for name, err := range v.errs {
			failed = append(failed, name+": "+err)
		}
		sort.Strings(failed)
		switch {
		case len(v.models) == 0 && len(failed) > 0:
			m.add("error", "Could not list models.\n"+strings.Join(failed, "\n"))
		case len(m.ctl.Settings().Providers) == 0:
			m.add("notice", "No providers yet. Use /connect to add one.")
		case len(v.models) == 0:
			m.add("notice", "No models returned by providers.")
		case len(failed) > 0:
			m.add("notice", "Some providers failed to list models:\n"+strings.Join(failed, "\n"))
		}
		m.refresh(true)
		return m, nil
	case redraw:
		m.redrawScheduled = false
		if m.dirty || m.renderPending {
			m.refresh(follow)
		}
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
		cmd, _ := m.composerKey(key)
		return m, cmd
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
		case "ctrl+c":
			if m.ctl.Status().Busy {
				m.requestCancel()
				return m, nil
			}
			return m.runCommand("/quit")
		case "ctrl+d":
			if !m.ctl.Status().Busy && m.input.Value() == "" {
				return m.runCommand("/quit")
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
			if v.String() == "up" && m.input.Value() == "" && len(m.ctl.Status().Queued) > 0 {
				in, err := m.ctl.Recall()
				if err != nil {
					return m.reject(err)
				}
				if in != nil {
					m.setComposer(in.Text)
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
			if text == "" {
				return m, nil
			}
			if matches := m.completions(); len(matches) > 0 {
				return m.runCommand(matches[m.selected%len(matches)].name)
			}
			return m.sendInput(agent.Input{Text: text})
		}
		return m, m.edit(follow, func() tea.Cmd { return m.updateInput(msg) })
	}
	return m, m.edit(follow, func() tea.Cmd { return m.updateInput(msg) })
}

// edit applies a text change to the composer. Changed text restarts the completion
// menu, and changed text or cursor movement rechecks the @path completion.
func (m *model) edit(follow bool, change func() tea.Cmd) tea.Cmd {
	valBefore, curBefore := m.input.Value(), m.composerCursorOffset()
	cmd := change()
	changed := m.input.Value() != valBefore
	moved := m.composerCursorOffset() != curBefore
	if changed {
		m.selected, m.hideCompletion = 0, false
	}
	var fileCmd tea.Cmd
	if changed || moved {
		m.fileDismissed = false
		fileCmd = m.checkFileCompletion()
	}
	m.refresh(follow || changed)
	return tea.Batch(cmd, fileCmd)
}
func (m model) View() string {
	lines := strings.Split(m.viewport.View(), "\n")
	if m.sel != nil {
		lines = m.sel.highlight(lines, m.viewport.Width)
	}
	return strings.Join(m.composerHighlight(lines), "\n")
}

// sendInput hands a complete input to the controller. Idle input starts a turn;
// input during a turn is queued as steering. The composer clears only once accepted.
func (m model) sendInput(input agent.Input) (tea.Model, tea.Cmd) {
	if err := m.ctl.Input(input); err != nil {
		return m.reject(err)
	}
	m.input.Reset()
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
