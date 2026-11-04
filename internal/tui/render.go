package tui

import (
	"cmp"
	"fmt"
	"strings"

	"dashy/internal/agent"
	"github.com/charmbracelet/glamour"
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
	mdKey      any
	mdRenderer *glamour.TermRenderer
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

// paint fills every physical row, including trailing cells, rather than just text.
func paint(text string, width int, bg, fg lipgloss.TerminalColor) string {
	rows := strings.Split(text, "\n")
	style := lipgloss.NewStyle().Width(width).Background(bg).Foreground(fg)
	for i, row := range rows {
		rows[i] = style.Render(ansi.Truncate(row, max(1, width), ""))
	}
	return strings.Join(rows, "\n")
}

// renderEntry renders one timeline entry; a positive limit caps the wrapped body rows and 0 hides the entry.
func renderEntry(e entry, width, limit int) string {
	if limit == 0 {
		return "" // hidden by show_lines
	}
	fg, label := eventColors(e.Kind)

	text := ansi.Strip(e.Text)
	if e.Kind == "notice" { // <notify> messages; other notices and errors go to the status bar
		text = strings.NewReplacer("<notify>", "", "</notify>", "").Replace(text)
		text = cmp.Or(strings.Join(strings.Fields(text), " "), "Notification received.")
		return paint(ansi.Truncate("  "+label+" · "+text, max(1, width), "…"), max(1, width), lipgloss.NoColor{}, fg)
	}

	if e.Kind == "user" {
		return renderRail(ansi.Wrap(text, max(1, width-2), ""), width)
	}
	body := ansi.Wrap(text, max(1, width-4), "")
	if e.Kind == "assistant" {
		if key := [2]any{width, lipgloss.ColorProfile()}; mdKey != key {
			mdKey = key
			mdRenderer, _ = glamour.NewTermRenderer(glamour.WithStylesFromJSONBytes([]byte(markdownStyle)), glamour.WithWordWrap(max(1, width-4)), glamour.WithColorProfile(lipgloss.ColorProfile()), glamour.WithInlineTableLinks(true))
		}
		if mdRenderer != nil {
			if rendered, err := mdRenderer.Render(strings.NewReplacer("\u2063", "", "\u2064", "").Replace(text)); err == nil {
				body = markdownLinks(strings.TrimSpace(rendered))
			}
		}
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
	return paint(ansi.Truncate("  "+label, max(1, width), "…"), width, lipgloss.NoColor{}, fg) + "\n" + paint(strings.Join(lines, "\n"), width, lipgloss.NoColor{}, bodyColor(e.Kind))
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

func (m *model) timeline() string {
	var out strings.Builder
	out.WriteString(lipgloss.NewStyle().Foreground(cyan).Bold(true).Render("dashy. minimal coding agent."))
	out.WriteString(lipgloss.NewStyle().Foreground(muted).Render(" v" + strings.TrimPrefix(Version, "v")))
	out.WriteString("\n" + lipgloss.NewStyle().Foreground(muted).Render("Enter to send · Shift+Enter for newline · / for commands") + "\n")
	if hint := m.ctl.Settings().SetupHint(); hint != "" {
		out.WriteString(lipgloss.NewStyle().Foreground(warnColor).Render(hint) + "\n")
	}
	for _, path := range m.ctl.Instructions() {
		row := "DASHY.md " + shortPath(path)
		out.WriteString(lipgloss.NewStyle().Foreground(muted).Render(ansi.Truncate(row, max(1, m.width), "…")) + "\n")
	}
	out.WriteString("\n")
	if len(m.cache) > len(m.entries) {
		m.cache = m.cache[:len(m.entries)]
	}
	settings := m.ctl.Settings()
	for i, e := range m.entries {
		limit := -1
		switch e.Kind {
		case "code", "output", "reasoning":
			limit = settings.MaxLines(e.Kind)
		case "error": // a failed tool call is output in red
			limit = settings.MaxLines("output")
		}
		if i >= len(m.cache) {
			m.cache = append(m.cache, renderedEntry{})
		}
		cached := &m.cache[i]
		if cached.width != m.width || cached.source != e || cached.maxLines != limit {
			cached.source = e
			cached.width = m.width
			cached.maxLines = limit
			cached.Text = renderEntry(e, m.width, limit)
		}
		if cached.Text != "" {
			out.WriteString(cached.Text)
			out.WriteString("\n\n")
		}
	}
	out.WriteString(m.workingBlock())
	m.inputTop = strings.Count(out.String(), "\n") // every row above the composer ends with a newline
	out.WriteString(m.inputBlock())
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
		out.WriteString("\n" + paint(ansi.Truncate("  Effort: "+strings.Join(parts, " ")+"  (Left/Right)", m.width, "…"), m.width, lipgloss.NoColor{}, foreground))
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
		out.WriteByte('\n')
		out.WriteString(paint(ansi.Truncate(row, m.width, "…"), m.width, bg, fg))
	}
	out.WriteString("\n\n" + m.bottomBar())
	return out.String()
}
