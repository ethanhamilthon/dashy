package tui

import (
	"fmt"
	"path/filepath"
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

func eventColors(kind string) (bg, fg lipgloss.TerminalColor, label string) {
	switch kind {
	case "user":
		return lipgloss.NoColor{}, foreground, ""
	case "assistant":
		return lipgloss.NoColor{}, assistantColor, "*"
	case "code":
		return lipgloss.NoColor{}, codeGray, ">_"
	case "reasoning":
		return lipgloss.NoColor{}, darkGray, "~"
	case "output":
		return lipgloss.NoColor{}, darkGray, "<"
	case "error":
		return lipgloss.NoColor{}, errorColor, "error"
	case "notice":
		return lipgloss.NoColor{}, muted, "!"
	default:
		return lipgloss.NoColor{}, infoColor, "info"
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

// An optional limit caps wrapped body rows; zero hides the entire entry.
func renderEntry(e entry, width int, limits ...int) string {
	limit := -1
	if len(limits) > 0 {
		limit = limits[0]
	}
	if limit == 0 && e.Kind != "error" {
		return ""
	}
	bg, fg, label := eventColors(e.Kind)

	text := ansi.Strip(e.Text)
	if e.Kind == "notice" || e.Kind == "error" {
		text = strings.TrimSpace(text)
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "<notify>"), "</notify>"))
		if e.Kind == "notice" && strings.HasPrefix(text, "Shell session restarted.") {
			text = "Shell session restarted. Previous variables were cleared."
		}
		if e.Kind == "notice" && strings.HasPrefix(text, "Shell session initialized.") {
			text = "Shell session initialized."
		}
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			text = "Notification received."
			if e.Kind == "error" {
				text = "An error occurred."
			}
		}
		if e.Kind == "error" {
			icon := lipgloss.NewStyle().Foreground(errorColor).Render("!")
			message := lipgloss.NewStyle().Foreground(darkGray).Render(" · " + text)
			return paint(ansi.Truncate("  "+icon+message, max(1, width), "…"), max(1, width), lipgloss.NoColor{}, lipgloss.NoColor{})
		}
		return paint(ansi.Truncate("  "+label+" · "+text, max(1, width), "…"), max(1, width), bg, fg)

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
		return paint(ansi.Truncate("  "+label, max(1, width), "…"), width, bg, fg)
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
	return paint(ansi.Truncate("  "+label, max(1, width), "…"), width, bg, fg) + "\n" + paint(strings.Join(lines, "\n"), width, bg, bodyColor(e.Kind))
}
func bodyColor(kind string) lipgloss.TerminalColor {
	switch kind {
	case "code":
		return codeGray
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
	if m.menuKind == "setup" && len(m.setup) == 2 {
		return renderRail("API key: "+strings.Repeat("*", len([]rune(m.input.Value()))), m.width)
	}
	return renderRail(m.input.View(), m.width)
}

// setupHint explains what is missing before the first message can be sent.
func (m model) setupHint() string {
	s := m.ctl.Settings()
	if len(s.Providers) == 0 {
		return "No provider configured. Use /login to add a provider, then choose a model with /settings."
	}
	return "No model selected. Choose a model with /settings (use /login to add more providers)."
}

func (m *model) timeline() string {
	var out strings.Builder
	out.WriteString(lipgloss.NewStyle().Foreground(cyan).Bold(true).Render("dashy. minimal coding agent."))
	out.WriteString(lipgloss.NewStyle().Foreground(muted).Render(" v" + strings.TrimPrefix(Version, "v")))
	out.WriteString("\n" + lipgloss.NewStyle().Foreground(muted).Render("Enter to send · Shift+Enter for newline · / for commands") + "\n")
	if s := m.ctl.Settings(); s.Provider == "" || s.Model == "" {
		out.WriteString(lipgloss.NewStyle().Foreground(warnColor).Render(m.setupHint()) + "\n")
	}
	for _, s := range m.ctl.Sources() {
		where := s.Origin
		if s.Path != "" {
			where = shortPath(s.Path) + " (" + s.Origin + ")"
		}
		row := fmt.Sprintf("%-7s %s", strings.TrimSuffix(s.Name, filepath.Ext(s.Name)), where)
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
		case "code":
			limit = settings.MaxCodeLines()
		case "output":
			limit = settings.MaxResultLines()
		case "reasoning":
			limit = settings.MaxThinkingLines()
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
	for i, image := range m.images {
		label := fmt.Sprintf("  [Image %d: %s] Ctrl+X removes last", i+1, filepath.Base(image.Path))
		out.WriteString(paint(ansi.Truncate(ansi.Strip(label), m.width, "…"), m.width, lipgloss.NoColor{}, muted) + "\n")
	}
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
	if !fileMode && (m.menuKind == "model" || m.menuKind == "settings") {
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
			if m.menuKind == "model" || m.menuKind == "settings" {
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
	out.WriteString("\n\n" + lipgloss.NewStyle().Foreground(muted).Render(m.bottomBar()))
	return out.String()
}
