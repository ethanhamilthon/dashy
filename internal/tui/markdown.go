package tui

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// terminalLink makes text an OSC 8 hyperlink to target when target is a plain http(s) URL.
func terminalLink(target, text string) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsFunc(target, unicode.IsControl) {
		return text
	}
	return ansi.SetHyperlink(u.String()) + text + ansi.ResetHyperlink()
}

var (
	markedLink        = regexp.MustCompile("(?s)\u2063(.*?)\u2064")
	hyperlinkSequence = regexp.MustCompile("\x1b\\]8;[^\x1b\x07]*(?:\x07|\x1b\\\\)")
)

// Glamour's legacy reflow breaks OSC 8 sequences. Add hyperlinks only after
// rendering, using zero-width markers around URL text from the link formatter.
func markdownLinks(body string) string {
	body = markedLink.ReplaceAllStringFunc(body, func(marked string) string {
		label := strings.TrimSuffix(strings.TrimPrefix(marked, "\u2063"), "\u2064")
		target := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, ansi.Strip(label))
		return terminalLink(target, label)
	})
	return strings.NewReplacer("\u2063", "", "\u2064", "").Replace(body)
}

// Close links on each row so truncation, padding and viewport clipping cannot
// leak a link onto adjacent UI elements. Reopen wrapped links on the next row.
func linkedRows(body string, width int) []string {
	rows := strings.Split(body, "\n")
	active := ""
	for i, row := range rows {
		prefix := active
		for _, seq := range hyperlinkSequence.FindAllString(row, -1) {
			if seq == ansi.ResetHyperlink() {
				active = ""
			} else {
				active = seq
			}
		}
		rows[i] = ansi.Truncate(prefix+row, max(1, width), "…")
		if prefix != "" || strings.Contains(row, "\x1b]8;") {
			rows[i] += ansi.ResetHyperlink()
		}
	}
	return rows
}

var (
	mdKey      any
	mdRenderer *glamour.TermRenderer
	// mdStable caches the rendered stable prefix of the message being streamed.
	mdStable struct {
		key      any
		src, out string
	}
	urlToken = regexp.MustCompile(`(?i)(?:https?://|www\.)[^\s<>]+`)
)

// renderMarkdown renders assistant text with glamour at width. While the text is
// still streaming, only the part after the last completed block is rendered again;
// the blocks before it come from a cache, so a long reply costs linear time.
func renderMarkdown(text string, width int, streaming bool) (string, bool) {
	width = max(1, width)
	if key := [2]any{width, lipgloss.ColorProfile()}; mdKey != key {
		mdKey = key
		mdRenderer, _ = glamour.NewTermRenderer(glamour.WithStylesFromJSONBytes([]byte(markdownStyle)), glamour.WithWordWrap(width), glamour.WithColorProfile(lipgloss.ColorProfile()), glamour.WithInlineTableLinks(true))
	}
	if mdRenderer == nil {
		return "", false
	}
	text = strings.NewReplacer("\u2063", "", "\u2064", "").Replace(text)
	if split := stableSplit(text); streaming && split > 0 {
		if key := [2]any{mdKey, text[:split]}; mdStable.key != key {
			out, ok := renderGlamour(text[:split])
			if !ok {
				return "", false
			}
			mdStable.key, mdStable.out = key, out
		}
		tail, ok := renderGlamour(text[split:])
		if !ok {
			return "", false
		}
		return markdownLinks(mdStable.out + "\n\n" + tail), true
	}
	out, ok := renderGlamour(text)
	return markdownLinks(out), ok
}

func renderGlamour(text string) (string, bool) {
	protected := protectHyphens(text)
	rendered, err := mdRenderer.Render(protected)
	if err != nil {
		return "", false
	}
	if protected != text {
		rendered = strings.ReplaceAll(rendered, "\u2011", "-")
	}
	return strings.TrimSpace(rendered), true
}

// stableSplit returns the end of the last blank line outside a code fence, so
// text[:split] holds complete blocks; 0 when there is none.
func stableSplit(text string) int {
	split, fence, offset := 0, false, 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			fence = !fence
		case trimmed == "" && !fence && strings.HasSuffix(line, "\n") && offset > 0:
			split = offset + len(line)
		}
		offset += len(line)
	}
	if strings.TrimSpace(text[split:]) == "" {
		return 0 // nothing after the split yet: render as one piece
	}
	return split
}

// protectHyphens turns hyphens inside text into non-breaking hyphens. Glamour's
// word wrap does not count a hyphen's width, so lines with hyphens overflow and are
// broken again in the middle of a word. URLs and Markdown syntax keep their hyphens.
func protectHyphens(text string) string {
	if !strings.Contains(text, "-") || strings.Contains(text, "\u2011") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		body := strings.TrimLeft(line, " \t>")
		if strings.Trim(body, "-|:+ \t") == "" {
			continue // thematic break, setext underline or table delimiter row
		}
		head := len(line) - len(body)
		if strings.HasPrefix(body, "- ") {
			head++ // list marker
		}
		var b strings.Builder
		b.WriteString(line[:head])
		rest, last := line[head:], 0
		for _, m := range urlToken.FindAllStringIndex(rest, -1) {
			b.WriteString(strings.ReplaceAll(rest[last:m[0]], "-", "\u2011"))
			b.WriteString(rest[m[0]:m[1]])
			last = m[1]
		}
		b.WriteString(strings.ReplaceAll(rest[last:], "-", "\u2011"))
		lines[i] = b.String()
	}
	return strings.Join(lines, "\n")
}
