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
	cyan           = lipgloss.Color("#79d9ff")
	foreground     = lipgloss.Color("#d5ddeb")
	muted          = lipgloss.Color("#8794a8")
	codeGray       = lipgloss.Color("#a0a0a0")
	darkGray       = lipgloss.Color("#606060")
	inputBlue      = lipgloss.Color("#8aadff")
	assistantColor = lipgloss.Color("#9bdef5")
	errorColor     = lipgloss.Color("#f09eac")
	warnColor      = lipgloss.Color("#e5c07b")
	infoColor      = lipgloss.Color("#d7c38b")
	// The Markdown style uses the same fixed colors as the timeline.
	markdownStyle = fmt.Sprintf(markdownTemplate, assistantColor, cyan, warnColor, muted,
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
	color  any // the color profile
}

// painted memoizes paint per row: a streamed message repaints mostly unchanged rows.
var painted = map[paintKey]string{}

// paint fills every physical row, including trailing cells, rather than just text.
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

// renderEntry renders one timeline entry; a positive limit caps the wrapped body rows and 0 hides the entry.
func renderEntry(e entry, width, limit int) string { return renderEntryMode(e, width, limit, true) }

// renderEntryMode is renderEntry; without markdown an assistant entry shows as wrapped plain text.
func renderEntryMode(e entry, width, limit int, markdown bool) string {
	if limit == 0 {
		return "" // hidden by show_lines
	}
	fg, label := eventColors(e.Kind)

	text := ansi.Strip(e.Text)
	if e.Kind == "notice" { // task results and view images; other notices and errors go to the status bar
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
	label += formatMetadata(e.Meta, e.Kind)
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
	if footer := answerFooter(e); footer != "" {
		out += "\n" + paint(ansi.Truncate("  "+footer, max(1, width), "…"), width, lipgloss.NoColor{}, muted)
	}
	return out
}

// answerFooter ends a finished answer: when it arrived and how long the turn took.
func answerFooter(e entry) string {
	if e.Kind != "assistant" || e.Preview || e.Meta == nil || e.Meta.At.IsZero() {
		return ""
	}
	at, now := e.Meta.At.Local(), time.Now()
	clock := at.Format("15:04")
	if at.YearDay() != now.YearDay() || at.Year() != now.Year() {
		clock = at.Format("Jan 2 15:04")
	}
	return clock + " · " + formatDuration(e.Meta.Duration)
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
	rail := lipgloss.NewStyle().Foreground(inputBlue).Render("▌")
	for i, line := range lines {
		lines[i] = ansi.Truncate(rail+paint(" "+line, max(1, width-1), lipgloss.NoColor{}, foreground), max(1, width), "")
	}
	return strings.Join(lines, "\n")
}
func (m model) inputBlock() string {
	if m.menuKind == pickerSetup && len(m.setup) == 2 {
		return renderRail("API key: "+strings.Repeat("*", len([]rune(m.input.Value()))), m.width)
	}
	return renderRail(m.input.View(), m.width)
}

// timeline renders the whole timeline at once; the interface uses timelineRows.
func (m *model) timeline() string { return strings.Join(m.timelineRows(0), "\n") }

// screenRows collects text chunks as screen rows. A chunk continues the last row,
// so cached entries are appended without joining and splitting them again.
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

// timelineRows renders the timeline as screen rows. With a positive budget,
// Markdown of stale assistant entries is rendered newest first until the budget is
// spent; older ones show as plain text and renderPending asks for another pass.
func (m *model) timelineRows(budget time.Duration) []string {
	var out strings.Builder
	out.WriteString(lipgloss.NewStyle().Foreground(cyan).Bold(true).Render("dashy. minimal coding agent."))
	out.WriteString(lipgloss.NewStyle().Foreground(muted).Render(" v" + strings.TrimPrefix(Version, "v")))
	out.WriteString("\n")
	if hint := m.ctl.Settings().SetupHint(); hint != "" {
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
		case "error": // a failed tool call is output in red
			limit = settings.MaxLines("output")
		}
		cached := &m.cache[i]
		if cached.width == m.width && cached.source == e && cached.maxLines == limit && !cached.draft {
			continue
		}
		// The newest Markdown always renders, so every pass makes progress.
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
	m.inputTop = len(screen) - 1 // every row above the composer ends with a newline
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
			if m.menuKind == pickerSettings || m.menuKind == pickerModels {
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
	screen.add("\n\n" + m.bottomBar())
	return screen
}
