package tui

import (
	"cmp"
	"fmt"
	"strings"
	"time"

	"dashy/internal/agent"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	cyan            = lipgloss.Color("#79d9ff")
	foreground      = lipgloss.Color("#d5ddeb")
	muted           = lipgloss.Color("#8794a8")
	codeGray        = lipgloss.Color("#a0a0a0")
	darkGray        = lipgloss.Color("#606060")
	inputBlue       = lipgloss.Color("#8aadff")
	inputBackground = lipgloss.Color("#202020")
	assistantColor  = lipgloss.Color("#9bdef5")
	errorColor      = lipgloss.Color("#f09eac")
	warnColor       = lipgloss.Color("#e5c07b")
	infoColor       = lipgloss.Color("#d7c38b")
	markdownStyle   = fmt.Sprintf(markdownTemplate, assistantColor, cyan, warnColor, muted,
		infoColor, errorColor, inputBlue, inputBlue)
)

func eventColors(kind string) (fg lipgloss.TerminalColor, label string) {
	switch kind {
	case "user":
		return foreground, ""
	case "assistant":
		return assistantColor, "*"
	case "code":
		return codeGray, ">_"
	case "reasoning":
		return darkGray, "~"
	case "output":
		return darkGray, "<"
	case "error":
		return errorColor, "<"
	case "notice":
		return muted, "!"
	default:
		return infoColor, "info"
	}
}

type paintKey struct {
	row    string
	width  int
	bg, fg lipgloss.TerminalColor
	color  any
}

var painted = map[paintKey]string{}

func paint(text string, width int, bg, fg lipgloss.TerminalColor) string {
	rows := strings.Split(text, "\n")
	style := lipgloss.NewStyle().Width(width).Background(bg).Foreground(fg)
	for i, row := range rows {
		key := paintKey{row, width, bg, fg, lipgloss.ColorProfile()}
		if out, ok := painted[key]; ok {
			rows[i] = out
			continue
		}
		if len(painted) >= 8192 {
			clear(painted)
		}
		rows[i] = style.Render(ansi.Truncate(row, max(1, width), ""))
		painted[key] = rows[i]
	}
	return strings.Join(rows, "\n")
}

func renderEntryMode(e entry, width, limit int, markdown bool) string {
	if limit == 0 {
		return ""
	}
	fg, label := eventColors(e.Kind)

	text := ansi.Strip(e.Text)
	if e.Kind == "notice" {
		text = cmp.Or(agent.NoticeSummary(text), "Notice received.")
		return paint(ansi.Truncate("  "+label+" · "+text, max(1, width), "…"), max(1, width), lipgloss.NoColor{}, fg)
	}

	if e.Kind == "user" {
		return renderRail(ansi.Wrap(text, max(1, width-2), ""), width)
	}
	body, ok := "", false
	if e.Kind == "assistant" && markdown {
		body, ok = renderMarkdown(text, width-4, e.Preview)
	}
	if !ok {
		body = ansi.Wrap(text, max(1, width-4), "")
	}
	if e.Preview && e.Kind == "code" {
		label += " · preview"
	}
	if e.Kind == "assistant" {
		if metadata := answerMetadata(e); metadata != "" {
			label += " " + metadata
		}
	} else {
		label += formatMetadata(e.Meta, e.Kind)
	}
	if body == "" && e.Kind == "code" {
		return paint(ansi.Truncate("  "+label, max(1, width), "…"), width, lipgloss.NoColor{}, fg)
	}
	if body == "" {
		body = "(no output)"
	}
	lines := linkedRows(body, max(1, width-2))
	if limit > 0 && len(lines) > limit {
		lines = lines[:limit]
	}
	for i, line := range lines {
		lines[i] = ansi.Truncate(strings.Repeat(" ", min(2, max(0, width-1)))+line, max(1, width), "…")
	}
	out := paint(ansi.Truncate("  "+label, max(1, width), "…"), width, lipgloss.NoColor{}, fg) + "\n" + paint(strings.Join(lines, "\n"), width, lipgloss.NoColor{}, bodyColor(e.Kind))
	return out
}

func answerMetadata(e entry) string {
	if e.Kind != "assistant" || e.Preview || e.Meta == nil || e.Meta.At.IsZero() {
		return ""
	}
	at, now := e.Meta.At.Local(), time.Now()
	clock := at.Format("15:04")
	if at.YearDay() != now.YearDay() || at.Year() != now.Year() {
		clock = at.Format("Jan 2 15:04")
	}
	return clock + " · " + formatDuration(e.Meta.Duration) + " · " + strings.TrimSpace(formatMetadata(e.Meta, "assistant"))
}
func bodyColor(kind string) lipgloss.TerminalColor {
	switch kind {
	case "code":
		return codeGray
	case "error":
		return errorColor
	case "output", "reasoning":
		return darkGray
	}
	return foreground
}

func renderRail(text string, width int) string {
	lines := strings.Split(text, "\n")
	rail := lipgloss.NewStyle().Foreground(inputBlue).Background(inputBackground).Render("▌")
	// The textarea viewport pads its placeholder with resets before spaces.
	// Restore the input background after those resets as well.
	background := ""
	if sequence := lipgloss.ColorProfile().Color(string(inputBackground)).Sequence(true); sequence != "" {
		background = "\x1b[" + sequence + "m"
	}
	for i, line := range lines {
		line = strings.NewReplacer("\x1b[0m", "\x1b[0m"+background, "\x1b[m", "\x1b[m"+background, "\x1b[49m", background).Replace(line)
		body := paint(" "+line, max(1, width-1), inputBackground, foreground)
		lines[i] = ansi.Truncate(rail+body, max(1, width), "")
	}
	return strings.Join(lines, "\n")
}
func (m model) inputBlock() string {
	if m.keyInComposer() {
		return renderRail(strings.Repeat("•", len([]rune(m.input.Value()))), m.width)
	}
	return renderRail(m.input.View(), m.width)
}

func (m model) pickerHint() string {
	switch {
	case m.confirmDelete != "":
		return "Delete " + m.confirmDelete + "? d: delete · Esc: keep"
	case m.menuKind == pickerTasks:
		return "Enter: show output · k: stop · Esc: close"
	case m.menuKind == pickerConnect:
		return "Enter: use · e: edit · d: delete · Esc: close"
	case m.menuKind == pickerProvider:
		save := "Enter: save"
		if m.form.failedAsTyped(m.input.Value()) {
			save = "Enter: save anyway"
		}
		return "Tab: next field · " + save + " · Esc: " + map[bool]string{true: "skip", false: "back"}[m.form.welcome]
	}
	return ""
}

func (m model) keyInComposer() bool { return m.form != nil && m.form.field == 2 }

type screenRows []string

func (r *screenRows) add(chunk string) { r.addLines(strings.Split(chunk, "\n")) }

func (r *screenRows) addLines(lines []string) {
	if len(*r) == 0 {
		*r = append(*r, lines...)
		return
	}
	(*r)[len(*r)-1] += lines[0]
	*r = append(*r, lines[1:]...)
}

func (m *model) timelineRows(budget time.Duration) []string {
	var out strings.Builder
	out.WriteString(lipgloss.NewStyle().Foreground(cyan).Bold(true).Render("dashy. minimal coding agent."))
	out.WriteString(lipgloss.NewStyle().Foreground(muted).Render(" v" + strings.TrimPrefix(m.version, "v")))
	out.WriteString("\n")
	if hint := m.ctl.Settings().SetupHint(); hint != "" && m.form == nil {
		out.WriteString(lipgloss.NewStyle().Foreground(warnColor).Render(hint) + "\n")
	}
	for _, path := range m.ctl.Instructions() {
		row := "DASHY.md " + shortPath(path)
		out.WriteString(lipgloss.NewStyle().Foreground(muted).Render(ansi.Truncate(row, max(1, m.width), "…")) + "\n")
	}
	out.WriteString("\n")
	var screen screenRows
	screen.add(out.String())
	if len(m.cache) > len(m.entries) {
		m.cache = m.cache[:len(m.entries)]
	}
	for len(m.cache) < len(m.entries) {
		m.cache = append(m.cache, renderedEntry{})
	}
	settings := m.ctl.Settings()
	started, rendered := time.Now(), 0
	m.renderPending = false
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := m.entries[i]
		limit := -1
		switch e.Kind {
		case "code", "output", "reasoning":
			limit = settings.MaxLines(e.Kind)
		case "error":
			limit = settings.MaxLines("output")
		}
		if settings.ShowLines == "minimal" && e.Kind != "user" && e.Kind != "assistant" {
			limit = 0
		}
		cached := &m.cache[i]
		if cached.width == m.width && cached.source == e && cached.maxLines == limit && !cached.draft {
			continue
		}
		draft := budget > 0 && e.Kind == "assistant" && rendered > 0 && time.Since(started) >= budget
		if draft {
			m.renderPending = true
			if cached.draft && cached.width == m.width && cached.source == e && cached.maxLines == limit {
				continue
			}
		}
		cached.source, cached.width, cached.maxLines, cached.draft = e, m.width, limit, draft
		cached.Text = renderEntryMode(e, m.width, limit, !draft)
		cached.rows = strings.Split(cached.Text, "\n")
		if e.Kind == "assistant" && !draft {
			rendered++
		}
	}
	for _, cached := range m.cache {
		if cached.Text != "" {
			screen.addLines(cached.rows)
			screen.add("\n\n")
		}
	}
	screen.add(m.workingBlock())
	m.inputTop = len(screen) - 1
	screen.add(m.inputBlock())
	fileMode := m.hasFileCompletions()
	matches := m.completions()
	count, selected := len(matches), m.selected
	if fileMode {
		count, selected = len(m.fileCompletions), m.fileSelected
	}
	start, rows := 0, min(8, max(1, m.height-m.input.Height()-5))
	if count > 0 {
		start = max(0, selected%count-rows+1)
	}
	if !fileMode && m.menuKind == pickerModels {
		parts := make([]string, len(agent.Efforts))
		for i, e := range agent.Efforts {
			parts[i] = " " + e + " "
			if i == m.effort {
				parts[i] = "[" + e + "]"
			}
		}
		screen.add("\n" + paint(ansi.Truncate("  Effort: "+strings.Join(parts, " ")+"  (Left/Right)", m.width, "…"), m.width, lipgloss.NoColor{}, foreground))
	}
	for i := start; i < min(count, start+rows); i++ {
		var bg lipgloss.TerminalColor = lipgloss.NoColor{}
		fg, marker := muted, "  "
		if i == selected%count {
			bg, fg, marker = lipgloss.Color("#303030"), foreground, "› "
		}
		var row string
		if fileMode {
			row = marker + sanitizeLabel(m.fileCompletions[i])
		} else {
			cmd := matches[i]
			row = fmt.Sprintf("%s%-2s %-9s %s", marker, cmd.icon, cmd.name, cmd.description)
			if m.menuKind == pickerSettings || m.menuKind == pickerModels || m.menuKind == pickerTasks || m.menuKind == pickerConnect {
				if cmd.label != "" {
					row = marker + cmd.label
				} else if cmd.description != "" {
					row = marker + cmd.name + "  " + cmd.description
				} else {
					row = marker + cmd.name
				}
			} else if cmd.label != "" {
				row = marker + cmd.icon + " " + cmd.label
			}
		}
		screen.add("\n" + paint(ansi.Truncate(row, m.width, "…"), m.width, bg, fg))
	}
	if m.form != nil {
		rows, errRow := m.formRows()
		for i, row := range rows {
			fg := muted
			if i == 0 || strings.HasPrefix(row, "›") {
				fg = foreground
			}
			screen.add("\n" + paint(ansi.Truncate("  "+row, m.width, "…"), m.width, lipgloss.NoColor{}, fg))
		}
		if errRow != "" {
			for _, line := range strings.Split(ansi.Wrap(errRow, max(1, m.width-2), ""), "\n") {
				screen.add("\n" + paint("  "+line, m.width, lipgloss.NoColor{}, errorColor))
			}
		}
	}
	if hint := m.pickerHint(); hint != "" && !fileMode {
		screen.add("\n" + paint(ansi.Truncate("  "+hint, m.width, "…"), m.width, lipgloss.NoColor{}, muted))
	}
	screen.add("\n\n" + m.bottomBar())
	return screen
}
